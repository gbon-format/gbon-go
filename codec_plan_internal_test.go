package gbon

import (
	"bytes"
	"reflect"
	"sync"
	"testing"
)

// plTestCoder is a minimal custom coder: the body is the field value
// shifted by a fixed offset.
type plTestCoder struct{}

func (plTestCoder) EncodeValue(e *Encoder, v reflect.Value) error {
	return e.Encode(v.Field(0).Int() + 100)
}

func (plTestCoder) DecodeValue(d *Decoder, v reflect.Value) error {
	var n int64
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.Field(0).SetInt(n - 100)
	return nil
}

// dropPlans removes the global plan-cache entries of the given types at
// the stateless scope (test hook: the cold-cache leg of the
// cache-independence scenarios).
func dropPlans(ts ...reflect.Type) {
	planRegistry.Lock()
	defer planRegistry.Unlock()
	for _, t := range ts {
		delete(planRegistry.m, planKey{t: t, ep: 0})
	}
}

// cold ≡ warm ≡ parallel-warmed: the encoded bytes of a value depend
// only on the value, never on the plan-cache state or on which other
// streams warmed it.
func TestPlanColdWarmParallelByteIdentity(t *testing.T) {
	type leaf struct {
		A int64
		B string
	}
	type node struct {
		L leaf
		M map[string]int64
		P *leaf
	}
	lt, nt := reflect.TypeFor[leaf](), reflect.TypeFor[node]()
	val := node{
		L: leaf{A: 1, B: "x"},
		M: map[string]int64{"k": 2},
		P: &leaf{A: 3, B: "y"},
	}
	warm := []any{
		[]leaf{{A: 9, B: "w1"}, {A: 8, B: "w2"}},
		map[string]leaf{"a": {A: 7, B: "w3"}},
		node{L: leaf{A: 6, B: "w4"}},
	}
	// cold: cache entries for the types dropped, fresh encoder per value
	dropPlans(lt, nt, reflect.TypeFor[[]leaf](), reflect.TypeFor[map[string]leaf]())
	cold, err := Marshal(val)
	if err != nil {
		t.Fatal(err)
	}
	// warm: other values of the same types already built their plans
	for _, w := range warm {
		if _, err := Marshal(w); err != nil {
			t.Fatal(err)
		}
	}
	warmb, err := Marshal(val)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cold, warmb) {
		t.Fatal("cold and warm encodings differ")
	}
	// parallel warm-up: concurrent streams of the same types, then the
	// target value through yet another encoder
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := Marshal(node{L: leaf{A: int64(i), B: "par"}}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	parb, err := Marshal(val)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cold, parb) {
		t.Fatal("parallel-warmed encoding differs from cold")
	}
}

// A Register after cache warm-up bumps the scope epoch: the type's
// plan is rebuilt with the coder (a new cache entry; the pre-register
// entry stays untouched), and two encoders holding the same coder
// registration produce identical bytes.
func TestPlanRegisterAfterWarmEpochBump(t *testing.T) {
	type dto struct{ X int }
	dt := reflect.TypeFor[dto]()
	dropPlans(dt)

	plain, err := Marshal(dto{X: 1})
	if err != nil {
		t.Fatal(err)
	}
	encOne := func() []byte {
		var buf bytes.Buffer
		enc := NewEncoder(&buf)
		if err := enc.RegisterCoder(dto{}, plTestCoder{}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(dto{X: 1}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	b1 := encOne()
	planRegistry.RLock()
	_, hasEpoch0 := planRegistry.m[planKey{t: dt, ep: 0}]
	n := 0
	for k := range planRegistry.m {
		if k.t == dt {
			n++
		}
	}
	planRegistry.RUnlock()
	if !hasEpoch0 || n < 2 {
		t.Fatalf("plan entries for dto after register: epoch0=%v total=%d, want the old entry kept plus a new-epoch entry", hasEpoch0, n)
	}
	if bytes.Equal(plain, b1) {
		t.Fatal("coder registration did not change the encoding")
	}
	if b2 := encOne(); !bytes.Equal(b1, b2) {
		t.Fatal("two coders-registered encoders produced different bytes")
	}
}

// Budget trips are cache-independent: a value tripping the depth budget
// reports the identical error cold and warm. The walked leg drives the
// scan recursion through a pointer chain; the pure leg drives the static
// fast path through a sharing-source-free nesting (structs over a leaf
// scalar, no pointer/interface/map/slice/string anywhere), whose depth
// trip comes from the plan's static scan depth.
func TestPlanBudgetErrorsColdWarm(t *testing.T) {
	type deep struct {
		Next *deep
		V    int64
	}
	type pureNested struct {
		L1 struct {
			L2 struct {
				L3 struct {
					L4 struct{ V int64 }
				}
			}
		}
	}
	var d deep
	cur := &d
	for i := range 8 {
		cur.Next = &deep{V: int64(i)}
		cur = cur.Next
	}
	pv := pureNested{}
	pv.L1.L2.L3.L4.V = 1
	cases := []struct {
		label string
		val   any
	}{
		{"walked pointer chain", d},
		{"pure static nesting", pv},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			run := func() string {
				var buf bytes.Buffer
				enc := NewEncoder(&buf)
				if err := enc.SetLimits(Limits{MaxDepth: 4}); err != nil {
					t.Fatal(err)
				}
				err := enc.Encode(tc.val)
				if err == nil {
					t.Fatal("depth budget not tripped")
				}
				return err.Error()
			}
			dropPlans(reflect.TypeFor[deep](), reflect.TypeFor[*deep](),
				reflect.TypeFor[pureNested]())
			cold := run()
			warm := run()
			if cold != warm {
				t.Fatalf("budget error differs cold vs warm:\ncold: %s\nwarm: %s", cold, warm)
			}
		})
	}
}

// The pure leg above must actually take the fast path: the plan of the
// nesting is pure with the static depth the trip formula consumes.
func TestPlanBudgetPureFixtureIsPure(t *testing.T) {
	type pureNested struct {
		L1 struct {
			L2 struct {
				L3 struct {
					L4 struct{ V int64 }
				}
			}
		}
	}
	dropPlans(reflect.TypeFor[pureNested]())
	e := newCodecEncoder()
	pl := e.planFor(reflect.TypeFor[pureNested]())
	if !pl.pure {
		t.Fatal("nesting fixture plan is impure — depth-trip leg exercises the walked path")
	}
	if want := 6; pl.scanDepth != want {
		t.Fatalf("static scan depth: got %d, want %d", pl.scanDepth, want)
	}
}

// A non-struct type already encoded in this stream is rejected by
// RegisterAs: the encoded-type gate reads the per-stream type table,
// which carries every encoded kind, not only structs.
func TestPlanRegisterAsNonStructAfterEncode(t *testing.T) {
	type namedStr string
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(namedStr("v")); err != nil {
		t.Fatal(err)
	}
	if err := enc.RegisterAs("gbon.plan.named-str", namedStr("")); err == nil {
		t.Fatal("RegisterAs after encoding a non-struct type: got nil, want rejection")
	}
}

// A stream of same-typed values resolves the struct decode plan once
// per Reader: the second (and later) values decode through the cached
// plan, and the round trip is value-exact.
func TestPlanPerReaderLayoutSharing(t *testing.T) {
	type rec struct {
		A int64
		B string
		C float64
	}
	const n = 4
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	want := []rec{}
	for i := range n {
		r := rec{A: int64(i), B: "v", C: float64(i) / 2}
		want = append(want, r)
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	dec := NewDecoder(bytes.NewReader(buf.Bytes()))
	got := []rec{}
	for range n {
		var r rec
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip mismatch: want %v, got %v", want, got)
	}
	if dec.dec.shr == nil || dec.dec.shr.built != 1 {
		built := -1
		if dec.dec.shr != nil {
			built = dec.dec.shr.built
		}
		t.Fatalf("struct plan builds over %d values: got %d, want 1", n, built)
	}
}

// decodeMode decodes one stream through a public Decoder whose stream
// decoder runs with the given plan switch, returning the error text.
// prep runs on the Decoder before the first Decode (fixture bindings).
func decodeMode(t *testing.T, b []byte, referenceLeg bool, prep func(*Decoder), mk func() any) string {
	t.Helper()
	dec := NewDecoder(bytes.NewReader(b))
	if prep != nil {
		prep(dec)
	}
	sd := &codecStreamDecoder{reg: dec.reg, asName: dec.asName, coders: dec.coders, fac: dec}
	sd.init(bytes.NewReader(b))
	sd.shr.referenceLeg = referenceLeg
	dec.dec = sd
	v := mk()
	if err := dec.Decode(v); err != nil {
		return err.Error()
	}
	return ""
}

// The compiled struct plans and the reference interpreter agree on
// values and errors across a structural corpus (P3): nested composites,
// maps, named and blank fields, wide all-primitive structs, skip paths
// (stream field absent on target), and truncated input.
func TestPlanDecodeCompiledMatchesReference(t *testing.T) {
	type inner struct {
		A int64
		B []string
	}
	type outer struct {
		I inner
		M map[string]int64
		N *inner
	}
	type wide struct {
		F00, F01, F02, F03, F04, F05, F06, F07 int64
		F08, F09, F10, F11, F12, F13, F14, F15 int64
		F16                                    int64
	}
	type named struct {
		X int64
		_ float64
	}
	type fullSource struct {
		X int64
		Y string
	}
	type skipTarget struct {
		X int64
	}
	val := outer{
		I: inner{A: 1, B: []string{"a", "b"}},
		M: map[string]int64{"k": 2, "j": 3},
		N: &inner{A: 4},
	}
	skipPrep := func(d *Decoder) {
		if err := d.RegisterAs(nameOf(reflect.TypeFor[fullSource]()), skipTarget{}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		label string
		val   any
		prep  func(*Decoder)
		mk    func() any
	}{
		{"nested composite", val, nil, func() any { return &outer{} }},
		{"wide flat", wide{F16: 7}, nil, func() any { return &wide{} }},
		{"named blank field", named{X: 5}, nil, func() any { return &named{} }},
		{"skip path", fullSource{X: 5, Y: "s"}, skipPrep, func() any { return &skipTarget{} }},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			b, err := Marshal(tc.val)
			if err != nil {
				t.Fatal(err)
			}
			errA := decodeMode(t, b, false, tc.prep, tc.mk)
			errB := decodeMode(t, b, true, tc.prep, tc.mk)
			if errA != errB {
				t.Fatalf("compiled vs reference errors differ:\nplan: %s\nref:  %s", errA, errB)
			}
			// value identity leg: both modes decode into fresh targets,
			// then compare through one more pass each
			va := tc.mk()
			vb := tc.mk()
			da := NewDecoder(bytes.NewReader(b))
			db := NewDecoder(bytes.NewReader(b))
			if tc.prep != nil {
				tc.prep(da)
				tc.prep(db)
			}
			db.dec = &codecStreamDecoder{reg: db.reg, asName: db.asName, coders: db.coders, fac: db}
			db.dec.init(bytes.NewReader(b))
			db.dec.shr.referenceLeg = true
			if err := da.Decode(va); err != nil {
				t.Fatal(err)
			}
			if err := db.Decode(vb); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(va, vb) {
				t.Fatalf("compiled vs reference values differ: %v vs %v", va, vb)
			}
		})
	}
	// truncated input: identical errors on both disciplines
	b, err := Marshal(outer{I: inner{A: 1, B: []string{"a"}}, M: map[string]int64{"k": 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{8, 16, len(b) - 3} {
		errA := decodeMode(t, b[:cut], false, nil, func() any { return &outer{} })
		errB := decodeMode(t, b[:cut], true, nil, func() any { return &outer{} })
		if errA == "" {
			t.Fatalf("truncation at %d: compiled leg unexpectedly succeeded", cut)
		}
		if errA != errB {
			t.Fatalf("truncation at %d: error texts differ:\nplan: %s\nref:  %s", cut, errA, errB)
		}
	}
}
