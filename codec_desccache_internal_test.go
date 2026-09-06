package gbon

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

// The struct walk descriptor is built once per stream per
// type. The six struct occurrences below (2 outer + 4 inner) must leave
// two cache entries, not six.
func TestStructDescOncePerStreamType(t *testing.T) {
	type inner struct{ A int }
	type outer struct {
		X inner
		Y inner
		Z int
	}
	enc := NewEncoder(&bytes.Buffer{})
	if err := enc.Encode([]outer{{X: inner{A: 1}}, {Y: inner{A: 2}}}); err != nil {
		t.Fatal(err)
	}
	if got := len(enc.enc.structDescs); got != 2 {
		t.Fatalf("structDesc cache entries: got %d, want 2", got)
	}
}

// The flat-plan scenarios below resolve descriptors through the
// package's own walk (descOf), keeping the test suite free of
// internal/wire imports. Each descOf call builds a fresh descriptor
// instance; the walk memo inside one call closes recursive types on a
// single instance, which is the intern-canonical shape the stream
// decoder presents to flatLayoutFor.

// The recursive-chain workload (SB-1): one descriptor instance visited
// once per node, with the pointer field forcing a nil plan on every
// visit. A repeat visit must not allocate and must not resolve fields
// again — the negative result is cached like a plan.
func TestFlatLayoutNilPlanCachedOnRepeatVisit(t *testing.T) {
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
	if fl := d.flatLayoutFor(desc, ct); fl != nil {
		t.Fatalf("pointer-field plan: got %+v, want nil", fl)
	}
	if got := len(d.flat); got != 1 {
		t.Fatalf("negative cache entries after first visit: got %d, want 1", got)
	}
	var fl *flatLayout
	allocs := testing.AllocsPerRun(100, func() {
		fl = d.flatLayoutFor(desc, ct)
	})
	if fl != nil {
		t.Fatalf("repeat nil-plan visit: got %+v, want nil", fl)
	}
	if allocs != 0 {
		t.Fatalf("repeat nil-plan visit allocations: got %v, want 0", allocs)
	}
}

// A successful plan resolves to the exact staging table for the target
// (SB-2), and a repeat visit returns the identical cached entry
// (pointer-equal, allocation-free) — the decode-side analog of
// TestStructDescOncePerStreamType. An empty struct yields an empty
// non-nil plan: the nil-fields marker stays reserved for negative
// entries.
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
	want := []flatField{
		{idx: []int{0}, kind: fkInt, ft: reflect.TypeFor[int32]()},
		{idx: []int{1}, kind: fkString, ft: reflect.TypeFor[string]()},
		{idx: []int{2}, kind: fkBool, ft: reflect.TypeOf(false)},
		{idx: []int{3}, kind: fkF32, ft: reflect.TypeFor[float32]()},
		{idx: []int{4}, kind: fkF64, ft: reflect.TypeFor[float64]()},
		{idx: []int{5}, kind: fkUint, ft: reflect.TypeFor[uint64]()},
	}
	st := reflect.TypeFor[sample]()
	d := &codecDecoder{}
	fl := d.flatLayoutFor(desc, st)
	if fl == nil {
		t.Fatal("primitive-only plan: got nil, want a plan")
	}
	if fl.desc != desc {
		t.Fatal("cached plan descriptor: instance mismatch")
	}
	if len(fl.fields) != len(want) {
		t.Fatalf("plan fields: got %d, want %d", len(fl.fields), len(want))
	}
	for i, ff := range fl.fields {
		w := want[i]
		if ff.kind != w.kind || ff.ft != w.ft || len(ff.idx) != len(w.idx) {
			t.Fatalf("plan field %d: got {%v %v %v}, want {%v %v %v}", i, ff.idx, ff.kind, ff.ft, w.idx, w.kind, w.ft)
		}
		for j := range ff.idx {
			if ff.idx[j] != w.idx[j] {
				t.Fatalf("plan field %d idx: got %v, want %v", i, ff.idx, w.idx)
			}
		}
	}
	var again *flatLayout
	allocs := testing.AllocsPerRun(100, func() {
		again = d.flatLayoutFor(desc, st)
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
	efl := d.flatLayoutFor(edesc, reflect.TypeFor[empty]())
	if efl == nil {
		t.Fatal("empty-struct plan: got nil, want an empty plan")
	}
	if efl.fields == nil || len(efl.fields) != 0 {
		t.Fatalf("empty-struct plan fields: got %v, want empty non-nil", efl.fields)
	}
}

// The enumerated nil causes cache their negative entry (SB-3): the
// repeat visit is allocation-free and returns nil. A named wrapper is
// the walk image of a named primitive field; a missing target field is
// a superset descriptor against a subset target; a composite field kind
// is any non-primitive field (slice here — the pointer variant is the
// chain case above). The unexported-field half of the missing/unsettable
// check is unreachable through the walk (it rejects unexported fields),
// and shares the missing-field branch it fails with.
func TestFlatLayoutNilCausesCached(t *testing.T) {
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
		label  string
		target reflect.Type
		source reflect.Type
	}{
		{"named wrapper", reflect.TypeFor[named](), reflect.TypeFor[named]()},
		{"missing target field", reflect.TypeFor[subset](), reflect.TypeFor[superset]()},
		{"composite field kind", reflect.TypeFor[withSlice](), reflect.TypeFor[withSlice]()},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			desc, err := descOf(tc.source, "")
			if err != nil {
				t.Fatal(err)
			}
			d := &codecDecoder{}
			if fl := d.flatLayoutFor(desc, tc.target); fl != nil {
				t.Fatalf("first visit: got plan %+v, want nil", fl)
			}
			if got := len(d.flat); got != 1 {
				t.Fatalf("cache entries after nil cause: got %d, want 1", got)
			}
			var fl *flatLayout
			allocs := testing.AllocsPerRun(100, func() {
				fl = d.flatLayoutFor(desc, tc.target)
			})
			if fl != nil {
				t.Fatalf("repeat visit: got plan %+v, want nil", fl)
			}
			if allocs != 0 {
				t.Fatalf("repeat visit allocations: got %v, want 0", allocs)
			}
		})
	}
}

// Wide layouts take the early nil exit before any field resolution
// (SB-4); the cheap length check also keeps them out of the cache, and
// the repeat visit stays allocation-free.
func TestFlatLayoutWideEarlyExit(t *testing.T) {
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
	if len(desc.Fields) != maxFlatFields+1 {
		t.Fatalf("wide fixture fields: got %d, want %d", len(desc.Fields), maxFlatFields+1)
	}
	d := &codecDecoder{}
	if fl := d.flatLayoutFor(desc, st); fl != nil {
		t.Fatalf("wide plan: got %+v, want nil", fl)
	}
	if got := len(d.flat); got != 0 {
		t.Fatalf("cache entries after wide visit: got %d, want 0", got)
	}
	var fl *flatLayout
	allocs := testing.AllocsPerRun(100, func() {
		fl = d.flatLayoutFor(desc, st)
	})
	if fl != nil {
		t.Fatalf("repeat wide visit: got %+v, want nil", fl)
	}
	if allocs != 0 {
		t.Fatalf("repeat wide visit allocations: got %v, want 0", allocs)
	}
}

// Alternating two descriptor instances on one target type rebuilds the
// entry on each switch (SB-5): pointer-identity semantics preserved,
// the plan always matches the latest descriptor — both for plans and
// for negative entries. Two descOf calls over one type produce exactly
// the structurally-equal-but-distinct instance pair of the alternating
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
	flA1 := d.flatLayoutFor(descA, pt)
	if flA1 == nil {
		t.Fatal("first flat descriptor: got nil plan")
	}
	flB := d.flatLayoutFor(descB, pt)
	if flB == nil || flB.desc != descB {
		t.Fatalf("switched descriptor: got %+v, want rebuilt plan for descB", flB)
	}
	flA2 := d.flatLayoutFor(descA, pt)
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
		if a.kind != b.kind || a.ft != b.ft || len(a.idx) != len(b.idx) {
			t.Fatalf("structurally equal descriptors: field %d differs", i)
		}
	}
	if got := len(d.flat); got != 1 {
		t.Fatalf("cache entries after alternation: got %d, want 1", got)
	}

	negA, errA := descOf(npt, "")
	negB, errB := descOf(npt, "")
	if errA != nil || errB != nil {
		t.Fatalf("negative descriptors: %v / %v", errA, errB)
	}
	negD := &codecDecoder{}
	if fl := negD.flatLayoutFor(negA, npt); fl != nil {
		t.Fatalf("negative descriptor: got %+v, want nil", fl)
	}
	if fl := negD.flatLayoutFor(negB, npt); fl != nil {
		t.Fatalf("negative switch: got %+v, want nil", fl)
	}
	if entry := negD.flat[npt]; entry == nil || entry.desc != negB || entry.fields != nil {
		t.Fatalf("negative entry after switch: got %+v, want nil-fields marker on negB", entry)
	}
}

// The plan cache is bounded by distinct type visits (SB-6): repeats
// neither grow the table nor resolve fields again.
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
		d.flatLayoutFor(t1d, reflect.TypeFor[t1]())
		d.flatLayoutFor(t2d, reflect.TypeFor[t2]())
		d.flatLayoutFor(t3d, reflect.TypeFor[t3]())
	}
	if got := len(d.flat); got != 3 {
		t.Fatalf("cache entries after 300 visits: got %d, want 3", got)
	}
}

// TestConcreteCoderMemoZeroAlloc: after the first resolution of a type,
// repeated concreteCoder calls on the same decoder allocate nothing
// (U-I6, KL-18).
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
	d := newBudgetDecoder(facade.dec.r, effLimits(Limits{}), nil, nil, nil, nil, nil, nil)
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

// TestCkTimeSetZeroAlloc pins KL-19: decoding a time.Time field into an
// addressable target does not box the struct per value. The stream-level
// per-value allocation delta between an int-only struct and the same
// struct plus time.Time is dominated by fixed decode mechanics (coder
// memo, zone-name string, coder-descriptor entries: measured ~4.1 per
// value, stable under AllocsPerRun). The regressed boxing Set would add
// exactly one allocation per value (+N), so the ceiling below separates
// the addressable assignment from the boxed one with headroom; verified
// by mutation (restoring target.Set(reflect.ValueOf(tt)) pushes the
// delta over the ceiling).
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
