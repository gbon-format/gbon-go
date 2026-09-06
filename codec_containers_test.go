package gbon_test

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Aliasing observability and key rejection/dup matrix over the
// public API. Bytes are never inspected here except for
// crafted malformed streams.

type twoWin struct{ S1, S2 []int }

// Overlapping windows share one
// reconstructed backing; the equivWindows operator checks geometry
// and two-sided mutation visibility.
func TestSliceAliasing(t *testing.T) {
	b := []int{1, 2, 3}
	v := twoWin{S1: b[0:2], S2: b[1:3]} // windows [0,2)cap3 and [1,3)
	out := roundTrip(t, v).(twoWin)
	if !equivWindows(t, [4][]int{v.S1, v.S2, nil, nil}, [4][]int{out.S1, out.S2, nil, nil}) {
		t.Fatalf("aliasing lost: in %v out %v", v, out)
	}
}

// RT-cap: g26 fixture — append hijack within spare
// capacity hits the neighbor; exhausted-cap append reallocates and leaves
// the neighbor untouched; the reallocation threshold equals cap; g28
// fixture — reslice [len,cap) sees implicit zeros.
func TestCapSemantics(t *testing.T) {
	arr := []int{1, 2, 3, 4}
	v := P{A: arr[0:2], B: arr[2:4]}
	out := roundTrip(t, v).(P)
	if cap(out.A) != 4 || len(out.A) != 2 {
		t.Fatalf("A geometry: len=%d cap=%d", len(out.A), cap(out.A))
	}
	hij := append(out.A, 42)
	if out.B[0] != 42 {
		t.Fatalf("append-hijack lost: B[0]=%d", out.B[0])
	}
	if &hij[0] != &out.A[0] {
		t.Fatalf("append within cap reallocated")
	}
	full := append(out.A, 5, 6) // len==cap here
	if len(full) != 4 || cap(full) != 4 || &full[0] != &out.A[0] {
		t.Fatalf("threshold mismatch: len=%d cap=%d", len(full), cap(full))
	}
	realloc := append(full, 7) // len>cap → new backing
	if &realloc[0] == &out.A[0] {
		t.Fatalf("realloc did not detach")
	}
	if realloc[4] != 7 {
		t.Fatalf("realloc append lost: %v", realloc)
	}
	if out.B[0] != 5 {
		t.Fatalf("realloc append touched neighbor: B[0]=%d", out.B[0])
	}
	big := make([]int, 2, 8)
	big[0] = 5
	w := roundTrip(t, R{W1: big, W2: big[1:2]}).(R)
	tail := w.W1[2:8] // reslice beyond len sees implicit zeros [E,L)
	for i, x := range tail {
		if x != 0 {
			t.Fatalf("tail[%d]=%d, want implicit zero", i, x)
		}
	}
}

// Equal data pointers across fields;
// pointer keys keep identity and unified interning makes a key
// reachable from a value position; zero-size keys encode per slot.
func TestSharedIdentity(t *testing.T) {
	a := []int{7, 8}
	out := roundTrip(t, twoWin{S1: a, S2: a}).(twoWin)
	if &out.S1[0] != &out.S2[0] {
		t.Fatalf("full-window sharing lost")
	}
	out.S1[0] = 100
	if out.S2[0] != 100 {
		t.Fatalf("shared mutation not visible")
	}

	x1, x2 := X{N: 1}, X{N: 1} // equal values, distinct pointers
	h := holder{M: map[*X]int{&x1: 1, &x2: 2}, P: &x1}
	rh := roundTrip(t, h).(holder)
	if len(rh.M) != 2 {
		t.Fatalf("pointer-key slots collapsed: len=%d", len(rh.M))
	}
	if v, ok := rh.M[rh.P]; !ok || v != 1 {
		t.Fatalf("unified interning lost: lookup via value-position copy gave %d,%v", v, ok)
	}
	if !(*rh.P == *kOf(rh.M, 2) && rh.P != kOf(rh.M, 2)) {
		t.Fatalf("R1 identity: equal pointees must be distinct pointers")
	}

	z1, z2 := new(struct{}), new(struct{})
	mz0 := map[*struct{}]int{z1: 1, z2: 2} // zero-size pointers == in Go: one live slot
	mz := roundTrip(t, mz0).(map[*struct{}]int)
	if len(mz) != len(mz0) {
		t.Fatalf("zero-size keys: len=%d want %d", len(mz), len(mz0))
	}
}

type holder struct {
	M map[*X]int
	P *X
}

func kOf(m map[*X]int, want int) *X {
	for k := range m {
		if m[k] == want {
			return k
		}
	}
	return nil
}

// RT-cyckey: g32 fixture and mutual cycles.
func TestRTCycleKey(t *testing.T) {
	n := N{}
	n.Next = &n
	m := roundTrip(t, map[*N]int{&n: 1}).(map[*N]int)
	if len(m) != 1 {
		t.Fatalf("len=%d", len(m))
	}
	for k := range m {
		if k.Next != k {
			t.Fatalf("key self-reference broken")
		}
	}
	type C struct{ F *C }
	p1, p2 := &C{}, &C{}
	p1.F, p2.F = p2, p1
	mm := roundTrip(t, map[*C]int{p1: 1, p2: 2}).(map[*C]int)
	if len(mm) != 2 {
		t.Fatalf("mutual-cycle keys collapsed: len=%d", len(mm))
	}
	for k, want := range mm {
		if k.F == nil || k.F.F != k {
			t.Fatalf("mutual cycle not closed (want %d)", want)
		}
	}
}

// Full category matrix over both mechanics.
func TestDuplicateKeys(t *testing.T) {
	// (а) pointer-free: crafted byte-equal keys, non-adjacent, in
	// non-canonical stream order → ErrFormat from the byte-set.
	crafted := []struct {
		name  string
		input any
		from  []byte // byte pattern of the second key
		to    []byte // byte-equal duplicate of the first key
	}{
		{"string", map[string]int{"a": 1, "b": 2}, []byte{'b'}, []byte{'a'}},
		{"int", map[int]int{1: 10, 2: 20}, []byte{0x24}, []byte{0x22}},                            // zz(2)→zz(1)
		{"float", map[float64]int{1: 10, 2: 20}, nil, nil},                                        // via raw-bits patch below
		{"struct", map[Point]int{{1, 2}: 10, {2, 1}: 10}, []byte{0x24, 0x22}, []byte{0x22, 0x24}}, // swap to equal
		{"array", map[[2]int64]int{{1, 2}: 10, {2, 1}: 10}, []byte{0x24, 0x22}, []byte{0x22, 0x24}},
	}
	for _, tc := range crafted {
		t.Run(tc.name, func(t *testing.T) {
			data := mustMarshal(t, tc.input)
			var mutant []byte
			if tc.from == nil { // float: patch second key raw bits to +1.0
				one := [8]byte{0x3F, 0xF0, 0, 0, 0, 0, 0, 0}
				two := [8]byte{0x40, 0x00, 0, 0, 0, 0, 0, 0}
				mutant = bytes.Replace(data, two[:], one[:], 1)
			} else {
				mutant = bytes.Replace(data, tc.from, tc.to, 1)
			}
			if bytes.Equal(mutant, data) {
				t.Fatalf("patch did not apply")
			}
			out := reflect.New(reflect.TypeOf(tc.input))
			err := gbon.Unmarshal(mutant, out.Interface())
			if !errors.Is(err, gbon.ErrFormat) {
				t.Fatalf("dup not rejected: %v", err)
			}
		})
	}
	// (а) collapse guard: the ±0 pair is byte-distinct but Go-equal — it
	// cannot be constructed as a live map (Go collapses at insert), so its
	// crafted-stream rejection is covered by the TestErrorMapping
	// pm-zero-pair fixture.
	// (б) pointer-containing: crafted double REF on one id → collapse guard.
	x1, x2, x3 := X{N: 1}, X{N: 1}, X{N: 1}
	triple := mustMarshal(t, map[*X]int{&x1: 1, &x2: 2, &x3: 3})
	refThird := bytes.Replace(triple, []byte{0xB0, 0x22, 0x26}, []byte{0xC2, 0x0C, 0x26}, 1) // literal(id12)→REF id12 (u8 form)
	if bytes.Equal(refThird, triple) {
		t.Fatalf("double-REF patch did not apply")
	}
	mp := reflect.New(reflect.TypeFor[map[*X]int]())
	if err := gbon.Unmarshal(refThird, mp.Interface()); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("double-REF dup not rejected: %v", err)
	}
	// (б) a REF before the intern definition → ErrFormat.
	refFirst := bytes.Replace(triple, []byte{0xB0, 0x22, 0x22}, []byte{0xCB, 0x22}, 1) // first literal→REF id11 (undefined yet)
	if bytes.Equal(refFirst, triple) {
		t.Fatalf("pre-def REF patch did not apply")
	}
	if err := gbon.Unmarshal(refFirst, mp.Interface()); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("pre-definition REF not rejected: %v", err)
	}
	// Legality negative: g29 stream — byte-identical literal keys, two
	// distinct intern objects — must be accepted.
	ok := roundTrip(t, map[*X]int{&x1: 1, &x2: 2}).(map[*X]int)
	if len(ok) != 2 {
		t.Fatalf("legal identical literals rejected: len=%d", len(ok))
	}
}

// NaN penetration at key depth (with
// the field path), chan keys (KO-7); interface keys incl. nil and
// typed-nil dynamic values are legal.
func TestRejectKeys(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name  string
		input any
	}{
		{"nan field", map[struct{ F float64 }]int{{F: nan}: 1}},
		{"chan", map[chan int]int{}},
	}
	// Interface keys are encodable, including nil
	// and typed-nil dynamic values.
	for _, v := range []any{map[any]int{}, map[any]int{nil: 1}} {
		if _, err := gbon.Marshal(v); err != nil {
			t.Fatalf("iface map must encode: %v", err)
		}
	}
	var p *int
	if _, err := gbon.Marshal(map[any]int{p: 1}); err != nil {
		t.Fatalf("typed-nil iface key must encode: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := gbon.Marshal(tc.input)
			if !errors.Is(err, gbon.ErrUnsupported) {
				t.Fatalf("want ErrUnsupported, got %v", err)
			}
		})
	}
	_, mErr := gbon.Marshal(map[struct{ F float64 }]int{{F: nan}: 1})
	var nk *gbon.Error
	if mErr == nil || !errors.As(mErr, &nk) || nk.Class() != "unsupported_kind" {
		t.Fatalf("NaN key error: want class unsupported_kind, got %v", mErr)
	}
	if !strings.Contains(nk.Path, ".F") {
		t.Fatalf("NaN key error lacks field path: %v", mErr)
	}
}

// After a first error both codec
// facades fail every subsequent call — never a success, never a panic.
func TestStickyError(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(make(chan int)); err == nil {
		t.Fatalf("chan encode must fail")
	}
	if err := enc.Encode(5); err == nil {
		t.Fatalf("encoder reused after error")
	}

	good := mustMarshal(t, 7)
	bad := good[:len(good)-1] // truncate the value tail mid-argument
	dec := gbon.NewDecoder(bytes.NewReader(bad))
	var n int
	if err := dec.Decode(&n); err == nil {
		t.Fatalf("decode of corrupt stream must fail")
	}
	if err := dec.Decode(&n); err == nil {
		t.Fatalf("decoder reused after error")
	}
	if err := gbon.Unmarshal(good, &n); err != nil || n != 7 {
		t.Fatalf("independent decoder must be fine: %v %d", err, n)
	}
}

// A shared group must not bypass
// the backing budget (L is budgeted, not per-window len).
func TestSharedBudget(t *testing.T) {
	arr := []int{1, 2, 3, 4}
	data := mustMarshal(t, P{A: arr[0:2], B: arr[2:4]})
	out := reflect.New(reflect.TypeFor[P]())
	dec := gbon.NewDecoder(bytes.NewReader(data))
	dec.SetLimits(gbon.Limits{MaxSliceLen: 4})
	if err := dec.Decode(out.Interface()); err != nil {
		t.Fatalf("L=4 within budget: %v", err)
	}
	mutant := bytes.Replace(data, []byte{0x84, 0x04}, []byte{0x8F, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04}, 1)
	if bytes.Equal(mutant, data) {
		t.Fatalf("huge-L patch did not apply")
	}
	dec2 := gbon.NewDecoder(bytes.NewReader(mutant))
	dec2.SetLimits(gbon.Limits{MaxSliceLen: 8})
	if err := dec2.Decode(out.Interface()); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("huge L must exceed budget: %v", err)
	}
	if err := gbon.Unmarshal(mutant, out.Interface()); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("unmarshal huge L must fail bounded: %v", err)
	}
}

type twoEmpty struct{ A, B []struct{} }

// Zero-size element slices are fresh-owner without
// grouping — Marshal must not panic and len/cap survive the round trip.
func TestZeroSizeSliceElems(t *testing.T) {
	if _, err := gbon.Marshal(make([]struct{}, 1, 4)); err != nil {
		t.Fatalf("Marshal []struct{}: %v", err)
	}
	v := twoEmpty{A: make([]struct{}, 1, 4), B: make([]struct{}, 2)}
	out := roundTrip(t, v).(twoEmpty)
	if len(out.A) != 1 || cap(out.A) != 4 || len(out.B) != 2 || cap(out.B) != 2 {
		t.Fatalf("geometry lost: A=%d/%d B=%d/%d",
			len(out.A), cap(out.A), len(out.B), cap(out.B))
	}
}

type mpKey struct{ A, B *X }

// Struct keys with byte-equal skeletons (shared first
// pointer, equal pointees *q1==*q2, q1≠q2) and equal values — the full DFS
// address sequence is the total tie-break; the external REF holder P makes
// pair order byte-observable, repeated marshals stay identical under Go map
// iteration randomization.
func TestMapDeterminismMultiPtr(t *testing.T) {
	p := &X{N: 1}
	shared := &X{N: 5}
	for round := range 20 {
		q1, q2 := &X{N: 7}, &X{N: 7}
		v := struct {
			M map[mpKey]*X
			P *X
		}{
			M: map[mpKey]*X{{A: p, B: q1}: shared, {A: p, B: q2}: shared},
			P: q1,
		}
		b1 := mustMarshal(t, v)
		b2 := mustMarshal(t, v)
		if !bytes.Equal(b1, b2) {
			t.Fatalf("round %d: non-deterministic bytes", round)
		}
	}
}

// Cyclic and nil-terminated key components share the
// same skeleton bytes (repeat-address nil-marker collapse) — only the DFS
// address sequence separates the pairs; order must not leak into bytes.
func TestMapDeterminismCyclicVsNil(t *testing.T) {
	type ck struct{ A, B *kcyc }
	for round := range 20 {
		p := &kcyc{V: 3}
		c1 := &kcyc{V: 7}
		c1.Next = c1 // self-cycle
		c2 := &kcyc{V: 7}
		v := map[ck]int{{A: p, B: c1}: 0, {A: p, B: c2}: 0}
		b1 := mustMarshal(t, v)
		b2 := mustMarshal(t, v)
		if !bytes.Equal(b1, b2) {
			t.Fatalf("round %d: non-deterministic bytes", round)
		}
		out := roundTrip(t, v).(map[ck]int)
		if len(out) != 2 {
			t.Fatalf("round %d: distinct keys collapsed: len=%d", round, len(out))
		}
		shapes := 0
		var as []*kcyc
		for k := range out {
			if k.A == nil || k.B == nil {
				t.Fatalf("round %d: key component lost", round)
			}
			as = append(as, k.A)
			switch {
			case k.B.Next == k.B:
				shapes |= 1
			case k.B.Next == nil:
				shapes |= 2
			}
		}
		if shapes != 3 {
			t.Fatalf("round %d: cyclic-vs-nil shapes lost: %d", round, shapes)
		}
		if as[0] != as[1] {
			t.Fatalf("round %d: shared A not shared after RT", round)
		}
	}
}

// RT-empty-map-share: an empty non-nil map interns (A0 record) and a second
// field over the same object is a REF — identity survives, observable by
// mutation after the round trip.
func TestEmptyMapShare(t *testing.T) {
	m := map[string]int{}
	b, err := gbon.Marshal(mapShare{X: m, Y: m})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte{0xA0}) {
		t.Fatalf("empty map must emit a MAP record (A0): % x", b)
	}
	if !bytes.Contains(b, []byte{0xCA}) {
		t.Fatalf("second field over one empty map must be a REF: % x", b)
	}
	var out mapShare
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	out.X["k"] = 5
	if _, visible := out.Y["k"]; !visible {
		t.Fatal("empty-map identity lost: mutation through X invisible in Y")
	}
	if len(out.Y) != 1 {
		t.Fatalf("Y len = %d, want 1", len(out.Y))
	}
}
