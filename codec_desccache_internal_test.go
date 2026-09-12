package gbon

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

// The type plan is built once per (type, coder-scope epoch) in the
// global cache. The six struct occurrences below (2 outer + 4 inner)
// across three separate streams must leave two plan entries, not six.
func TestStructDescOncePerStreamType(t *testing.T) {
	type inner struct{ A int }
	type outer struct {
		X inner
		Y inner
		Z int
	}
	it := reflect.TypeFor[inner]()
	ot := reflect.TypeFor[outer]()
	vals := []any{
		[]outer{{X: inner{A: 1}}, {Y: inner{A: 2}}},
		[]outer{{X: inner{A: 3}}},
		outer{Z: 9},
	}
	for _, v := range vals {
		if _, err := Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	planRegistry.Lock()
	defer planRegistry.Unlock()
	n := 0
	for k := range planRegistry.m {
		if k.ep == 0 && (k.t == it || k.t == ot) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("plan cache entries for the pair: got %d, want 2", n)
	}
}

// A recursive type closes its plan graph on shared nodes: the pointer
// node's element plan is the struct's own plan, built exactly once, and
// the struct's scan fields reference that same pointer plan.
func TestPlanGraphSharedNodesRecursive(t *testing.T) {
	type chain struct {
		V    int64
		Next *chain
	}
	ct := reflect.TypeFor[chain]()
	if _, err := Marshal(chain{V: 1, Next: &chain{V: 2}}); err != nil {
		t.Fatal(err)
	}
	planRegistry.Lock()
	defer planRegistry.Unlock()
	cp, ok := planRegistry.m[planKey{t: ct, ep: 0}]
	if !ok {
		t.Fatal("chain plan missing from the global cache")
	}
	var ptrPlan *typePlan
	for i := range cp.scanFields {
		if cp.scanFields[i].plan.op == opPointer {
			ptrPlan = cp.scanFields[i].plan
		}
	}
	if ptrPlan == nil {
		t.Fatal("chain plan has no pointer field plan")
	}
	if ptrPlan.elem != cp {
		t.Fatal("pointer element plan is not the shared chain plan node")
	}
}

// The flat-plan scenarios below resolve descriptors through the
// package's own walk (descOf), keeping the test suite free of
// internal/wire imports. Each descOf call builds a fresh descriptor
// instance; the walk memo inside one call closes recursive types on a
// single instance, which is the intern-canonical shape the stream
// decoder presents to structPlanFor.

// The recursive-chain workload: one descriptor instance visited once
// per node, with the pointer field classifying every visit as composite
// staging. A repeat visit must not allocate and must not resolve fields
// again — the composite entry is cached like a plan.
func TestFlatLayoutCompositePlanCachedOnRepeatVisit(t *testing.T) {
	type chain struct {
		V    int64
		Next *chain
	}
	desc, err := descOf(reflect.TypeFor[chain](), "")
	if err != nil {
		t.Fatal(err)
	}
	ct := reflect.TypeFor[chain]()
	d := &codecDecoder{}
	pl := d.structPlanFor(desc, ct)
	if pl == nil || pl.allPrim {
		t.Fatalf("pointer-field plan: got %+v, want a composite plan", pl)
	}
	if got := len(d.caches().plans); got != 1 {
		t.Fatalf("cache entries after first visit: got %d, want 1", got)
	}
	var again *decStructPlan
	allocs := testing.AllocsPerRun(100, func() {
		again = d.structPlanFor(desc, ct)
	})
	if again != pl {
		t.Fatal("repeat composite visit: cached plan pointer changed")
	}
	if allocs != 0 {
		t.Fatalf("repeat composite visit allocations: got %v, want 0", allocs)
	}
}

// A fully flat plan resolves to the exact staging table for the target
// (SB-2), and a repeat visit returns the identical cached entry
// (pointer-equal, allocation-free) — the decode-side analog of
// TestStructDescOncePerStreamType. An empty struct yields an empty
// non-nil plan with allPrim set.
func TestFlatLayoutPlanTable(t *testing.T) {
	type sample struct {
		A int32
		B string
		C bool
		D float32
		E float64
		F uint64
	}
	desc, err := descOf(reflect.TypeFor[sample](), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []decFieldPlan{
		{name: "A", idx: []int{0}, prim: fkInt, ft: reflect.TypeFor[int32]()},
		{name: "B", idx: []int{1}, prim: fkString, ft: reflect.TypeFor[string]()},
		{name: "C", idx: []int{2}, prim: fkBool, ft: reflect.TypeOf(false)},
		{name: "D", idx: []int{3}, prim: fkF32, ft: reflect.TypeFor[float32]()},
		{name: "E", idx: []int{4}, prim: fkF64, ft: reflect.TypeFor[float64]()},
		{name: "F", idx: []int{5}, prim: fkUint, ft: reflect.TypeFor[uint64]()},
	}
	st := reflect.TypeFor[sample]()
	d := &codecDecoder{}
	fl := d.structPlanFor(desc, st)
	if fl == nil || !fl.allPrim {
		t.Fatal("primitive-only plan: got nil or non-flat")
	}
	if fl.desc != desc {
		t.Fatal("cached plan descriptor: instance mismatch")
	}
	if len(fl.fields) != len(want) {
		t.Fatalf("plan fields: got %d, want %d", len(fl.fields), len(want))
	}
	for i, ff := range fl.fields {
		w := want[i]
		if ff.prim != w.prim || ff.ft != w.ft || ff.name != w.name || len(ff.idx) != len(w.idx) {
			t.Fatalf("plan field %d: got {%v %v %v %v}, want {%v %v %v %v}", i, ff.idx, ff.prim, ff.ft, ff.name, w.idx, w.prim, w.ft, w.name)
		}
		for j := range ff.idx {
			if ff.idx[j] != w.idx[j] {
				t.Fatalf("plan field %d idx: got %v, want %v", i, ff.idx, w.idx)
			}
		}
	}
	var again *decStructPlan
	allocs := testing.AllocsPerRun(100, func() {
		again = d.structPlanFor(desc, st)
	})
	if again != fl {
		t.Fatal("repeat visit: cached plan pointer changed")
	}
	if allocs != 0 {
		t.Fatalf("repeat plan visit allocations: got %v, want 0", allocs)
	}

	type empty struct{}
	edesc, err := descOf(reflect.TypeFor[empty](), "")
	if err != nil {
		t.Fatal(err)
	}
	efl := d.structPlanFor(edesc, reflect.TypeFor[empty]())
	if efl == nil || !efl.allPrim {
		t.Fatal("empty-struct plan: got nil or non-flat, want an empty flat plan")
	}
	if efl.fields == nil || len(efl.fields) != 0 {
		t.Fatalf("empty-struct plan fields: got %v, want empty non-nil", efl.fields)
	}
}

// The enumerated non-flat causes cache their composite entry: a named
// wrapper is the walk image of a named primitive field; a missing
// target field is a superset descriptor against a subset target; a
// composite field kind is any non-primitive field (slice here — the
// pointer variant is the chain case above). The unexported-field half
// of the missing/unsettable check is unreachable through the walk (it
// rejects unexported fields), and shares the missing-field branch it
// fails with.
func TestFlatLayoutNonFlatCausesCached(t *testing.T) {
	type myInt int64
	type named struct{ M myInt }
	type subset struct{ A int64 }
	type superset struct {
		A int64
		Z int64
	}
	type withSlice struct {
		A int64
		B []int64
	}
	cases := []struct {
		label    string
		target   reflect.Type
		source   reflect.Type
		skipSlot int // desc field index resolving to the skip path (-1: none)
	}{
		{"named wrapper", reflect.TypeFor[named](), reflect.TypeFor[named](), -1},
		{"missing target field", reflect.TypeFor[subset](), reflect.TypeFor[superset](), 1},
		{"composite field kind", reflect.TypeFor[withSlice](), reflect.TypeFor[withSlice](), -1},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			desc, err := descOf(tc.source, "")
			if err != nil {
				t.Fatal(err)
			}
			d := &codecDecoder{}
			pl := d.structPlanFor(desc, tc.target)
			if pl == nil || pl.allPrim {
				t.Fatalf("first visit: got %+v, want a cached composite plan", pl)
			}
			if got := len(d.caches().plans); got != 1 {
				t.Fatalf("cache entries after non-flat cause: got %d, want 1", got)
			}
			if tc.skipSlot >= 0 && pl.fields[tc.skipSlot].idx != nil {
				t.Fatalf("skip slot %d: got idx %v, want nil", tc.skipSlot, pl.fields[tc.skipSlot].idx)
			}
			var again *decStructPlan
			allocs := testing.AllocsPerRun(100, func() {
				again = d.structPlanFor(desc, tc.target)
			})
			if again != pl {
				t.Fatalf("repeat visit: cached plan pointer changed")
			}
			if allocs != 0 {
				t.Fatalf("repeat visit allocations: got %v, want 0", allocs)
			}
		})
	}
}

// Wide layouts resolve a flat plan like any other: the staging lives on
// the shared scratch, so no per-field stack bound applies; the repeat
// visit is a cache hit and stays allocation-free.
func TestFlatLayoutWidePlan(t *testing.T) {
	type wide struct {
		F00, F01, F02, F03, F04, F05, F06, F07 int64
		F08, F09, F10, F11, F12, F13, F14, F15 int64
		F16                                    int64
	}
	desc, err := descOf(reflect.TypeFor[wide](), "")
	if err != nil {
		t.Fatal(err)
	}
	st := reflect.TypeFor[wide]()
	if len(desc.Fields) != 17 {
		t.Fatalf("wide fixture fields: got %d, want 17", len(desc.Fields))
	}
	d := &codecDecoder{}
	pl := d.structPlanFor(desc, st)
	if pl == nil || !pl.allPrim {
		t.Fatalf("wide plan: got %+v, want a flat plan", pl)
	}
	if len(pl.fields) != 17 {
		t.Fatalf("wide plan fields: got %d, want 17", len(pl.fields))
	}
	if got := len(d.caches().plans); got != 1 {
		t.Fatalf("cache entries after wide visit: got %d, want 1", got)
	}
	var again *decStructPlan
	allocs := testing.AllocsPerRun(100, func() {
		again = d.structPlanFor(desc, st)
	})
	if again != pl {
		t.Fatal("repeat wide visit: cached plan pointer changed")
	}
	if allocs != 0 {
		t.Fatalf("repeat wide visit allocations: got %v, want 0", allocs)
	}
}

// Alternating two descriptor instances on one target type rebuilds the
// entry on each switch: pointer-identity semantics preserved, the plan
// always matches the latest descriptor — both for flat plans and for
// composite entries. Two descOf calls over one type produce exactly the
// structurally-equal-but-distinct instance pair of the alternating
// literal scenario.
func TestFlatLayoutDescriptorSwitchRebuilds(t *testing.T) {
	type pair struct {
		A int64
		B string
	}
	type negPair struct {
		A int64
		B *string
	}
	pt := reflect.TypeFor[pair]()
	npt := reflect.TypeFor[negPair]()

	descA, errA := descOf(pt, "")
	descB, errB := descOf(pt, "")
	if errA != nil || errB != nil {
		t.Fatalf("pair descriptors: %v / %v", errA, errB)
	}
	d := &codecDecoder{}
	flA1 := d.structPlanFor(descA, pt)
	if flA1 == nil || !flA1.allPrim {
		t.Fatal("first flat descriptor: got a non-flat plan")
	}
	flB := d.structPlanFor(descB, pt)
	if flB == nil || flB.desc != descB {
		t.Fatalf("switched descriptor: got %+v, want rebuilt plan for descB", flB)
	}
	flA2 := d.structPlanFor(descA, pt)
	if flA2 == nil || flA2.desc != descA {
		t.Fatalf("switched back: got %+v, want rebuilt plan for descA", flA2)
	}
	if flA2 == flA1 {
		t.Fatal("switch-back must rebuild, not reuse the evicted plan")
	}
	if len(flA2.fields) != len(flB.fields) {
		t.Fatalf("structurally equal descriptors: field counts %d vs %d", len(flA2.fields), len(flB.fields))
	}
	for i := range flA2.fields {
		a, b := flA2.fields[i], flB.fields[i]
		if a.prim != b.prim || a.ft != b.ft || len(a.idx) != len(b.idx) {
			t.Fatalf("structurally equal descriptors: field %d differs", i)
		}
	}
	if got := len(d.caches().plans); got != 1 {
		t.Fatalf("cache entries after alternation: got %d, want 1", got)
	}

	negA, errA := descOf(npt, "")
	negB, errB := descOf(npt, "")
	if errA != nil || errB != nil {
		t.Fatalf("negative descriptors: %v / %v", errA, errB)
	}
	negD := &codecDecoder{}
	p1 := negD.structPlanFor(negA, npt)
	if p1 == nil || p1.allPrim {
		t.Fatalf("negative descriptor: got %+v, want a composite plan", p1)
	}
	p2 := negD.structPlanFor(negB, npt)
	if p2 == nil || p2.desc != negB || p2.allPrim {
		t.Fatalf("negative switch: got %+v, want composite plan on negB", p2)
	}
	if entry := negD.caches().plans[npt]; entry == nil || entry.desc != negB || entry.allPrim {
		t.Fatalf("entry after switch: got %+v, want composite entry on negB", entry)
	}
}

// Alternating descriptor instances of one type rebuild the cached plan
// on every switch, in both starting orders; the entry always matches the
// latest descriptor and same-instance repeats are cache hits.
func TestStructPlanAlternationBothOrders(t *testing.T) {
	type pair struct {
		A int64
		B string
	}
	pt := reflect.TypeFor[pair]()
	x, err := descOf(pt, "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := descOf(pt, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, startWithX := range []bool{true, false} {
		a, b := x, y
		if !startWithX {
			a, b = y, x
		}
		d := &codecDecoder{}
		pa1 := d.structPlanFor(a, pt)
		if pa1 == nil || pa1.desc != a {
			t.Fatalf("order x=%v first visit: got %v, want plan on the passed descriptor", startWithX, pa1)
		}
		pb := d.structPlanFor(b, pt)
		if pb == nil || pb.desc != b {
			t.Fatalf("order x=%v switch: got %v, want rebuilt plan on the second descriptor", startWithX, pb)
		}
		if pb == pa1 {
			t.Fatalf("order x=%v switch must rebuild, not reuse", startWithX)
		}
		pa2 := d.structPlanFor(a, pt)
		if pa2 == nil || pa2.desc != a || pa2 == pa1 {
			t.Fatalf("order x=%v switch back must rebuild on the first descriptor", startWithX)
		}
		var again *decStructPlan
		allocs := testing.AllocsPerRun(10, func() {
			again = d.structPlanFor(a, pt)
		})
		if again != pa2 || allocs != 0 {
			t.Fatalf("order x=%v repeat after rebuild: got hit=%v allocs=%v, want cached hit with 0 allocs", startWithX, again == pa2, allocs)
		}
		if got := len(d.caches().plans); got != 1 {
			t.Fatalf("order x=%v cache entries: got %d, want 1", startWithX, got)
		}
		if len(pa2.fields) != len(pb.fields) {
			t.Fatalf("order x=%v rebuilt plans diverge in field count", startWithX)
		}
	}
}

// The plan cache is bounded by distinct type visits: repeats neither
// grow the table nor resolve fields again.
func TestFlatLayoutCacheBoundedByDistinctTypes(t *testing.T) {
	type t1 struct{ A int64 }
	type t2 struct{ A string }
	type t3 struct{ A float64 }
	t1d, err := descOf(reflect.TypeFor[t1](), "")
	if err != nil {
		t.Fatal(err)
	}
	t2d, err := descOf(reflect.TypeFor[t2](), "")
	if err != nil {
		t.Fatal(err)
	}
	t3d, err := descOf(reflect.TypeFor[t3](), "")
	if err != nil {
		t.Fatal(err)
	}
	d := &codecDecoder{}
	for range 100 {
		d.structPlanFor(t1d, reflect.TypeFor[t1]())
		d.structPlanFor(t2d, reflect.TypeFor[t2]())
		d.structPlanFor(t3d, reflect.TypeFor[t3]())
	}
	if got := len(d.caches().plans); got != 3 {
		t.Fatalf("cache entries after 300 visits: got %d, want 3", got)
	}
}

// TestConcreteCoderMemoZeroAlloc: after the first resolution of a type,
// repeated concreteCoder calls on the same decoder allocate nothing.
func TestConcreteCoderMemoZeroAlloc(t *testing.T) {
	// a completed stream decode leaves the facade's wire reader live —
	// reuse it as the carrier for a bare per-value decoder
	pb, err := Marshal(struct{ T time.Time }{T: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	facade := NewDecoder(bytes.NewReader(pb))
	var sink struct{ T time.Time }
	if err := facade.Decode(&sink); err != nil {
		t.Fatal(err)
	}
	d := newBudgetDecoder(facade.dec.r, effLimits(Limits{}), nil, nil, nil, nil, nil, nil, nil)
	testing.AllocsPerRun(1, func() {
		if tc := d.concreteCoder(timeType, nameOf(timeType)); tc == nil || tc.ck != ckTime {
			t.Fatal("time coder resolution failed")
		}
	})
	repeat := testing.AllocsPerRun(10, func() {
		if tc := d.concreteCoder(timeType, nameOf(timeType)); tc == nil || tc.ck != ckTime {
			t.Fatal("time coder resolution failed")
		}
	})
	if repeat > 0 {
		t.Fatalf("cached concreteCoder allocates: %v allocs/call", repeat)
	}
}

// TestCkTimeSetZeroAlloc pins the addressable time assignment: decoding
// a time.Time field into an addressable target does not box the struct
// per value. The stream-level per-value allocation delta between an
// int-only struct and the same struct plus time.Time is dominated by
// fixed decode mechanics (coder memo, zone-name string, coder-descriptor
// entries: measured ~4.1 per value, stable under AllocsPerRun). The
// regressed boxing Set would add exactly one allocation per value (+N),
// so the ceiling below separates the addressable assignment from the
// boxed one with headroom; verified by mutation (restoring
// target.Set(reflect.ValueOf(tt)) pushes the delta over the ceiling).
func TestCkTimeSetZeroAlloc(t *testing.T) {
	const n = 100
	const ceiling = 450 // measured 410 + headroom; a boxed Set would read 510
	type base struct{ X int }
	type withTime struct {
		X int
		T time.Time
	}
	tt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mkStream := func(sample any) []byte {
		var buf bytes.Buffer
		enc := NewEncoder(&buf)
		for range n {
			if err := enc.Encode(sample); err != nil {
				t.Fatal(err)
			}
		}
		return buf.Bytes()
	}
	streamBase := mkStream(base{X: 7})
	streamTime := mkStream(withTime{X: 7, T: tt})
	ab := int(testing.AllocsPerRun(20, func() {
		var ob base
		dec := NewDecoder(bytes.NewReader(streamBase))
		for range n {
			if err := dec.Decode(&ob); err != nil {
				t.Fatal(err)
			}
		}
	}))
	at := int(testing.AllocsPerRun(20, func() {
		var ot withTime
		dec := NewDecoder(bytes.NewReader(streamTime))
		for range n {
			if err := dec.Decode(&ot); err != nil {
				t.Fatal(err)
			}
		}
	}))
	if delta := at - ab; delta > ceiling {
		t.Fatalf("time field stream decode adds %d allocs over %d values (base %d, with-time %d): ceiling %d exceeded — the addressable Set is boxing again", delta, n, ab, at, ceiling)
	}
}
