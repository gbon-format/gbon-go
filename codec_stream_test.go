package gbon_test

// Streaming decode: record-boundary reading without materializing the
// whole stream, and multi-value streams over io.Reader sources.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// TestStreamingMultiValueDecode reads five values of mixed shapes from one
// stream sequentially, then observes io.EOF.
func TestStreamingMultiValueDecode(t *testing.T) {
	want := []any{
		int64(-7),
		"stream value",
		[]byte("record three"),
		4.5,
		complex128(3 + 4i),
	}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	for _, v := range want {
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	for i := range want {
		var got any
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
		if !valuesEqualGeneric(got, want[i]) {
			t.Fatalf("value %d drift: %#v vs %#v", i, got, want[i])
		}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		t.Fatalf("end of stream: got %v, want io.EOF", err)
	}
}

// TestStreamingLargePayload decodes a ~100MB stream of blob records from a
// file source and asserts the decoding live set stays under 2x the wire
// size: records materialize one at a time over the sliding window.
func TestStreamingLargePayload(t *testing.T) {
	if raceEnabled {
		t.Skip("100MB streaming soak is a non-race gate leg")
	}
	const records = 25
	const recLen = 4 << 20 // 4MiB payloads -> ~100MiB wire
	dir := t.TempDir()
	path := filepath.Join(dir, "stream.bin")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := gbon.NewEncoder(f)
	for i := range records {
		payload := make([]byte, recLen) // distinct backing per record: one
		// shared window would collapse all records into views of the first
		binary.BigEndian.PutUint64(payload, uint64(i))
		payload[recLen-1] = byte(i + 1) // keep the dense prefix nonzero
		if err := enc.Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wireSize := st.Size()

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rf.Close() }()
	dec := gbon.NewDecoder(rf)
	peak := measureLiveHeap()
	for i := range records {
		var got []byte
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if len(got) != recLen || binary.BigEndian.Uint64(got) != uint64(i) {
			t.Fatalf("record %d drift: len %d", i, len(got))
		}
		if h := measureLiveHeap(); h > peak {
			peak = h
		}
	}
	if err := dec.Decode(new([]byte)); err != io.EOF {
		t.Fatalf("end of stream: got %v, want io.EOF", err)
	}
	if limit := 2 * uint64(wireSize); peak > limit {
		t.Fatalf("streaming live heap %d exceeds 2x wire size %d", peak, limit)
	}
	t.Logf("wire %d bytes, peak live heap %d bytes (%.2fx)", wireSize, peak, float64(peak)/float64(wireSize))
}

// measureLiveHeap samples the heap after a forced collection: an
// approximation of the process live set at a record boundary.
func measureLiveHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func valuesEqualGeneric(a, b any) bool {
	bb, ok := b.([]byte)
	if ok {
		ab, ok := a.([]byte)
		return ok && bytes.Equal(ab, bb)
	}
	switch b := b.(type) {
	case map[string]int64:
		am, ok := a.(map[string]int64)
		if !ok || len(am) != len(b) {
			return false
		}
		for k, v := range b {
			if am[k] != v {
				return false
			}
		}
		return true
	}
	return any(a) == any(b)
}

// TestDecodeHeaderOnlyTruncated pins the empty-stream contract: the
// header is written with the first value, so input ending right after
// the header is a truncated stream — a typed ErrFormat on the first
// Decode, sticky afterwards, never a raw io.EOF (doc.go: an arbitrary
// input yields a sentinel error). The header-only prefix is derived
// from a real encode, not hand-crafted.
func TestDecodeHeaderOnlyTruncated(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(int64(42)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	full := buf.Bytes()
	hdrLen := headerLen(t, full)
	cut := full[:hdrLen]

	marker := int64(777)
	out := marker
	dec := gbon.NewDecoder(bytes.NewReader(cut))
	err := dec.Decode(&out)

	var ge *gbon.Error
	if !errors.As(err, &ge) {
		t.Fatalf("first Decode: want *gbon.Error, got %T: %v", err, err)
	}
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("first Decode: want ErrFormat, got %v", err)
	}
	if ge.Class() != "truncated" {
		t.Fatalf("class: want truncated, got %q", ge.Class())
	}
	if ge.Offset != hdrLen {
		t.Fatalf("offset: want %d, got %d", hdrLen, ge.Offset)
	}
	if out != marker {
		t.Fatalf("target mutated on error: %d", out)
	}

	// Sticky: a repeat Decode returns the same error, not io.EOF.
	again := dec.Decode(&out)
	if !errors.Is(again, gbon.ErrFormat) || again.Error() != err.Error() {
		t.Fatalf("repeat Decode: want same sticky error, got %v", again)
	}
}

// TestDecodeHeaderOnlyAdjacentPins pins the neighbours: shorter cuts
// stay truncated at offset 0, and a stream with at least one value
// still ends in a clean io.EOF (repeatable).
func TestDecodeHeaderOnlyAdjacentPins(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(int64(1)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := enc.Encode(int64(2)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	full := buf.Bytes()
	hdrLen := headerLen(t, full)

	for _, n := range []int{0, 2, hdrLen - 1} {
		var out int64
		err := gbon.NewDecoder(bytes.NewReader(full[:n])).Decode(&out)
		var ge *gbon.Error
		if !errors.As(err, &ge) || ge.Class() != "truncated" || ge.Offset != 0 {
			t.Fatalf("cut %d: want truncated@0, got %v", n, err)
		}
	}

	// Clean end after >= 1 value: two values then EOF, repeat EOF.
	var v int64
	dec := gbon.NewDecoder(bytes.NewReader(full))
	if err := dec.Decode(&v); err != nil || v != 1 {
		t.Fatalf("first value: %d %v", v, err)
	}
	if err := dec.Decode(&v); err != nil || v != 2 {
		t.Fatalf("second value: %d %v", v, err)
	}
	if err := dec.Decode(&v); err != io.EOF {
		t.Fatalf("end of stream: want io.EOF, got %v", err)
	}
	if err := dec.Decode(&v); err != io.EOF {
		t.Fatalf("repeat end: want io.EOF, got %v", err)
	}
}

// headerLen derives the stream header size from a real encoded stream
// through the stateless path: for prefixes shorter than the header,
// Unmarshal reports offset 0 (mid-header truncation); at and after the
// header boundary the reported offset equals the prefix length. The
// derivation never touches the streaming branch under test.
func headerLen(t *testing.T, stream []byte) int {
	t.Helper()
	for n := 1; n <= len(stream); n++ {
		var probe int64
		err := gbon.Unmarshal(stream[:n], &probe)
		var ge *gbon.Error
		if errors.As(err, &ge) && ge.Offset == n {
			return n
		}
	}
	t.Fatal("no header boundary found")
	return 0
}

// TestDecodeHeaderOnlyMisusePriority pins the misuse priority: an
// invalid target is reported before the stream state is consulted and
// leaves the Decoder usable — on a header-only stream the next
// well-formed call still yields the truncated error of the stream
// itself, never a sticky misuse verdict (branch-order regression guard
// for the AtEOF path).
func TestDecodeHeaderOnlyMisusePriority(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(int64(42)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	full := buf.Bytes()
	cut := full[:headerLen(t, full)]

	dec := gbon.NewDecoder(bytes.NewReader(cut))
	err := dec.Decode(nil)
	if !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("misuse: want ErrUnsupported, got %v", err)
	}

	var out int64
	err = dec.Decode(&out)
	var ge *gbon.Error
	if !errors.As(err, &ge) || ge.Class() != "truncated" {
		t.Fatalf("after misuse: want stream truncated error, got %v", err)
	}
}
