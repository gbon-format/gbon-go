package wire

import (
	"hash/maphash"
	"math/rand"
	"strconv"
	"testing"
)

// internWrite writes s and returns the stream offset of the position
// token; the decision is read off w.buf[off], the token decoded from
// w.buf[off:].
func internWrite(t *testing.T, w *Writer, s string) int {
	t.Helper()
	pre := len(w.buf)
	if err := w.WriteString(s); err != nil {
		t.Fatalf("WriteString(%q): %v", s, err)
	}
	return pre
}

// internEdgeStr returns a short byte string over a tiny alphabet:
// the repeat-prone len 0..4 edge zone.
func internEdgeStr(rng *rand.Rand) string {
	b := make([]byte, rng.Intn(5))
	for i := range b {
		b[i] = byte('a' + rng.Intn(4))
	}
	return string(b)
}

// internRandStr returns a fresh random byte string: any bytes, no
// UTF-8 gate.
func internRandStr(rng *rand.Rand) string {
	b := make([]byte, rng.Intn(12))
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return string(b)
}

// TestInternTableLenEdges pins first-encounter literal and repeat REF
// decisions for the short-string edge lengths, repeats included.
func TestInternTableLenEdges(t *testing.T) {
	edges := []string{"", "a", "ab", "abc", "abcd", "\xff\x00\xfe", "z"}
	oracle := make(map[string]uint64)
	w := NewWriter()
	next := uint64(0)
	for rep := 1; rep <= 3; rep++ {
		for _, s := range edges {
			pre := internWrite(t, w, s)
			id, hit := oracle[s]
			if hit {
				if class := w.buf[pre] >> 4; class != classRef {
					t.Fatalf("rep %d %q: class %#x, want REF", rep, s, class)
				}
				got, err := NewReader(w.buf[pre:]).readTokenArg(classRef)
				if err != nil || got != id {
					t.Fatalf("rep %d %q: ref id %d, %v, want %d", rep, s, got, err, id)
				}
				continue
			}
			if class := w.buf[pre] >> 4; class != classString {
				t.Fatalf("rep %d %q: class %#x, want literal", rep, s, class)
			}
			oracle[s] = next
			next++
		}
	}
	if w.strs.count != len(oracle) {
		t.Fatalf("table count %d, oracle %d", w.strs.count, len(oracle))
	}
	for s, id := range oracle {
		if got, ok := w.strs.get(s); !ok || got != id {
			t.Fatalf("get(%q) = %d,%v, want %d", s, got, ok, id)
		}
	}
}

// TestInternTableGrowthThresholds drives unique-literal counts across
// the power-of-two growth thresholds: ids stay sequential and the slot
// array stays a power of two.
func TestInternTableGrowthThresholds(t *testing.T) {
	for _, n := range []int{5, 6, 7, 8, 13, 25, 49, 97, 200, 320} {
		w := NewWriter()
		oracle := make(map[string]uint64)
		for i := range n {
			s := "k" + strconv.Itoa(i)
			pre := internWrite(t, w, s)
			if class := w.buf[pre] >> 4; class != classString {
				t.Fatalf("n=%d i=%d: class %#x, want literal", n, i, class)
			}
			oracle[s] = uint64(i)
		}
		if w.strs.count != n {
			t.Fatalf("n=%d: count %d", n, w.strs.count)
		}
		slots := len(w.strs.slots)
		if slots&(slots-1) != 0 || slots < 8 {
			t.Fatalf("n=%d: slot count %d not a power-of-two capacity", n, slots)
		}
		for i := range n {
			s := "k" + strconv.Itoa(i)
			if got, ok := w.strs.get(s); !ok || got != uint64(i) {
				t.Fatalf("n=%d get(%q) = %d,%v, want %d", n, s, got, ok, i)
			}
		}
		// The whole stream reads back in write order: REFs resolve.
		r := NewReader(w.buf)
		for i := range n {
			got, err := r.ReadString()
			if err != nil || got != "k"+strconv.Itoa(i) {
				t.Fatalf("n=%d read %d: %q, %v", n, i, got, err)
			}
		}
	}
}

// TestInternTableLiteralRebind pins the map-assignment semantics of a
// literal re-registration: the newest id wins, no new entry appears.
func TestInternTableLiteralRebind(t *testing.T) {
	w := NewWriter()
	if err := w.WriteString("dup"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteStringLit("dup"); err != nil {
		t.Fatal(err)
	}
	if w.strs.count != 1 {
		t.Fatalf("count %d after rebind, want 1", w.strs.count)
	}
	id, ok := w.strs.get("dup")
	if !ok || id != 1 {
		t.Fatalf("get(dup) = %d,%v, want id 1 (newest wins)", id, ok)
	}
	pre := len(w.buf)
	if err := w.WriteString("dup"); err != nil {
		t.Fatal(err)
	}
	if class := w.buf[pre] >> 4; class != classRef {
		t.Fatalf("class %#x after rebind, want REF", class)
	}
}

// TestInternTableOracleProperty compares every decision and id
// against a map oracle on a generated stream, with full-state sweeps
// along the way.
func TestInternTableOracleProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20260912))
	oracle := make(map[string]uint64)
	w := NewWriter()
	next := uint64(0)
	seq := make([]string, 0, 4000)
	for step := range 4000 {
		var s string
		switch rng.Intn(4) {
		case 0:
			s = internEdgeStr(rng)
		case 1:
			s = "pool" + strconv.Itoa(rng.Intn(40))
		default:
			s = internRandStr(rng)
		}
		seq = append(seq, s)
		pre := internWrite(t, w, s)
		if id, hit := oracle[s]; hit {
			if class := w.buf[pre] >> 4; class != classRef {
				t.Fatalf("step %d %q: class %#x, oracle says REF", step, s, class)
			}
			if got, ok := w.strs.get(s); !ok || got != id {
				t.Fatalf("step %d %q: id %d,%v, oracle %d", step, s, got, ok, id)
			}
		} else {
			if class := w.buf[pre] >> 4; class != classString {
				t.Fatalf("step %d %q: class %#x, oracle says literal", step, s, class)
			}
			oracle[s] = next
			next++
		}
		if step%64 == 63 {
			if w.strs.count != len(oracle) {
				t.Fatalf("step %d: count %d, oracle %d", step, w.strs.count, len(oracle))
			}
			for k, v := range oracle {
				if got, ok := w.strs.get(k); !ok || got != v {
					t.Fatalf("step %d: get(%q) = %d,%v, want %d", step, k, got, ok, v)
				}
			}
			fresh := internRandStr(rng)
			if _, hit := oracle[fresh]; !hit {
				if _, ok := w.strs.get(fresh); ok {
					t.Fatalf("step %d: fresh %q hits table", step, fresh)
				}
			}
		}
	}
	// The full stream reads back: every REF resolves to its own string.
	r := NewReader(w.buf)
	for _, s := range seq {
		got, err := r.ReadString()
		if err != nil {
			t.Fatalf("read-back: %v", err)
		}
		if got != s {
			t.Fatalf("read-back: got %q, want %q", got, s)
		}
	}
}

// TestInternTableSharedPoolWithDescName drives one content through
// the string-value and descriptor-name roles in both orders: the
// second role is a REF to the id bound by the first.
func TestInternTableSharedPoolWithDescName(t *testing.T) {
	d := &Desc{Kind: KindInterface, Name: "shared"}

	w := NewWriter()
	if err := w.WriteString("shared"); err != nil {
		t.Fatal(err)
	}
	pre := len(w.buf)
	if err := w.WriteDesc(d); err != nil {
		t.Fatal(err)
	}
	if w.buf[pre] != classDesc<<4|byte(KindInterface) {
		t.Fatalf("desc head %#x", w.buf[pre])
	}
	if class := w.buf[pre+1] >> 4; class != classRef {
		t.Fatalf("desc name class %#x, want REF", class)
	}
	if id, err := NewReader(w.buf[pre+1:]).readTokenArg(classRef); err != nil || id != 0 {
		t.Fatalf("desc name ref %d, %v, want id 0", id, err)
	}

	w2 := NewWriter()
	pre2 := len(w2.buf)
	if err := w2.WriteDesc(d); err != nil {
		t.Fatal(err)
	}
	if class := w2.buf[pre2+1] >> 4; class != classString {
		t.Fatalf("desc name class %#x, want literal", class)
	}
	pre3 := len(w2.buf)
	if err := w2.WriteString("shared"); err != nil {
		t.Fatal(err)
	}
	if class := w2.buf[pre3] >> 4; class != classRef {
		t.Fatalf("string class %#x, want REF", class)
	}
	if id, err := NewReader(w2.buf[pre3:]).readTokenArg(classRef); err != nil || id != 1 {
		t.Fatalf("string ref %d, %v, want id 1 (name after desc id 0)", id, err)
	}
}

// TestInternTableResetClearsPool pins the Reset carrier move: entries
// gone, ids back to zero, slot array retained for reuse.
func TestInternTableResetClearsPool(t *testing.T) {
	w := NewWriter()
	for i := range 20 {
		if err := w.WriteString("s" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	slots := len(w.strs.slots)
	if slots == 0 {
		t.Fatal("no slots allocated before reset")
	}
	w.Reset()
	if w.strs.count != 0 || w.nextID != 0 {
		t.Fatalf("after reset: count %d nextID %d", w.strs.count, w.nextID)
	}
	if got := len(w.strs.slots); got != slots {
		t.Fatalf("slot array %d after reset, want retained %d", got, slots)
	}
	if _, ok := w.strs.get("s5"); ok {
		t.Fatal("s5 survives reset")
	}
	pre := len(w.buf)
	if err := w.WriteString("s5"); err != nil {
		t.Fatal(err)
	}
	if class := w.buf[pre] >> 4; class != classString {
		t.Fatalf("class %#x after reset, want literal", class)
	}
	r := NewReader(w.buf)
	got, err := r.ReadString()
	if err != nil || got != "s5" {
		t.Fatalf("read after reset: %q, %v", got, err)
	}
}

// TestWriteDescReboundErrorFields pins the raw render-side verdict
// of a name rebound: kind, offset, got, and the phrase the rank-walk
// verdict mirrors (codec_maprank_internal_test.go).
func TestWriteDescReboundErrorFields(t *testing.T) {
	a := &Desc{Kind: KindStruct, Name: "rbClash",
		Fields: []Field{{Name: "X", Type: &Desc{Kind: KindInt, Width: 8}}}}
	b := &Desc{Kind: KindStruct, Name: "rbClash",
		Fields: []Field{{Name: "Y", Type: &Desc{Kind: KindString}}}}
	w := NewWriter()
	if err := w.WriteDesc(a); err != nil {
		t.Fatal(err)
	}
	err := w.WriteDesc(b)
	if err == nil {
		t.Fatal("rebound: want a rejection")
	}
	we, ok := err.(*Error)
	if !ok {
		t.Fatalf("verdict %T, want *Error", err)
	}
	if we.Kind != "register_conflict" || we.Off != -1 || we.Got != "rbClash" {
		t.Fatalf("verdict kind=%q off=%d got=%v", we.Kind, we.Off, we.Got)
	}
	const wantMsg = "gbon/wire: descriptor name rebound to a structurally different type: distinct types sharing one name are ambiguous (qualify the module path)"
	if we.Msg != wantMsg {
		t.Fatalf("msg %q, want %q", we.Msg, wantMsg)
	}
}

// TestInternTableFabricatedCollision pins the string comparison of
// probe hits: distinct strings sharing one stored hash and one probe
// chain resolve to their own ids.
func TestInternTableFabricatedCollision(t *testing.T) {
	// The probe chain of "y" carries a foreign entry first: same
	// stored hash, different string. Membership must walk past it.
	w := NewWriter()
	w.strs.slots = make([]internSlot, 8)
	w.strs.mask = 7
	w.strs.count = 2
	hy := maphash.String(internSeed, "y")
	w.strs.slots[hy&7] = internSlot{s: "x", hash: hy, idPlus1: 1}
	w.strs.slots[(hy+1)&7] = internSlot{s: "y", hash: hy, idPlus1: 2}
	if id, ok := w.strs.get("y"); !ok || id != 1 {
		t.Fatalf("get(y) = %d,%v, want id 1 (the chain head shares the hash)", id, ok)
	}

	// The probe chain of "x" carries "x" itself first: a hit on the
	// opening slot stays its own id.
	w2 := NewWriter()
	w2.strs.slots = make([]internSlot, 8)
	w2.strs.mask = 7
	w2.strs.count = 2
	hx := maphash.String(internSeed, "x")
	w2.strs.slots[hx&7] = internSlot{s: "x", hash: hx, idPlus1: 1}
	w2.strs.slots[(hx+1)&7] = internSlot{s: "w", hash: hx, idPlus1: 2}
	if id, ok := w2.strs.get("x"); !ok || id != 0 {
		t.Fatalf("get(x) = %d,%v, want id 0", id, ok)
	}
}
