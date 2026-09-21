package gbon_test

// U7 property tests for positional path references (7.2): P1 — an
// aliased graph and its independent-ized twin encode DIFFERENTLY;
// P2 — decode∘encode re-encodes byte-identically across the
// path-carrying shapes (self-referential, cyclic, mid-fill,
// double-reference); graph counters (nodes/edges) are derived by a
// pre-run over the DECODED graph — no magic constants;
// pointer-grain terminals at interface positions keep the cell route
// for double-reference forms; plain interiors carry paths (7.2).

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// pprCounters walks a decoded graph by reflection and counts pointer
// nodes (distinct non-nil pointer targets) and edges (pointer-valued
// slots) — the pre-run derivation.
func pprCounters(v any) (nodes, edges int) {
	seenNode := map[uintptr]bool{}
	var walk func(rv reflect.Value, depth int)
	walk = func(rv reflect.Value, depth int) {
		if depth > 32 || !rv.IsValid() {
			return
		}
		switch rv.Kind() {
		case reflect.Pointer:
			if rv.IsNil() {
				return
			}
			edges++
			a := rv.Pointer()
			if !seenNode[a] {
				seenNode[a] = true
				nodes++
				walk(rv.Elem(), depth+1)
			}
		case reflect.Struct:
			for i := 0; i < rv.NumField(); i++ {
				walk(rv.Field(i), depth+1)
			}
		case reflect.Slice:
			for i := 0; i < rv.Len(); i++ {
				walk(rv.Index(i), depth+1)
			}
		case reflect.Interface:
			if !rv.IsNil() {
				walk(rv.Elem(), depth+1)
			}
		}
	}
	walk(reflect.ValueOf(v), 0)
	return nodes, edges
}

func pprRoundTrip(t *testing.T, v any) {
	t.Helper()
	b, err := gbon.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rt := reflect.New(reflect.TypeOf(v))
	if err := gbon.Unmarshal(b, rt.Interface()); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b2, err := gbon.Marshal(rt.Elem().Interface())
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatalf("P2 re-encode drift:\n  first: %x\n  second: %x", b, b2)
	}
	n1, e1 := pprCounters(v)
	n2, e2 := pprCounters(rt.Elem().Interface())
	if n1 != n2 || e1 != e2 {
		t.Fatalf("graph counters drifted: nodes %d→%d edges %d→%d", n1, n2, e1, e2)
	}
}

// P1: aliased vs independent — the bytes differ.
func TestPPRP1AliasedDiffers(t *testing.T) {
	type S struct{ A, B, C int64 }
	type W struct {
		V *S
		P *int64
	}
	s := S{A: 1, B: 2, C: 3}
	aliased := W{V: &s, P: &s.B}
	indep := W{V: &s}
	iB := int64(2)
	indep.P = &iB
	ba, err := gbon.Marshal(&aliased)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := gbon.Marshal(&indep)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ba, bi) {
		t.Fatalf("P1 violated: aliased and independent graphs encode identically")
	}
	// each side round-trips on its own
	pprRoundTrip(t, &aliased)
	pprRoundTrip(t, &indep)
}

// P2 shapes.
func TestPPRP2Shapes(t *testing.T) {
	type S struct{ A, B, C int64 }
	type W struct {
		V *S
		P *int64
	}
	// interior mid-field alias
	s := S{A: 1, B: 2, C: 3}
	pprRoundTrip(t, &W{V: &s, P: &s.B})
	// self-referential path: a container holding a pointer into itself
	type Self struct {
		X int64
		P *int64
	}
	se := Self{X: 4}
	se.P = &se.X
	pprRoundTrip(t, &se)
	// cycle through the aliased record
	type Ring struct {
		Name string
		N    *Ring
		X    int64
	}
	type Host struct {
		R *Ring
		P *int64
	}
	r1 := Ring{Name: "a"}
	r2 := Ring{Name: "b"}
	r1.N = &r2
	r2.N = &r1
	h := Host{R: &r1, P: &r2.X}
	pprRoundTrip(t, &h)
	// cross-record mid-fill: the second record's slot aliased while the
	// first is still open
	type Pair struct {
		L, R *S
		Q    *int64
	}
	l := S{A: 7}
	rr := S{A: 8, B: 9}
	pprRoundTrip(t, &Pair{L: &l, R: &rr, Q: &rr.B})
	// double reference (7.2): q → p → interior
	type DR struct {
		A int64
		P *int64
		Q **int64
	}
	d := DR{A: 5}
	d.P = &d.A
	box := d.P
	d.Q = &box
	pprRoundTrip(t, &d)
	// slice-element interior
	type E struct{ F int64 }
	type W3 struct {
		V []E
		P *int64
	}
	v3 := []E{{1}, {2}, {3}}
	pprRoundTrip(t, &W3{V: v3, P: &v3[2].F})
}

// Pointer-grain terminals at interface positions keep the cell route
// for double-reference forms (the repeat REF names the cell); plain
// interiors take the path form (7.2): the round trip still holds.
func TestPPRInterfaceCellRoute(t *testing.T) {
	type T struct {
		F int64
		Q *int64
	}
	type W struct {
		V *T
		I any
	}
	tt := T{F: 1}
	w := W{V: &tt}
	qp := &tt.Q
	w.I = qp
	pprRoundTrip(t, &w)
}

// Array-element interiors over inline [N]T fields (7.2 rooting): the
// path roots at the field's ARRAY record, and the handle binds the
// decoded slot's storage — identity by address survives the round trip
// at any field offset, with the byte-exact re-encode through the
// pointer re-marshal.
func TestPPRArrayElementIdentity(t *testing.T) {
	roundTrip := func(t *testing.T, marshal func() ([]byte, error), unmarshal func(b []byte) error, remarshal func() ([]byte, error), ident func() bool) {
		t.Helper()
		b, err := marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unmarshal(b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !ident() {
			t.Fatalf("array-element identity lost: handle does not address the decoded slot")
		}
		b2, err := remarshal()
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if !bytes.Equal(b, b2) {
			t.Fatalf("re-encode drift:\n  first: %x\n  second: %x", b, b2)
		}
	}
	// field at offset 0: the array's storage base shares the owner's
	// address — the backing root must resolve to the ARRAY record, not
	// the owner's grain
	type Head struct {
		A [4]byte
		P *byte
	}
	h := &Head{A: [4]byte{1, 2, 3, 4}}
	h.P = &h.A[1]
	var hr Head
	roundTrip(t, func() ([]byte, error) { return gbon.Marshal(h) },
		func(b []byte) error { return gbon.Unmarshal(b, &hr) },
		func() ([]byte, error) { return gbon.Marshal(&hr) },
		func() bool { return &hr.A[1] == hr.P })
	// field past a leading scalar
	type Tail struct {
		Z int64
		A [3]int32
		P *int32
	}
	d := &Tail{Z: 9, A: [3]int32{7, 8, 9}}
	d.P = &d.A[2]
	var tr Tail
	roundTrip(t, func() ([]byte, error) { return gbon.Marshal(d) },
		func(b []byte) error { return gbon.Unmarshal(b, &tr) },
		func() ([]byte, error) { return gbon.Marshal(&tr) },
		func() bool { return &tr.A[2] == tr.P })
	// array of structs: the element step descends into a record-valued
	// element's field
	type E struct{ F int64 }
	type Rec struct {
		V [2]E
		P *int64
	}
	r := &Rec{V: [2]E{{1}, {2}}}
	r.P = &r.V[1].F
	var rr Rec
	roundTrip(t, func() ([]byte, error) { return gbon.Marshal(r) },
		func(b []byte) error { return gbon.Unmarshal(b, &rr) },
		func() ([]byte, error) { return gbon.Marshal(&rr) },
		func() bool { return &rr.V[1].F == rr.P })
}

// Blob-element interiors (7.2 rooting): []byte element slots root at
// the BLOB record; indices ride the backing's declared index space — a
// window's local index never appears — and the handle binds the decoded
// backing's slot storage.
func TestPPRBlobElementIdentity(t *testing.T) {
	type W struct {
		B []byte
		P *byte
	}
	b := []byte{1, 2, 3, 4, 5}
	w := W{B: b, P: &b[2]}
	bw, err := gbon.Marshal(&w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var w2 W
	if err := gbon.Unmarshal(bw, &w2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if &w2.B[2] != w2.P || w2.P == nil || *w2.P != 3 {
		t.Fatalf("blob-element identity lost: handle does not address the decoded slot")
	}
	bw2, err := gbon.Marshal(&w2)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(bw, bw2) {
		t.Fatalf("re-encode drift:\n  first: %x\n  second: %x", bw, bw2)
	}
	// a window over the same allocation: the element index lands in the
	// backing's index space, so the handle addresses the window's own
	// slot
	full := []byte{9, 8, 7, 6, 5}
	vw := W{B: full[1:4]}
	vw.P = &vw.B[1]
	bv, err := gbon.Marshal(&vw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wv W
	if err := gbon.Unmarshal(bv, &wv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if &wv.B[1] != wv.P || wv.P == nil || *wv.P != 7 {
		t.Fatalf("blob-view element identity lost: handle does not address the decoded slot")
	}
}

// pprIdentRT is the identity round trip of one interior-pointer shape:
// marshal, unmarshal, address identity of the handle, and the byte-exact
// re-encode of the decoded value through the pointer re-marshal.
func pprIdentRT(t *testing.T, marshal func() ([]byte, error), unmarshal func(b []byte) error, remarshal func() ([]byte, error), ident func() bool) {
	t.Helper()
	b, err := marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !ident() {
		t.Fatalf("interior identity lost: handle does not address the decoded slot")
	}
	b2, err := remarshal()
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatalf("re-encode drift:\n  first: %x\n  second: %x", b, b2)
	}
}

// Nested fixed-array interiors (7.2 rooting): the path roots at the
// outermost inline array's ARRAY record; a nested array at element-0
// shares the owner's storage base but is a distinct backing — the
// element-stride discrimination keeps the owner's record resolvable —
// and element types of any kind (primitive, named, struct) ride the
// same descent.
func TestPPRNestedArrayElementIdentity(t *testing.T) {
	type P2 struct {
		M [2][2]byte
		Q *byte
	}
	p2 := &P2{M: [2][2]byte{{1, 2}, {3, 4}}}
	p2.Q = &p2.M[1][0]
	var r2 P2
	pprIdentRT(t,
		func() ([]byte, error) { return gbon.Marshal(p2) },
		func(b []byte) error { return gbon.Unmarshal(b, &r2) },
		func() ([]byte, error) { return gbon.Marshal(&r2) },
		func() bool { return &r2.M[1][0] == r2.Q })
	type E struct{ F int64 }
	type PE struct {
		M [2][2]E
		Q *int64
	}
	pe := &PE{M: [2][2]E{{{1}, {2}}, {{3}, {4}}}}
	pe.Q = &pe.M[1][1].F
	var re PE
	pprIdentRT(t,
		func() ([]byte, error) { return gbon.Marshal(pe) },
		func(b []byte) error { return gbon.Unmarshal(b, &re) },
		func() ([]byte, error) { return gbon.Marshal(&re) },
		func() bool { return &re.M[1][1].F == re.Q })
	type SA struct {
		V [][2]int32
		Q *int32
	}
	v := [][2]int32{{1, 2}, {3, 4}}
	sa := &SA{V: v}
	sa.Q = &sa.V[1][0]
	var rs SA
	pprIdentRT(t,
		func() ([]byte, error) { return gbon.Marshal(sa) },
		func(b []byte) error { return gbon.Unmarshal(b, &rs) },
		func() ([]byte, error) { return gbon.Marshal(&rs) },
		func() bool { return len(rs.V) == 2 && &rs.V[1][0] == rs.Q })
}

// Nested slice-element interiors (7.2 rooting): an element of a slice
// whose own elements live in a separate backing roots its interior
// paths at that element's backing record; the address identity holds
// through the window over one backing.
func TestPPRNestedSliceElementIdentity(t *testing.T) {
	type SS struct {
		V [][]int32
		Q *int32
	}
	i0 := []int32{1, 2}
	i1 := []int32{3, 4}
	ss := SS{V: [][]int32{i0, i1}}
	ss.Q = &ss.V[1][1]
	var rs SS
	pprIdentRT(t,
		func() ([]byte, error) { return gbon.Marshal(&ss) },
		func(b []byte) error { return gbon.Unmarshal(b, &rs) },
		func() ([]byte, error) { return gbon.Marshal(&rs) },
		func() bool { return len(rs.V) == 2 && &rs.V[1][1] == rs.Q })
	type SS3 struct {
		V [][][]int32
		Q *int32
	}
	x0 := []int32{1, 2}
	x1 := []int32{3, 4}
	inner := [][]int32{x0, x1}
	ss3 := SS3{V: [][][]int32{inner}}
	ss3.Q = &ss3.V[0][1][1]
	var rs3 SS3
	pprIdentRT(t,
		func() ([]byte, error) { return gbon.Marshal(&ss3) },
		func(b []byte) error { return gbon.Unmarshal(b, &rs3) },
		func() ([]byte, error) { return gbon.Marshal(&rs3) },
		func() bool { return len(rs3.V) == 1 && len(rs3.V[0]) == 2 && &rs3.V[0][1][1] == rs3.Q })
}

// Interior-slot population bound: a record-reference graph at scale
// (rows of inline-member structs, no interior pointers) scans without
// the per-field slot storm — the slot registrations stop at the
// population bound while containers and climb links keep building, and
// the encode stays fast. The guard is a wall-clock bound generous to
// the fixed codec and unreachable for the per-field registration it
// replaced (minutes at this scale).
func TestPPRSlotPopulationBound(t *testing.T) {
	if raceEnabled {
		t.Skip("wall-clock bound under the race detector is distortion, not signal")
	}
	type Member struct {
		Key, Region string
		Tier        int32
		Active      bool
	}
	type Row struct {
		M [5]Member
		Q int64
	}
	const rows = 300_000
	chunks := make([][]Row, 30)
	for c := range chunks {
		chunks[c] = make([]Row, rows/30)
		for i := range chunks[c] {
			for m := range chunks[c][i].M {
				chunks[c][i].M[m] = Member{Key: "k", Tier: int32(m)}
			}
		}
	}
	start := time.Now()
	b, err := gbon.Marshal(&struct{ C [][]Row }{C: chunks})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The wall-clock bound guards the lazy-weak registration path (the
	// weak.Make-per-container regression multiplies it into minutes);
	// the slotPopMax truncation itself is silent on this pointer-free
	// graph — a bond-specific counting guard lives in the registry.
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("lazy-weak registration regressed: marshal of a pointer-free graph took %v", el)
	}
	dec := gbon.NewDecoder(bytes.NewReader(b))
	dec.SetLimits(gbon.Limits{MaxNodes: 32_000_000, MaxBytes: 1 << 30})
	var back struct{ C [][]Row }
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.C) != 30 || len(back.C[0]) != rows/30 || back.C[0][7].M[3].Tier != 3 {
		t.Fatalf("round trip lost data")
	}
}

// Interface-terminal interiors (7.2 interface-grain clause): a pointer
// to an interface-typed slot carries the explicit path form — the
// terminal slot's declared type is interface-compatible — and the
// slot-storage invariant binds every handle to the decoded slot's
// storage, so repeated handles alias one address.
func TestPPRUserReproInterfaceInterior(t *testing.T) {
	type testS4 struct{ F1, F2, F3, F4 any }
	s := &testS4{}
	s.F1 = &s.F3
	s.F2 = &s.F3
	s.F3 = true
	s.F4 = &s.F3
	var b bytes.Buffer
	enc := gbon.NewEncoder(&b)
	if err := enc.Encode(s); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(b.Bytes()))
	if err := dec.Register((*testS4)(nil), (*any)(nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	var actual any
	if err := dec.Decode(&actual); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rt, ok := actual.(*testS4)
	if !ok {
		t.Fatalf("decoded %T", actual)
	}
	if rt.F1 != any(&rt.F3) || rt.F2 != any(&rt.F3) || rt.F4 != any(&rt.F3) {
		t.Fatalf("interface-terminal identity lost: handles do not address the decoded slot")
	}
	if p, ok := rt.F1.(*any); !ok || *p != true || rt.F3 != true {
		t.Fatalf("interface-terminal value lost")
	}
	var b2 bytes.Buffer
	enc2 := gbon.NewEncoder(&b2)
	if err := enc2.Encode(actual); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(b.Bytes(), b2.Bytes()) {
		t.Fatalf("re-encode drift:\n  first: %x\n  second: %x", b.Bytes(), b2.Bytes())
	}
}

// Pointer-to-handle over an interface terminal (7.2 double reference):
// the handle is a standalone cell whose body carries the explicit path
// — legal regardless of derivability, cell body, not pointer position —
// and the referencing pointer chain reads through to the aliased slot.
func TestPPRDoubleRefPointerToHandle(t *testing.T) {
	type Double struct {
		F1, F3 any
		PP     **any
	}
	d := &Double{}
	d.F1 = &d.F3
	d.F3 = true
	h := &d.F1
	d.PP = &h
	var b bytes.Buffer
	enc := gbon.NewEncoder(&b)
	if err := enc.Encode(d); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(b.Bytes()))
	if err := dec.Register((*Double)(nil), (**any)(nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	var actual any
	if err := dec.Decode(&actual); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rt := actual.(*Double)
	if rt.F1 != any(&rt.F3) {
		t.Fatalf("interface-terminal identity lost: F1 does not address the F3 slot")
	}
	if rt.PP == nil || *rt.PP != any(&rt.F1) {
		t.Fatalf("pointer-to-handle identity lost: *PP does not address the F1 slot")
	}
	if bp := *rt.PP; *bp != any(&rt.F3) {
		t.Fatalf("pointer-to-handle chain lost the value")
	}
	var b2 bytes.Buffer
	enc2 := gbon.NewEncoder(&b2)
	if err := enc2.Encode(actual); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(b.Bytes(), b2.Bytes()) {
		t.Fatalf("re-encode drift:\n  first: %x\n  second: %x", b.Bytes(), b2.Bytes())
	}
}
