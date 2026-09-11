package gbon_test

// D1 regression: positional restoration of a peeked REF token on the
// sliding window. The historical defect: PeekRef pinned an absolute
// position, a mid-token fill compacted the window (base shift) between
// the pin and the restore, and the restored relative position went
// negative — the next direct buffer read panicked. The repro shape is a
// stream-decoded pointer graph (ptr→iface sharing) whose REF tokens sit
// past the largeRead compaction boundary; the differential oracle is
// the buffered-mode decode of the same bytes.

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// d1Node is a shared ring: Self closes the cycle, so every hop after
// the first is a REF token.
type d1Node struct {
	Label string
	Self  *d1Node
}

// d1Payload crosses the window boundary: Pad of 9-byte int64 tokens
// pushes past largeRead before the ring REFs decode (both are peeked
// lookaheads in the pointer and interface paths).
type d1Payload struct {
	Pad   []int64
	Iface any
	Ring  *d1Node
}

// d1PadElem is wider than the u32 ARG form, so every Pad element
// travels as a 9-byte token.
func d1PadElem(i int) int64 { return 1<<40 + int64(i) }

// TestDecodePeekRefPastWindowBoundary decodes a stream whose REF tokens
// sit past the largeRead compaction boundary, at pad sizes sweeping the
// boundary phase: values round-trip, sharing survives, no panic.
func TestDecodePeekRefPastWindowBoundary(t *testing.T) {
	const base = 131072 // 9B tokens ≈ 1.18MB of Pad
	for _, delta := range []int{-2, -1, 0, 1, 2} {
		n := base + delta
		t.Run(string(rune('a'+delta+2)), func(t *testing.T) {
			ring := &d1Node{Label: "ring"}
			ring.Self = ring
			in := d1Payload{Pad: make([]int64, n), Iface: ring, Ring: ring}
			for i := range in.Pad {
				in.Pad[i] = d1PadElem(i)
			}
			wire, err := gbon.Marshal(in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if len(wire) < 1<<20 {
				t.Fatalf("stream too short to cross the window boundary: %d", len(wire))
			}

			var stream d1Payload
			dec := gbon.NewDecoder(&d1ChunkReader{b: wire})
			if err := dec.Register(d1Node{}, d1Payload{}, new(d1Node)); err != nil {
				t.Fatalf("register: %v", err)
			}
			if err := dec.Decode(&stream); err != nil {
				t.Fatalf("stream decode: %v", err)
			}
			if len(stream.Pad) != n {
				t.Fatalf("Pad length %d, want %d", len(stream.Pad), n)
			}
			for i := range stream.Pad {
				if stream.Pad[i] != d1PadElem(i) {
					t.Fatalf("Pad[%d] = %d", i, stream.Pad[i])
				}
			}
			if stream.Ring == nil || stream.Ring.Self != stream.Ring || stream.Ring.Label != "ring" {
				t.Fatalf("ring cycle broken by the peek path")
			}
			if node, ok := stream.Iface.(*d1Node); !ok || node != stream.Ring {
				t.Fatalf("interface slot lost the shared ring identity")
			}
			if re, err := gbon.Marshal(stream); err != nil || !bytes.Equal(re, wire) {
				t.Fatalf("re-encode diverged (err %v)", err)
			}

			// PB-1 differential oracle on basic-typed bytes crossing
			// the same boundary: stream mode vs buffer mode agree.
			basic := d1Basic{Pad: in.Pad, M: map[string]int64{"k": 1}}
			bwire, err := gbon.Marshal(basic)
			if err != nil {
				t.Fatalf("Marshal basic: %v", err)
			}
			var s2, b2 d1Basic
			sdec := gbon.NewDecoder(&d1ChunkReader{b: bwire})
			if err := sdec.Decode(&s2); err != nil {
				t.Fatalf("stream basic: %v", err)
			}
			if err := gbon.Unmarshal(bwire, &b2); err != nil {
				t.Fatalf("buffered basic: %v", err)
			}
			if len(s2.Pad) != len(b2.Pad) || s2.M["k"] != b2.M["k"] || len(s2.Pad) != n {
				t.Fatalf("stream vs buffered drift")
			}
		})
	}
}

// d1Basic is the PB-1 differential carrier: plain types only, so the
// stateless buffer-mode Unmarshal decodes the same bytes the chunked
// stream decoder consumes.
type d1Basic struct {
	Pad []int64
	M   map[string]int64
}

// d1ChunkReader serves the wire in bounded chunks so the sliding window
// pulls and compacts as it would over a real stream source.
type d1ChunkReader struct {
	b []byte
}

func (r *d1ChunkReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := min(copy(p, r.b), 1<<16)
	r.b = r.b[n:]
	return n, nil
}

// d1LiveReader serves the wire once and then blocks forever without
// reporting io.EOF — a live socket whose peer finished sending but
// keeps the connection open (no half-close).
type d1LiveReader struct {
	b    []byte
	hold chan struct{}
}

func (r *d1LiveReader) Read(p []byte) (int, error) {
	if len(r.b) > 0 {
		n := copy(p, r.b)
		r.b = r.b[n:]
		return n, nil
	}
	<-r.hold // never closed before the test ends
	return 0, io.EOF
}

// TestDecodePeekRefTailOnLiveSource: the lookahead fills exactly the
// token extent — an over-read blocks forever on a live non-EOF source
// whose last delivered bytes are a REF token (hang-class regression).
func TestDecodePeekRefTailOnLiveSource(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	shared := &d1Node{Label: "shared"}
	for _, v := range []*d1Node{{Label: "first", Self: shared}, {Label: "tail", Self: shared}} {
		if err := enc.Encode(v); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	src := &d1LiveReader{b: buf.Bytes(), hold: make(chan struct{})}
	defer close(src.hold)

	type result struct {
		label string
		self  string
		err   error
	}
	done := make(chan result, 2)
	dec := gbon.NewDecoder(src)
	go func() {
		for range 2 {
			var got d1Node
			if err := dec.Decode(&got); err != nil {
				done <- result{err: err}
				return
			}
			done <- result{label: got.Label, self: got.Self.Label}
		}
	}()
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("Decode: %v", r.err)
			}
			if r.self != "shared" {
				t.Fatalf("%q: Self.Label = %q, want the interned shared node", r.label, r.self)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Decode blocked on a live source: peek lookahead over-reads past the token")
		}
	}
}
