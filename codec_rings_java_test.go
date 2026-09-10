package gbon_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Object-graph cycle scenarios over the java-semantics decoder: every
// positive case asserts the decode axes — exact dynamic type identity of
// filled slots, pointer-identity topology (canonical ring closure), and
// byte-identical re-encode of the decoded value.

type rgN struct{ Next *rgN }

type rgN2 struct{ A, B *rgN2 }

type rgW struct{ A, B any }

type rgW2 struct{ A any }

type rgAN struct {
	Next *rgAN
	Box  any
}

type rgAB struct{ Box any }

type rgAA struct {
	B *rgBB
	V int
}

type rgBB struct{ A **rgAA }

type rgAA3 struct{ V int }

type rgBB3 struct{ A ***rgAA3 }

type rgIH2 struct {
	Q int
	P *int
}

type rgPP struct{ X, Y any }

// ringDecodePlain round-trips src through the stateless entry point.
func ringDecodePlain(t *testing.T, src any) any {
	t.Helper()
	b := mustMarshal(t, src)
	out := reflect.New(reflect.TypeOf(src))
	if err := gbon.Unmarshal(b, out.Interface()); err != nil {
		t.Fatalf("plain decode: %v", err)
	}
	ringAssertBytes(t, src, out.Elem().Interface(), b)
	return out.Elem().Interface()
}

// ringDecodeReg round-trips src through a registered decoder.
func ringDecodeReg(t *testing.T, src any, examples ...any) any {
	t.Helper()
	b := mustMarshal(t, src)
	out := reflect.New(reflect.TypeOf(src))
	dec := gbon.NewDecoder(bytes.NewReader(b))
	if err := dec.Register(examples...); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := dec.Decode(out.Interface()); err != nil {
		t.Fatalf("reg decode: %v", err)
	}
	ringAssertBytes(t, src, out.Elem().Interface(), b)
	return out.Elem().Interface()
}

// ringAssertBytes asserts the byte axis: re-encoding the decoded value
// reproduces the source stream exactly.
func ringAssertBytes(t *testing.T, src, dst any, srcBytes []byte) {
	t.Helper()
	if srcBytes == nil {
		srcBytes = mustMarshal(t, src)
	}
	if got := mustMarshal(t, dst); !bytes.Equal(got, srcBytes) {
		t.Fatalf("re-encode drift: %x vs %x", got, srcBytes)
	}
}

// ringAnyRing builds a closed chain of any slots: slot i holds a
// pointer of depth grains[i] to the storage of slot i+1 (mod n).
func ringAnyRing(grains []int) (reflect.Value, []reflect.Value) {
	n := len(grains)
	slots := make([]reflect.Value, n)
	for i := range slots {
		slots[i] = reflect.New(reflect.TypeFor[any]()).Elem()
	}
	for i, g := range grains {
		cur := slots[(i+1)%n].Addr()
		for range g - 1 {
			cell := reflect.New(cur.Type()).Elem()
			cell.Set(cur)
			cur = cell.Addr()
		}
		slots[i].Set(cur)
	}
	return slots[0], slots
}

// ringWalkAny follows an any-slot ring from the decoded root: each step
// dereferences the slot content by its grain depth; the walk must land
// back on the first edge value after the full cycle.
func ringWalkAny(t *testing.T, root any, grains []int) {
	t.Helper()
	first := root
	cur := root
	for i, g := range grains {
		v := reflect.ValueOf(cur)
		if v.Kind() != reflect.Pointer {
			t.Fatalf("step %d: slot content %T is not a pointer", i, cur)
		}
		for range g - 1 {
			if v.Kind() != reflect.Pointer {
				t.Fatalf("step %d: grain over-runs at %v", i, v.Type())
			}
			v = v.Elem()
		}
		if v.Type() != reflect.TypeFor[*any]() {
			t.Fatalf("step %d: terminal link %v, want *any", i, v.Type())
		}
		cur = v.Elem().Interface()
	}
	if cur != first {
		t.Fatalf("ring walk open: landed %#v, want %#v", cur, first)
	}
}

// A self-referential pointer chain round-trips under pointer, any, and
// double-pointer roots; the chain grain grows with depth without a
// reject.
func TestRingSelfPointerChain(t *testing.T) {
	// anchor form: x2 holds &x1, x1 holds &x2, root x2
	var x1 any
	x2 := &x1
	x1 = &x2
	for _, tc := range []struct {
		label string
		reg   bool
	}{
		{"plain", false},
		{"reg", true},
	} {
		var out *any
		b := mustMarshal(t, x2)
		if tc.reg {
			dec := gbon.NewDecoder(bytes.NewReader(b))
			if err := dec.Register(new(any), new(*any), new(**any)); err != nil {
				t.Fatal(err)
			}
			if err := dec.Decode(&out); err != nil {
				t.Fatalf("%s: %v", tc.label, err)
			}
		} else if err := gbon.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if out == nil {
			t.Fatalf("%s: nil root cell", tc.label)
		}
		qq, ok := (*out).(**any)
		if !ok || *qq != out {
			t.Fatalf("%s: ring open (%#v)", tc.label, *out)
		}
		ringAssertBytes(t, x2, out, b)
	}

	// any root over the same graph
	var a1 any
	a2 := &a1
	a1 = &a2
	outA := ringDecodePlain(t, a1)
	q := outA.(**any)
	if *q == nil || **q != any(q) {
		t.Fatalf("any root: ring open (%#v)", outA)
	}

	// double-pointer root over the same graph
	bb := mustMarshal(t, &a2)
	var outC **any
	if err := gbon.Unmarshal(bb, &outC); err != nil {
		t.Fatalf("**any root: %v", err)
	}
	if outC == nil || *outC == nil {
		t.Fatalf("**any root: nil link")
	}
	if (**outC).(**any) != outC {
		t.Fatalf("**any root: ring open")
	}

	// anchor-loop graph (any slot holds **any) under a pointer root
	var px any
	p := &px
	px = &p
	outP := ringDecodePlain(t, p).(*any)
	if *((*outP).(**any)) != outP {
		t.Fatalf("anchor loop: ring open")
	}

	// grain grows with depth: len-3 and len-4 rings under pointer and
	// deeper roots
	for _, grains := range [][]int{{1, 1, 1}, {1, 1, 1, 1}} {
		root, _ := ringAnyRing(grains)
		b := mustMarshal(t, root.Interface())
		var o1 any
		if err := gbon.Unmarshal(b, &o1); err != nil {
			t.Fatalf("len %d any root: %v", len(grains), err)
		}
		ringWalkAny(t, o1, grains)
		ringAssertBytes(t, root.Interface(), o1, b)
		pv := root.Addr()
		b2 := mustMarshal(t, pv.Interface())
		var o2 *any
		if err := gbon.Unmarshal(b2, &o2); err != nil {
			t.Fatalf("len %d ptr root: %v", len(grains), err)
		}
		if o2 == nil || *o2 == nil {
			t.Fatalf("len %d ptr root: nil link", len(grains))
		}
		ringAssertBytes(t, pv.Interface(), o2, b2)
	}
}

// A pointer-to-interface self cycle inside a struct field round-trips.
func TestRingPointerFieldCycle(t *testing.T) {
	w := &rgW2{}
	w.A = &w.A
	out := ringDecodeReg(t, w, rgW2{}, &rgW2{}).(*rgW2)
	if out.A == nil {
		t.Fatal("field pointer nil")
	}
	p, ok := out.A.(*any)
	if !ok || p == nil {
		t.Fatalf("field: got %#v, want *any", out.A)
	}
	if *p != any(p) {
		t.Fatalf("field cycle open: %#v", *p)
	}
}

// Reverse REF edges over an already-materialized cell decode across
// entry points (slice, map, struct, nesting, doubled refs, grain +1/+2)
// and order permutations keep the shared identity.
func TestRingDagBackEdgeGrain(t *testing.T) {
	var d1 any
	d2 := &d1
	// base: elem 1 back-references the elem 0 cell
	h := []any{d2, &d2}
	out := ringDecodePlain(t, h).([]any)
	p0 := out[0].(*any)
	q1 := out[1].(**any)
	if *q1 != p0 {
		t.Fatal("base: shared cell lost")
	}
	// reverse order
	rev := ringDecodePlain(t, []any{&d2, d2}).([]any)
	if *(rev[0].(**any)) != rev[1].(*any) {
		t.Fatal("reverse: shared cell lost")
	}
	// single deep element without a back edge
	lone := ringDecodePlain(t, []any{&d2}).([]any)
	if lone[0] == nil {
		t.Fatal("lone: nil slot")
	}
	// map values
	mv := map[string]any{"a": d2, "b": &d2}
	mout := ringDecodePlain(t, mv).(map[string]any)
	if *(mout["b"].(**any)) != mout["a"].(*any) {
		t.Fatal("map entry: shared cell lost")
	}
	// struct fields
	sv := rgW{A: d2, B: &d2}
	sout := ringDecodeReg(t, sv, rgW{}, &rgW{}).(rgW)
	if *(sout.B.(**any)) != sout.A.(*any) {
		t.Fatal("struct fields: shared cell lost")
	}
	// nesting one composite level deeper
	nv := []any{h}
	nout := ringDecodePlain(t, nv).([]any)
	inner := nout[0].([]any)
	if *(inner[1].(**any)) != inner[0].(*any) {
		t.Fatal("nested: shared cell lost")
	}
	// the same cell referenced twice by back edges
	dv := []any{d2, &d2, &d2}
	dout := ringDecodePlain(t, dv).([]any)
	base := dout[0].(*any)
	for i := 1; i < 3; i++ {
		if *(dout[i].(**any)) != base {
			t.Fatalf("double ref %d: shared cell lost", i)
		}
	}
	// grain +1 and +2 back edges over pointer chains
	var z1 any
	z2 := &z1
	z3 := &z2
	z4 := &z3
	for _, tc := range []struct {
		label string
		v     []any
		g     int
	}{
		{"grain+1", []any{z3, z4}, 3},
		{"grain+2", []any{z2, z4}, 3},
	} {
		o := ringDecodePlain(t, tc.v).([]any)
		deep := o[1].(***any)
		if **deep == nil {
			t.Fatalf("%s: nil chain", tc.label)
		}
	}
	// a lonely deepest chain without any back edge
	l4 := ringDecodePlain(t, []any{z4}).([]any)
	if l4[0] == nil {
		t.Fatal("lonely deep chain: nil slot")
	}
}

// A map referencing its own address round-trips through the map root
// and the healed entry points: pointer root, typed slot, and the typed
// map-pointer target; int keys and a two-map ring work the same way.
func TestRingMapSelfReference(t *testing.T) {
	// root = the map itself
	m := map[string]any{}
	m["s"] = &m
	out := ringDecodeReg(t, m, (*map[string]any)(nil)).(map[string]any)
	pm := out["s"].(*map[string]any)
	if *pm == nil || len(*pm) != 1 {
		t.Fatalf("map root: bad self value %#v", out["s"])
	}
	if (*pm)["s"].(*map[string]any) != pm {
		t.Fatal("map root: ring open")
	}
	// root = the pointer to the map
	outP := ringDecodeReg(t, &m, (*map[string]any)(nil)).(*map[string]any)
	if (*outP)["s"].(*map[string]any) != outP {
		t.Fatal("ptr root: ring open")
	}
	// map root decoded into a typed map-pointer target
	b := mustMarshal(t, m)
	dec := gbon.NewDecoder(bytes.NewReader(b))
	if err := dec.Register((*map[string]any)(nil)); err != nil {
		t.Fatal(err)
	}
	var tp *map[string]any
	if err := dec.Decode(&tp); err != nil {
		t.Fatalf("typed ptr target: %v", err)
	}
	if tp == nil || len(*tp) != 1 {
		t.Fatalf("typed ptr target: bad root %#v", tp)
	}
	sm := (*tp)["s"].(*map[string]any)
	if (*sm)["s"].(*map[string]any) != sm {
		t.Fatal("typed ptr target: ring open")
	}
	// int keys carry the same self reference
	mi := map[int]any{}
	mi[1] = &mi
	outI := ringDecodeReg(t, mi, (*map[int]any)(nil)).(map[int]any)
	if (*outI[1].(*map[int]any)) == nil || len(*outI[1].(*map[int]any)) != 1 {
		t.Fatal("int key: bad self value")
	}
	// a ring of two maps closes on both sides
	i2 := map[string]any{}
	o := map[string]any{}
	o["i"] = &i2
	i2["o"] = &o
	out2 := ringDecodeReg(t, &o, (*map[string]any)(nil)).(*map[string]any)
	in := (*out2)["i"].(*map[string]any)
	if (*in)["o"].(*map[string]any) != out2 {
		t.Fatal("two-map ring open")
	}
	// a typed map-pointer slot needs no registry
	inner := map[string]any{"k": int64(3)}
	typed := map[string]*map[string]any{"a": &inner}
	outT := ringDecodePlain(t, typed).(map[string]*map[string]any)
	if (*outT["a"])["k"] != int64(3) {
		t.Fatal("typed slot: payload lost")
	}
}

// Concrete double-pointer paths close: a struct ring through **T, an
// interior field pointer without a ring, and a triple-pointer chain.
func TestRingConcreteDoublePointer(t *testing.T) {
	// base: b.A holds the address of the root cell
	a := &rgAA{V: 5}
	b := &rgBB{}
	a.B = b
	b.A = &a
	out := ringDecodeReg(t, a, rgAA{}, &rgAA{}, rgBB{}, &rgBB{}).(*rgAA)
	if out.V != 5 || out.B == nil {
		t.Fatalf("base: %#v", out)
	}
	if *(out.B.A) != out {
		t.Fatal("base: ring open")
	}
	// interior pointer to an int field, no ring
	h1 := &rgIH2{Q: 314}
	h2 := &rgIH2{P: &h1.Q}
	pair := []*rgIH2{h1, h2}
	outP := ringDecodeReg(t, pair, rgIH2{}, &rgIH2{}).([]*rgIH2)
	if outP[0].Q != 314 || outP[1].P == nil {
		t.Fatalf("interior: %#v", outP)
	}
	if *outP[1].P != 314 {
		t.Fatal("interior: value lost")
	}
	// triple pointer over the double-pointer cell
	a3 := &rgAA3{V: 9}
	pa := &a3
	b3 := &rgBB3{A: &pa}
	out3 := ringDecodeReg(t, b3, rgAA3{}, &rgAA3{}, rgBB3{}, &rgBB3{}).(*rgBB3)
	if out3.A == nil || *out3.A == nil || **out3.A == nil {
		t.Fatalf("triple: %#v", out3.A)
	}
	if (***out3.A).V != 9 {
		t.Fatal("triple: payload lost")
	}
}

// Anchored double-pointer values behind any slots decode: the base
// reject form, the depth-swapped form, the one-node self form, and the
// shared two-node form without a ring.
func TestRingAnyFieldAnchor(t *testing.T) {
	p := &rgPP{}
	q := &rgPP{}
	p.X = q
	q.Y = &p
	out := ringDecodeReg(t, p, rgPP{}, &rgPP{}, (**rgPP)(nil)).(*rgPP)
	qo := out.X.(*rgPP)
	if *(qo.Y.(**rgPP)) != out {
		t.Fatal("base: ring open")
	}
	// swapped depths: each ref matches its own cell grain
	s1 := &rgPP{}
	s2 := &rgPP{}
	s1.X = &s2
	s2.Y = s1
	outS := ringDecodeReg(t, s1, rgPP{}, &rgPP{}, (**rgPP)(nil)).(*rgPP)
	qs := outS.X.(**rgPP)
	if *qs == nil {
		t.Fatalf("swap: %#v", outS.X)
	}
	if (*qs).Y.(*rgPP) != outS {
		t.Fatal("swap: ring open")
	}
	// one node holding itself at both grains
	self := &rgPP{}
	self.X = self
	self.Y = &self
	outC := ringDecodeReg(t, self, rgPP{}, &rgPP{}, (**rgPP)(nil)).(*rgPP)
	if outC.X.(*rgPP) != outC || *(outC.Y.(**rgPP)) != outC {
		t.Fatal("self: ring open")
	}
	// shared node without a ring
	r := &rgPP{}
	w := &rgPP{}
	w.X = r
	w.Y = &r
	outD := ringDecodeReg(t, w, rgPP{}, &rgPP{}, (**rgPP)(nil)).(*rgPP)
	if outD.X.(*rgPP) != *(outD.Y.(**rgPP)) {
		t.Fatal("shared: identity lost")
	}
}

// Mixed concrete/any rings keep the any edge: the base family, its
// mirror, the two-node any ring, and the double-pointer payload under a
// complete registry.
func TestRingMixedAnyConcrete(t *testing.T) {
	// base: typed Next edge plus an any edge back to the root
	a := &rgAN{}
	b := &rgAN{}
	a.Next = b
	b.Box = a
	out := ringDecodeReg(t, a, rgAN{}, &rgAN{}).(*rgAN)
	if out.Next.Box.(*rgAN) != out {
		t.Fatal("base: any edge lost")
	}
	// mirror: any edge forward, typed edge back
	c := &rgAN{}
	d := &rgAN{}
	c.Box = d
	d.Next = c
	outM := ringDecodeReg(t, c, rgAN{}, &rgAN{}).(*rgAN)
	if outM.Box.(*rgAN).Next != outM {
		t.Fatal("mirror: typed edge lost")
	}
	// two-node ring carried entirely by any fields
	e := &rgAB{}
	f := &rgAB{}
	e.Box = f
	f.Box = e
	outB := ringDecodeReg(t, e, rgAB{}, &rgAB{}).(*rgAB)
	if outB.Box.(*rgAB).Box.(*rgAB) != outB {
		t.Fatal("any ring: open")
	}
	// double-pointer payload over the root under a complete registry
	g := &rgAN{}
	h := &rgAN{}
	g.Next = h
	h.Box = &g
	outG := ringDecodeReg(t, g, rgAN{}, &rgAN{}, (**rgAN)(nil)).(*rgAN)
	if *(outG.Next.Box.(**rgAN)) != outG {
		t.Fatal("double-pointer payload: ring open")
	}
}

// Homogeneous registered rings do not regress: self ring, two-node
// ring, diamond sharing, ring pairs, rings with external links, the
// figure-eight, and the len-4 ring.
func TestRingHomogeneousControl(t *testing.T) {
	// self ring
	n := &rgN{}
	n.Next = n
	out := ringDecodeReg(t, n, rgN{}, &rgN{}).(*rgN)
	if out.Next != out {
		t.Fatal("self ring open")
	}
	// two-node ring
	a := &rgN{}
	b := &rgN{}
	a.Next = b
	b.Next = a
	out2 := ringDecodeReg(t, a, rgN{}, &rgN{}).(*rgN)
	if out2.Next.Next != out2 {
		t.Fatal("two-node ring open")
	}
	// diamond: two fields share one node
	c := &rgN{}
	w := rgW{A: c, B: c}
	outW := ringDecodeReg(t, w, rgN{}, &rgN{}, rgW{}, &rgW{}).(rgW)
	if outW.A.(*rgN) != outW.B.(*rgN) {
		t.Fatal("diamond: identity lost")
	}
	// container over a two-node ring
	w6 := rgW{A: a, B: b}
	out6 := ringDecodeReg(t, w6, rgN{}, &rgN{}, rgW{}, &rgW{}).(rgW)
	pa := out6.A.(*rgN)
	pb := out6.B.(*rgN)
	if pa.Next != pb || pb.Next != pa {
		t.Fatal("ring pair: open")
	}
	// len-3 ring with external links
	r1, r2, r3 := &rgN{}, &rgN{}, &rgN{}
	r1.Next, r2.Next, r3.Next = r2, r3, r1
	w16 := rgW{A: r1, B: r3}
	out16 := ringDecodeReg(t, w16, rgN{}, &rgN{}, rgW{}, &rgW{}).(rgW)
	if out16.A.(*rgN).Next.Next.Next != out16.A.(*rgN) {
		t.Fatal("len-3 ring open")
	}
	if out16.A.(*rgN).Next.Next != out16.B.(*rgN) {
		t.Fatal("external link lost")
	}
	// figure-eight: two rings on one node
	fe := &rgN2{}
	l := &rgN2{}
	r := &rgN2{}
	fe.A, l.A = l, fe
	fe.B, r.B = r, fe
	outF := ringDecodeReg(t, fe, rgN2{}, &rgN2{}).(*rgN2)
	if outF.A.A != outF || outF.B.B != outF {
		t.Fatal("figure-eight open")
	}
	// len-4 ring with external links
	q1, q2, q3, q4 := &rgN{}, &rgN{}, &rgN{}, &rgN{}
	q1.Next, q2.Next, q3.Next, q4.Next = q2, q3, q4, q1
	w24 := rgW{A: q1, B: q4}
	out24 := ringDecodeReg(t, w24, rgN{}, &rgN{}, rgW{}, &rgW{}).(rgW)
	if out24.A.(*rgN).Next.Next.Next != out24.B.(*rgN) {
		t.Fatal("len-4 external link lost")
	}
	if out24.B.(*rgN).Next != out24.A.(*rgN) {
		t.Fatal("len-4 ring open")
	}
}

// Any-only classes decode through the stateless entry point: the
// derived grammar returns for unnamed pointer chains and their
// composite wrappers.
func TestRingPlainDeriveModality(t *testing.T) {
	// self interface value
	var x any
	x = &x
	out := ringDecodePlain(t, x)
	p := out.(*any)
	if *p != any(p) {
		t.Fatal("self interface value: ring open")
	}
	// three-slot ring of single pointers
	var s1, s2, s3 any
	s1 = &s2
	s2 = &s3
	s3 = &s1
	o3 := ringDecodePlain(t, s1)
	p1 := o3.(*any)
	p2 := (*p1).(*any)
	p3 := (*p2).(*any)
	if *p3 != any(p1) {
		t.Fatal("three-slot ring open")
	}
	// len-4 ring, both slots holding double pointers
	root8, _ := ringAnyRing([]int{2, 1, 2, 1})
	o8 := ringDecodePlain(t, root8.Interface())
	ringWalkAny(t, o8, []int{2, 1, 2, 1})
	// len-3 ring closing over a triple pointer
	root9, _ := ringAnyRing([]int{1, 1, 3})
	o9 := ringDecodePlain(t, root9.Interface())
	ringWalkAny(t, o9, []int{1, 1, 3})
	// box: slice holding a slice whose element points back at the box
	var box []any
	s := []any{nil}
	s[0] = &box
	box = []any{s}
	oB := ringDecodeReg(t, box, (*[]any)(nil)).([]any)
	inner := oB[0].([]any)
	pb := inner[0].(*[]any)
	if pb == nil || *pb == nil || len(*pb) != 1 {
		t.Fatalf("box: bad target %#v", pb)
	}
	if &(*pb)[0] != &oB[0] {
		t.Fatal("box ring open")
	}
	// len-6 ring with alternating grains
	grains23 := []int{1, 2, 1, 2, 1, 2}
	root23, _ := ringAnyRing(grains23)
	o23 := ringDecodePlain(t, root23.Interface())
	ringWalkAny(t, o23, grains23)
	// anchor loop: any slot holds the pointer to its own pointer cell
	var ax any
	ap := &ax
	ax = &ap
	oA := ringDecodePlain(t, ax)
	qa := oA.(**any)
	if *qa == nil {
		t.Fatalf("anchor loop: nil link")
	}
	if (**qa).(**any) != qa {
		t.Fatal("anchor loop: ring open")
	}
}

// Registered entry points without a ring in the root decode: slice
// elements, map values, the slice self reference (contrast to the map
// self reference), and the any-field self reference.
func TestRingSupportedEntryPoints(t *testing.T) {
	// slice element
	n := &rgN{}
	n.Next = n
	sl := []*rgN{n}
	out := ringDecodeReg(t, sl, rgN{}, &rgN{}, []*rgN{}).([]*rgN)
	if out[0].Next != out[0] {
		t.Fatal("slice element: ring open")
	}
	// map value
	mv := map[string]*rgN{"a": n}
	outM := ringDecodeReg(t, mv, rgN{}, &rgN{}, map[string]*rgN{}).(map[string]*rgN)
	if outM["a"].Next != outM["a"] {
		t.Fatal("map value: ring open")
	}
	// slice self reference
	ss := []any{nil}
	ss[0] = &ss
	outS := ringDecodeReg(t, ss, (*[]any)(nil)).([]any)
	ps := outS[0].(*[]any)
	if *ps == nil || len(*ps) != 1 {
		t.Fatalf("slice self: bad target")
	}
	if (*ps)[0].(*[]any) != ps {
		t.Fatal("slice self: ring open")
	}
	// any-field self reference
	ab := &rgAB{}
	ab.Box = ab
	outA := ringDecodeReg(t, ab, rgAB{}, &rgAB{}).(*rgAB)
	if outA.Box.(*rgAB) != outA {
		t.Fatal("any-field self: ring open")
	}
}

// The encoder output for every taxonomy class is stable: two encoders
// agree byte-for-byte and decoded graphs re-encode identically; the two
// anchor chain streams are pinned as literals.
func TestRingEncodedBytesStable(t *testing.T) {
	build := []func() (any, []any){
		func() (any, []any) { n := &rgN{}; n.Next = n; return n, []any{rgN{}, &rgN{}} },
		func() (any, []any) { var x any; x = &x; return x, nil },
		func() (any, []any) {
			a, b := &rgN{}, &rgN{}
			a.Next, b.Next = b, a
			return a, []any{rgN{}, &rgN{}}
		},
		func() (any, []any) { r, _ := ringAnyRing([]int{1, 1, 1}); return r.Interface(), nil },
		func() (any, []any) {
			c := &rgN{}
			w := rgW{A: c, B: c}
			return w, []any{rgN{}, &rgN{}, rgW{}, &rgW{}}
		},
		func() (any, []any) {
			a, b := &rgN{}, &rgN{}
			a.Next, b.Next = b, a
			return rgW{A: a, B: b}, []any{rgN{}, &rgN{}, rgW{}, &rgW{}}
		},
		func() (any, []any) { r, _ := ringAnyRing([]int{1, 1}); return r.Addr().Interface(), nil },
		func() (any, []any) { r, _ := ringAnyRing([]int{2, 1, 2, 1}); return r.Interface(), nil },
		func() (any, []any) { r, _ := ringAnyRing([]int{1, 1, 3}); return r.Interface(), nil },
		func() (any, []any) {
			var d1 any
			d2 := &d1
			return []any{d2, &d2}, nil
		},
		func() (any, []any) {
			n := &rgN{}
			n.Next = n
			return []*rgN{n}, []any{rgN{}, &rgN{}, []*rgN{}}
		},
		func() (any, []any) {
			n := &rgN{}
			n.Next = n
			return map[string]*rgN{"a": n}, []any{rgN{}, &rgN{}, map[string]*rgN{}}
		},
		func() (any, []any) {
			var box []any
			s := []any{nil}
			s[0] = &box
			box = []any{s}
			return box, []any{(*[]any)(nil)}
		},
		func() (any, []any) {
			m := map[string]any{}
			m["s"] = &m
			return m, []any{(*map[string]any)(nil)}
		},
		func() (any, []any) {
			s := []any{nil}
			s[0] = &s
			return s, []any{(*[]any)(nil)}
		},
		func() (any, []any) {
			a, b, c := &rgN{}, &rgN{}, &rgN{}
			a.Next, b.Next, c.Next = b, c, a
			return rgW{A: a, B: c}, []any{rgN{}, &rgN{}, rgW{}, &rgW{}}
		},
		func() (any, []any) { ab := &rgAB{}; ab.Box = ab; return ab, []any{rgAB{}, &rgAB{}} },
		func() (any, []any) {
			a, b := &rgAB{}, &rgAB{}
			a.Box, b.Box = b, a
			return a, []any{rgAB{}, &rgAB{}}
		},
		func() (any, []any) {
			a, b := &rgAN{}, &rgAN{}
			a.Next = b
			b.Box = a
			return a, []any{rgAN{}, &rgAN{}}
		},
		func() (any, []any) {
			a := &rgAA{V: 5}
			b := &rgBB{}
			a.B = b
			b.A = &a
			return a, []any{rgAA{}, &rgAA{}, rgBB{}, &rgBB{}}
		},
		func() (any, []any) {
			p, q := &rgPP{}, &rgPP{}
			p.X = q
			q.Y = &p
			return p, []any{rgPP{}, &rgPP{}, (**rgPP)(nil)}
		},
		func() (any, []any) {
			fe, l, r := &rgN2{}, &rgN2{}, &rgN2{}
			fe.A, l.A = l, fe
			fe.B, r.B = r, fe
			return fe, []any{rgN2{}, &rgN2{}}
		},
		func() (any, []any) { r, _ := ringAnyRing([]int{1, 2, 1, 2, 1, 2}); return r.Interface(), nil },
		func() (any, []any) {
			q1, q2, q3, q4 := &rgN{}, &rgN{}, &rgN{}, &rgN{}
			q1.Next, q2.Next, q3.Next, q4.Next = q2, q3, q4, q1
			return rgW{A: q1, B: q4}, []any{rgN{}, &rgN{}, rgW{}, &rgW{}}
		},
		func() (any, []any) { r, _ := ringAnyRing([]int{2, 1}); return r.Interface(), nil },
	}
	if len(build) != 25 {
		t.Fatalf("taxonomy table: %d classes, want 25", len(build))
	}
	for i, mk := range build {
		src, examples := mk()
		b1 := mustMarshal(t, src)
		b2 := mustMarshal(t, src)
		if !bytes.Equal(b1, b2) {
			t.Fatalf("class %d: two encoders disagree", i)
		}
		out := reflect.New(reflect.TypeOf(src))
		if examples == nil {
			if err := gbon.Unmarshal(b1, out.Interface()); err != nil {
				t.Fatalf("class %d: plain decode: %v", i, err)
			}
		} else {
			dec := gbon.NewDecoder(bytes.NewReader(b1))
			if err := dec.Register(examples...); err != nil {
				t.Fatalf("class %d: register: %v", i, err)
			}
			if err := dec.Decode(out.Interface()); err != nil {
				t.Fatalf("class %d: decode: %v", i, err)
			}
		}
		ringAssertBytes(t, src, out.Elem().Interface(), b1)
	}
	// anchor chain streams from the untouched encoder, pinned literally
	var v3x any
	u1 := &v3x
	u2 := &u1
	v3x = &u2
	pin3 := "67626f6e0000d56c0f2a2a2a696e74657266616365207b7d" +
		"d56c0e2a2a696e74657266616365207b7dd56c0d2a696e74657266616365207b7d" +
		"d66c0c696e74657266616365207b7dc0c8"
	if got := fmt.Sprintf("%x", mustMarshal(t, v3x)); got != pin3 {
		t.Fatalf("depth-3 anchor: %s", got)
	}
	var w1x any
	w2x := &w1x
	w1x = &w2x
	pin2 := "67626f6e0000d56c0e2a2a696e74657266616365207b7d" +
		"d56c0d2a696e74657266616365207b7dd66c0c696e74657266616365207b7dc0c6"
	if got := fmt.Sprintf("%x", mustMarshal(t, w1x)); got != pin2 {
		t.Fatalf("depth-2 anchor: %s", got)
	}
}

// Negative outcomes keep their error classes: named-miss, incomplete
// registry at depth, depth budget, doubled body, and out-of-stream ref
// id — all loud, none a panic.
func TestRingNegativeOutcomes(t *testing.T) {
	// named payload outside the registry, registry active
	nw := rgW{A: &rgN{}}
	nb := mustMarshal(t, nw)
	var nOut rgW
	decN := gbon.NewDecoder(bytes.NewReader(nb))
	if err := decN.Register(rgW{}); err != nil {
		t.Fatal(err)
	}
	err := decN.Decode(&nOut)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("named miss: %v", err)
	}
	var ge *gbon.Error
	if !errors.As(err, &ge) || ge.Class() != "unknown_name" {
		t.Fatalf("named miss: class %v", err)
	}
	if !strings.Contains(err.Error(), "use Decoder.Register") {
		t.Fatalf("named miss: hint text %q", err.Error())
	}

	// incomplete registry: the double-pointer payload misses at depth
	a := &rgAN{}
	b := &rgAN{}
	a.Next = b
	b.Box = &a
	ib := mustMarshal(t, a)
	dec := gbon.NewDecoder(bytes.NewReader(ib))
	if err := dec.Register(rgAN{}, &rgAN{}); err != nil {
		t.Fatal(err)
	}
	var iOut *rgAN
	err = dec.Decode(&iOut)
	if !errors.As(err, &ge) || ge.Class() != "unknown_name" {
		t.Fatalf("incomplete registry: %v", err)
	}
	if !strings.Contains(err.Error(), "$.Next.Box") {
		t.Fatalf("incomplete registry: path %q", err.Error())
	}

	// depth budget over a deep ring stays loud
	deep, _ := ringAnyRing(make([]int, 64))
	db := mustMarshal(t, deep.Interface())
	decD := gbon.NewDecoder(bytes.NewReader(db))
	decD.SetLimits(gbon.Limits{MaxDepth: 2, MaxNodes: 1 << 20, MaxBytes: 1 << 20})
	var dOut any
	err = decD.Decode(&dOut)
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("depth budget: %v", err)
	}
	if !errors.As(err, &ge) || ge.Class() != "budget_depth" {
		t.Fatalf("depth budget: class %v", err)
	}

	// a record body repeated in the stream fails the next read loudly
	c := newCraft()
	c.descPos(dInt64)
	c.intTok(7)
	c.intTok(8)
	decB := gbon.NewDecoder(bytes.NewReader(c.buf))
	var i64 int64
	if err := decB.Decode(&i64); err != nil {
		t.Fatalf("doubled body: value 1: %v", err)
	}
	if err := decB.Decode(&i64); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("doubled body: %v", err)
	}

	// ref id far beyond the stream: format error, not a panic
	c2 := newCraft()
	c2.descPos(dPtr(dIface))
	c2.refUnregistered(1 << 30)
	var pOut *any
	err = gbon.NewDecoder(bytes.NewReader(c2.buf)).Decode(&pOut)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("out-of-stream ref: %v", err)
	}
	if !errors.As(err, &ge) || ge.Class() != "bad_ref" {
		t.Fatalf("out-of-stream ref: class %v", err)
	}

	// cell resolution against an incompatible slot stays bad_ref
	c3 := newCraft()
	c3.descPos(dPtr(dInt64))
	rec := c3.ptrRec()
	c3.intTok(7)
	c3.descPos(dPtr(dPtr(dIface)))
	c3.refTok(rec)
	dec3 := gbon.NewDecoder(bytes.NewReader(c3.buf))
	var v1 *int64
	if err := dec3.Decode(&v1); err != nil {
		t.Fatalf("cell mismatch: value 1: %v", err)
	}
	var v2 **any
	err = dec3.Decode(&v2)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("cell mismatch: %v", err)
	}
	if !errors.As(err, &ge) || ge.Class() != "bad_ref" {
		t.Fatalf("cell mismatch: class %v", err)
	}
}

// ringGraphIsomorphic asserts decoded-graph equivalence to the source:
// Next edges map one-to-one and Box payloads agree in dynamic type and
// value, with pointer payloads consistent under the node mapping.
func ringGraphIsomorphic(t *testing.T, label string, src, dst *rgAN) {
	t.Helper()
	seen := make(map[*rgAN]*rgAN)
	var visit func(a, b *rgAN)
	visit = func(a, b *rgAN) {
		if got, ok := seen[a]; ok {
			if got != b {
				t.Fatalf("%s: node %p maps to both %p and %p", label, a, got, b)
			}
			return
		}
		seen[a] = b
		visit(a.Next, b.Next)
		if (a.Box == nil) != (b.Box == nil) {
			t.Fatalf("%s: box nil-ness differs at %p", label, a)
		}
		if a.Box == nil {
			return
		}
		if reflect.TypeOf(a.Box) != reflect.TypeOf(b.Box) {
			t.Fatalf("%s: box type %T vs %T", label, a.Box, b.Box)
		}
		switch av := a.Box.(type) {
		case int64:
			if bv := b.Box.(int64); bv != av {
				t.Fatalf("%s: box int %d vs %d", label, av, bv)
			}
		case *rgAN:
			visit(av, b.Box.(*rgAN))
		default:
			pv := reflect.ValueOf(a.Box)
			qv := reflect.ValueOf(b.Box)
			for pv.Kind() == reflect.Pointer && pv.Type().Elem().Kind() == reflect.Pointer {
				if pv.IsNil() || qv.IsNil() {
					if pv.IsNil() != qv.IsNil() {
						t.Fatalf("%s: box chain nil-ness differs", label)
					}
					return
				}
				pv, qv = pv.Elem(), qv.Elem()
			}
			if pv.Kind() == reflect.Pointer && !pv.IsNil() &&
				pv.Type().Elem().Kind() == reflect.Interface {
				sa, sb := pv.Elem(), qv.Elem()
				if sa.IsNil() != sb.IsNil() {
					t.Fatalf("%s: box slot nil-ness differs", label)
				}
				if !sa.IsNil() && sa.Elem().Type() != sb.Elem().Type() {
					t.Fatalf("%s: box slot type %v vs %v", label, sa.Elem().Type(), sb.Elem().Type())
				}
			}
		}
	}
	visit(src, dst)
	if len(seen) == 0 {
		t.Fatalf("%s: empty graph", label)
	}
}

// Property: generated mixed concrete/any rings round-trip exactly —
// decode succeeds with type identity, graph isomorphism, and
// byte-identical re-encode; two independent encoders agree.
func TestRingPropertyRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260910))
	for iter := range 60 {
		l := 1 + rnd.Intn(6)
		nodes := make([]*rgAN, l)
		for i := range nodes {
			nodes[i] = &rgAN{}
		}
		for i := range nodes {
			nodes[i].Next = nodes[(i+1)%l]
		}
		for i := range nodes {
			switch rnd.Intn(5) {
			case 0:
			case 1:
				nodes[i].Box = int64(rnd.Int63())
			case 2:
				nodes[i].Box = nodes[rnd.Intn(l)]
			case 3:
				nodes[i].Box = &nodes[rnd.Intn(l)].Box
			case 4:
				var slot any
				slot = int64(rnd.Int63())
				curv := reflect.ValueOf(&slot)
				for g := 1 + rnd.Intn(3); g > 1; g-- {
					cell := reflect.New(curv.Type()).Elem()
					cell.Set(curv)
					curv = cell.Addr()
				}
				nodes[i].Box = curv.Interface()
			}
		}
		src := nodes[0]
		b1 := mustMarshal(t, src)
		b2 := mustMarshal(t, src)
		if !bytes.Equal(b1, b2) {
			t.Fatalf("iter %d: encoders disagree", iter)
		}
		dec := gbon.NewDecoder(bytes.NewReader(b1))
		if err := dec.Register(rgAN{}, &rgAN{}); err != nil {
			t.Fatal(err)
		}
		out := new(*rgAN)
		if err := dec.Decode(out); err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		ringGraphIsomorphic(t, fmt.Sprintf("iter %d", iter), src, *out)
		ringAssertBytes(t, src, *out, b1)
	}
}

// Property: per root modality a same-form wire round-trips — any, and
// the deeper pointer roots the ring's grain admits; cross-modality
// targets are covered by TestRingPropertyRootCrossModality.
func TestRingPropertyRootModality(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260911))
	for iter := range 40 {
		l := 2 + rnd.Intn(3)
		grains := make([]int, l)
		g0 := 1
		for i := range grains {
			grains[i] = 1 + rnd.Intn(2)
		}
		g0 = grains[0]
		root, _ := ringAnyRing(grains)
		b := mustMarshal(t, root.Interface())
		var oAny any
		if err := gbon.Unmarshal(b, &oAny); err != nil {
			t.Fatalf("iter %d: any root: %v", iter, err)
		}
		ringWalkAny(t, oAny, grains)
		ringAssertBytes(t, root.Interface(), oAny, b)
		if g0 >= 1 {
			pv := root.Addr()
			bp := mustMarshal(t, pv.Interface())
			var oP *any
			if err := gbon.Unmarshal(bp, &oP); err != nil {
				t.Fatalf("iter %d: ptr root: %v", iter, err)
			}
			ringAssertBytes(t, pv.Interface(), oP, bp)
			if g0 >= 2 {
				pp := pv
				cell := reflect.New(pp.Type()).Elem()
				cell.Set(pp)
				ppv := cell.Addr().Interface()
				bpp := mustMarshal(t, ppv)
				var oPP **any
				if err := gbon.Unmarshal(bpp, &oPP); err != nil {
					t.Fatalf("iter %d: double root: %v", iter, err)
				}
				ringAssertBytes(t, ppv, oPP, bpp)
			}
		}
	}
}

// Property: one any-slot ring wire decodes across root modalities with
// equivalent topology and a stable target-modality wire; a pointer
// target shallower than the wire's root fails loud (type_mismatch).
func TestRingPropertyRootCrossModality(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260913))
	for iter := range 40 {
		l := 2 + rnd.Intn(3)
		grains := make([]int, l)
		for i := range grains {
			grains[i] = 1 + rnd.Intn(2)
		}
		g0 := grains[0]
		rot := append(append([]int{}, grains[1:]...), grains[0])
		root, _ := ringAnyRing(grains)
		b := mustMarshal(t, root.Interface())
		pv := root.Addr()
		bp := mustMarshal(t, pv.Interface())
		cell := reflect.New(pv.Type()).Elem()
		cell.Set(pv)
		bpp := mustMarshal(t, cell.Addr().Interface())

		// pointer wire, any target: the any slot holds the root pointer;
		// one deref lands slot 0; re-encode reproduces the pointer wire.
		var oFlat any
		if err := gbon.Unmarshal(bp, &oFlat); err != nil {
			t.Fatalf("iter %d: ptr wire, any target: %v", iter, err)
		}
		rp, ok := oFlat.(*any)
		if !ok || rp == nil {
			t.Fatalf("iter %d: ptr wire, any target: root %T", iter, oFlat)
		}
		ringWalkAny(t, *rp, grains)
		ringAssertBytes(t, oFlat, oFlat, bp)

		// double-pointer wire, any target: two derefs land slot 0;
		// re-encode reproduces the double-pointer wire.
		var oWide any
		if err := gbon.Unmarshal(bpp, &oWide); err != nil {
			t.Fatalf("iter %d: pp wire, any target: %v", iter, err)
		}
		rpp, ok := oWide.(**any)
		if !ok || rpp == nil {
			t.Fatalf("iter %d: pp wire, any target: root %T", iter, oWide)
		}
		ringWalkAny(t, **rpp, grains)
		ringAssertBytes(t, oWide, oWide, bpp)

		// any wire, double-pointer target: the wire's grain chain heads
		// the root; the walk enters at slot 1 with rotated grains. A
		// grain-two chain carries the root itself, so bytes return to the
		// source wire; a synthesized level re-encodes to its own stable
		// wire that round-trips again.
		var oDbl **any
		if err := gbon.Unmarshal(b, &oDbl); err != nil {
			t.Fatalf("iter %d: any wire, ** target: %v", iter, err)
		}
		if oDbl == nil || *oDbl == nil {
			t.Fatalf("iter %d: any wire, ** target: nil root", iter)
		}
		ringWalkAny(t, *(*oDbl), rot)
		if g0 == 2 {
			ringAssertBytes(t, oDbl, oDbl, b)
		} else {
			re := mustMarshal(t, oDbl)
			var oAgain **any
			if err := gbon.Unmarshal(re, &oAgain); err != nil {
				t.Fatalf("iter %d: synthesized root re-decode: %v", iter, err)
			}
			ringWalkAny(t, *(*oAgain), rot)
			ringAssertBytes(t, oAgain, oAgain, re)
		}

		// any wire, pointer target: grain one is the pointer form;
		// deeper grains sit outside the pointer root and mismatch loud.
		var oPtr *any
		if g0 == 1 {
			if err := gbon.Unmarshal(b, &oPtr); err != nil {
				t.Fatalf("iter %d: any wire, ptr target: %v", iter, err)
			}
			if oPtr == nil {
				t.Fatalf("iter %d: any wire, ptr target: nil root", iter)
			}
			ringWalkAny(t, *oPtr, rot)
			ringAssertBytes(t, oPtr, oPtr, b)
		} else {
			err := gbon.Unmarshal(b, &oPtr)
			var ge *gbon.Error
			if !errors.As(err, &ge) || ge.Class() != "type_mismatch" || ge.Offset != 54 {
				t.Fatalf("iter %d: any wire, ptr target: %v, want type_mismatch at offset 54", iter, err)
			}
		}

		// double-pointer wire, pointer target: always outside the root
		err := gbon.Unmarshal(bpp, &oPtr)
		var ge *gbon.Error
		if !errors.As(err, &ge) || ge.Class() != "type_mismatch" || ge.Offset != 54 {
			t.Fatalf("iter %d: pp wire, ptr target: %v, want type_mismatch at offset 54", iter, err)
		}
	}
}

// Property: permuting the first inline definition and the back ref to
// a shared cell keeps the decoded topology — shared identity survives
// both orders across slice, map, and struct entry points.
func TestRingPropertyOrderIndependence(t *testing.T) {
	rnd := rand.New(rand.NewSource(20260912))
	for iter := range 40 {
		var slot any = int64(rnd.Int63())
		first := &slot
		var second **any = &first
		var srcF, srcR any
		switch rnd.Intn(3) {
		case 0:
			srcF, srcR = []any{first, second}, []any{second, first}
		case 1:
			srcF, srcR = map[string]any{"a": first, "b": second}, map[string]any{"a": second, "b": first}
		case 2:
			srcF, srcR = rgW{A: first, B: second}, rgW{A: second, B: first}
		}
		bF := mustMarshal(t, srcF)
		bR := mustMarshal(t, srcR)
		decodeSide := func(b []byte, out any) {
			dec := gbon.NewDecoder(bytes.NewReader(b))
			if err := dec.Register(rgW{}, &rgW{}); err != nil {
				t.Fatal(err)
			}
			if err := dec.Decode(out); err != nil {
				t.Fatalf("iter %d: %v", iter, err)
			}
		}
		var oF, oR any
		decodeSide(bF, &oF)
		decodeSide(bR, &oR)
		ringAssertBytes(t, srcF, oF, bF)
		ringAssertBytes(t, srcR, oR, bR)
		extract := func(v any) (*any, **any) {
			var s1, s2 any
			switch c := v.(type) {
			case []any:
				s1, s2 = c[0], c[1]
			case map[string]any:
				s1, s2 = c["a"], c["b"]
			case rgW:
				s1, s2 = c.A, c.B
			}
			if p, ok := s1.(*any); ok {
				return p, s2.(**any)
			}
			return s2.(*any), s1.(**any)
		}
		pF, qF := extract(oF)
		if *qF != pF {
			t.Fatalf("iter %d: forward shared identity lost", iter)
		}
		pR, qR := extract(oR)
		if *qR != pR {
			t.Fatalf("iter %d: reverse shared identity lost", iter)
		}
	}
}
