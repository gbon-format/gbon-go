package gbon_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// FuzzRTStream is the multi-message
// composition harness — one Encoder, one Decoder, a stream of any-position
// payloads drawn from the {map + registered structs} class space that the
// single-value rtGen table of FuzzRT cannot express, with allocation
// churn between messages so the identity-intern liveness axis (stream
// interning must stay valid across dead objects and allocator address
// reuse) is exercised, not just the happy live-graph path.

// stStreamW and stStreamG are the harness's registered leaf types: the
// same shape pair as the field repro (struct with flat fields; struct
// with a map[string]int64 field).
type stStreamW struct {
	ID   int64
	Name string
}

type stStreamG struct {
	Code   string
	Params map[string]int64
}

// stStreamMsg is the stream envelope: a monotone sequence number the
// comparator pins per message plus an any-position payload.
type stStreamMsg struct {
	Seq     int64
	Payload any
}

// stStreamPlan derives the stream shape from the seed: message count
// and whether the churn phase forces GC. Arithmetic only — the rand
// stream stays entirely inside stStreamMsg so the encode pass and the
// want pass consume identical randomness.
func stStreamPlan(seed int64) (n int, forced bool) {
	return 2 + int(uint64(seed)%39), seed%2 == 0
}

// stStreamBuild builds message i deterministically from r over the
// closed class table (see stStreamMsg); the >64 KiB string class makes
// the window pull, compact, and re-arm across a large record body.
func stStreamBuild(r *rand.Rand, i int) stStreamMsg {
	var p any
	switch r.Intn(7) {
	case 0:
		p = map[string]any{"k": "v", "j": int64(i)}
	case 1:
		p = map[string]any{"k": int64(i), "j": "w"}
	case 2:
		p = stStreamW{ID: int64(i), Name: "w"}
	case 3:
		p = stStreamG{Code: "g", Params: map[string]int64{"a": int64(i)}}
	case 4:
		p = []any{"l", int64(i), 3.5}
	case 5:
		// payload past the fill chunk: the sliding window pulls,
		// compacts, and re-arms across a >64 KiB record body
		p = strings.Repeat("x", 1<<16+i%2048)
	default:
		p = map[string]int{"m": i}
	}
	return stStreamMsg{Seq: int64(i), Payload: p}
}

// stStreamChurn is the between-messages garbage phase: short-lived maps
// of both interned shapes become dead allocations, and the forced mode
// collects them immediately so the next message's map allocations draw
// from freshly recycled addresses — the identity-intern liveness axis.
func stStreamChurn(gc bool) {
	for j := range 8 {
		_ = map[string]any{"churn": j}
		_ = map[string]int64{"churn": int64(j)}
	}
	if gc {
		runtime.GC()
	}
}

// stStreamDesc renders one message for failure reports: the envelope
// summary and a bounded payload description (type, length, Seq) — raw
// payload formatting never enters a report (the safeDesc canon of
// codec_fuzz_test.go:467, applied to the stream envelope).
func stStreamDesc(i int, m stStreamMsg, got bool) string {
	side := "want"
	if got {
		side = "got"
	}
	pl := m.Payload
	if pl == nil {
		return fmt.Sprintf("%s[%d] Seq=%d Payload=nil", side, i, m.Seq)
	}
	return fmt.Sprintf("%s[%d] Seq=%d Payload=%T(%d)", side, i, m.Seq, pl, stStreamLen(pl))
}

func stStreamLen(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		return len(x)
	case map[string]any:
		return len(x)
	case map[string]int:
		return len(x)
	case map[string]int64:
		return len(x)
	}
	return 0
}

// stStreamCheck is the shared oracle of the harness target and the
// deterministic replay: encode the generated stream on one Encoder,
// decode it message-by-message on one registered Decoder, compare each
// message, then re-encode the decoded values on a fresh Encoder and
// require byte identity with the original stream (the stream-level
// form of FuzzRT's re-marshal oracle: one value sequence has exactly
// one legal byte sequence).
func stStreamCheck(t testing.TB, data []byte) {
	seed := int64(len(data))
	for _, b := range data {
		seed = seed<<8 ^ int64(b)
	}
	n, forced := stStreamPlan(seed)

	// Encode pass: each message is dropped right after its Encode call,
	// so its payload map is garbage before the next message allocates —
	// the encoder's stream intern table must survive dead objects and
	// allocator address reuse without claiming a stale identity.
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	{
		r := rand.New(rand.NewSource(seed))
		for i := range n {
			m := stStreamBuild(r, i)
			if err := enc.Encode(m); err != nil {
				t.Fatalf("encode %s: %v", stStreamDesc(i, m, false), err)
			}
			stStreamChurn(forced)
		}
	}
	wire := append([]byte(nil), buf.Bytes()...)

	// Want pass: regenerate the identical message sequence (fresh rand,
	// same seed, same call order).
	var msgs []stStreamMsg
	{
		r := rand.New(rand.NewSource(seed))
		for i := range n {
			msgs = append(msgs, stStreamBuild(r, i))
		}
	}

	dec := gbon.NewDecoder(&stStreamChunkReader{b: wire})
	if err := dec.Register(stStreamW{}, stStreamG{}, map[string]any{},
		map[string]int{}, []any{}, "", int64(0), 3.5); err != nil {
		t.Fatalf("register: %v", err)
	}
	var decoded []stStreamMsg
	for i := range msgs {
		var v stStreamMsg
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode %s: %v", stStreamDesc(i, msgs[i], false), err)
		}
		decoded = append(decoded, v)
	}
	for i := range msgs {
		if decoded[i].Seq != msgs[i].Seq {
			t.Fatalf("message %d: Seq drift %d vs %d", i, decoded[i].Seq, msgs[i].Seq)
		}
		if !stStreamEq(decoded[i].Payload, msgs[i].Payload) {
			t.Fatalf("message %d: payload drift\n%s\n%s", i,
				stStreamDesc(i, decoded[i], true), stStreamDesc(i, msgs[i], false))
		}
	}

	var re bytes.Buffer
	reEnc := gbon.NewEncoder(&re)
	for _, m := range decoded {
		if err := reEnc.Encode(m); err != nil {
			t.Fatalf("re-encode %s: %v", stStreamDesc(int(m.Seq), m, true), err)
		}
	}
	if !bytes.Equal(wire, re.Bytes()) {
		t.Fatalf("re-encoded stream diverged: original %s vs re-encoded %s",
			streamAnchor(wire), streamAnchor(re.Bytes()))
	}
}

// stStreamEq is the stream comparator: structural equality per payload
// class. The harness class table carries no cycles, so class-structural
// comparison is total.
func stStreamEq(got, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !stStreamEq(gv, wv) {
				return false
			}
		}
		return true
	case map[string]int:
		g, ok := got.(map[string]int)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, wv := range w {
			if g[k] != wv {
				return false
			}
		}
		return true
	case stStreamW:
		g, ok := got.(stStreamW)
		return ok && g == w
	case stStreamG:
		g, ok := got.(stStreamG)
		if !ok || g.Code != w.Code || len(g.Params) != len(w.Params) {
			return false
		}
		for k, wv := range w.Params {
			if g.Params[k] != wv {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !stStreamEq(g[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return got == want
	}
}

// stStreamChunkReader serves the wire in bounded chunks (fill chunk
// size): the sliding window exercises its pull/compact cycle instead
// of one bulk Read.
type stStreamChunkReader struct {
	b []byte
}

func (r *stStreamChunkReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := min(copy(p, r.b), 1<<16)
	r.b = r.b[n:]
	return n, nil
}

// streamAnchor pins a stream a failure report refers to: total length
// plus a bounded hex prefix (the wireAnchor canon, stream form).
func streamAnchor(b []byte) string {
	n := min(len(b), 32)
	return fmt.Sprintf("%dB:%x", len(b), b[:n])
}

// FuzzRTStream: the multi-message composition target. Committed seeds
// are this harness's own fresh generation — they carry no corpus bytes
// of earlier harnesses.
func FuzzRTStream(f *testing.F) {
	f.Add([]byte{7})
	f.Add([]byte{0x5a, 0xa5})
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Fuzz(func(t *testing.T, data []byte) {
		stStreamCheck(t, data)
	})
}

// TestFuzzRTStreamReplay: deterministic replay of the committed seed
// corpus through the same oracle as the fuzz target — the regression
// carrier runs in every plain `go test` pass.
func TestFuzzRTStreamReplay(t *testing.T) {
	root := filepath.Join("testdata", "fuzz", "FuzzRTStream")
	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read seed dir: %v", err)
	}
	for _, e := range files {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("read seed %s: %v", e.Name(), err)
		}
		data, ok := parseFuzzSeed(string(raw))
		if !ok {
			t.Fatalf("seed %s: malformed corpus line", e.Name())
		}
		t.Run(e.Name(), func(t *testing.T) {
			stStreamCheck(t, data)
		})
	}
}

// parseFuzzSeed reads the Go fuzz corpus v1 file of a []byte argument:
// a `go test fuzz v1` header line followed by one []byte("…") literal.
func parseFuzzSeed(raw string) ([]byte, bool) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != "go test fuzz v1" {
		return nil, false
	}
	rest := strings.TrimSpace(lines[1])
	const prefix = "[]byte("
	if !strings.HasPrefix(rest, prefix) || !strings.HasSuffix(rest, ")") {
		return nil, false
	}
	s, err := strconv.Unquote(rest[len(prefix) : len(rest)-1])
	if err != nil {
		return nil, false
	}
	return []byte(s), true
}
