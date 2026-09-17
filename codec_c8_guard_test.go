package gbon

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

// negZ64C8 is a runtime-produced negative zero for the equivalence table.
var negZ64C8 = math.Copysign(0, -1)

// numeric-DTO fixture: a value tree with no pointer, interface, map,
// slice, string, or blob anywhere — the empty sharing-source class.
type c8PureDTO struct {
	A int64
	B float64
	C [4]int32
	D bool
	E c8PureCell
}

type c8PureCell struct {
	X uint8
	Y complex128
}

// control fixture: one slice field is enough to activate the grouping
// machinery (shared backing).
type c8SliceControl struct {
	A int64
	S []int32
}

// The pure-class scan fast path allocates nothing (no visited set, no
// slot/identity/header writes); the slice-bearing control type pays the
// visited-set allocation (the grouping sources are live).
func TestC8PureScanNoMachinery(t *testing.T) {
	dto := c8PureDTO{A: 1, B: 2.5, C: [4]int32{1, 2, 3, 4}, D: true, E: c8PureCell{X: 7}}
	e := newCodecEncoder()
	drv := reflect.ValueOf(dto)
	allocs := testing.AllocsPerRun(100, func() {
		if err := e.scanValue(drv); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("pure scan allocations: got %v, want 0", allocs)
	}
	if len(e.slotIx) != 0 || len(e.grains) != 0 || len(e.maps) != 0 {
		t.Fatalf("pure scan machinery writes: slots=%d grains=%d maps=%d, want 0/0/0", len(e.slotIx), len(e.grains), len(e.maps))
	}
	if e.hdrEp == 0 {
		t.Fatal("scan epoch not advanced")
	}

	ctl := c8SliceControl{A: 1, S: []int32{9, 8, 7}}
	e2 := newCodecEncoder()
	crv := reflect.ValueOf(ctl)
	if err := e2.scanValue(crv); err != nil {
		t.Fatal(err)
	}
	if len(e2.slotIx) == 0 {
		t.Fatal("control scan recorded no slot — grouping sources inactive")
	}
	if len(e2.hdrVisits) == 0 {
		t.Fatal("control scan recorded no header visit — grouping sources inactive")
	}
}

// The cheap path emits byte-identical output to a cold cache: pure
// values round-trip exactly, and repeated encodings of the same value
// are stable (the guard never reaches the output).
func TestC8PureOutputStable(t *testing.T) {
	dto := c8PureDTO{A: 1, B: 2.5, C: [4]int32{1, 2, 3, 4}, D: true, E: c8PureCell{X: 7}}
	dropPlans(reflect.TypeFor[c8PureDTO](), reflect.TypeFor[c8PureCell]())
	cold, err := Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cold, warm) {
		t.Fatal("pure-class encoding differs cold vs warm")
	}
	var back c8PureDTO
	if err := Unmarshal(cold, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dto, back) {
		t.Fatalf("pure round trip mismatch: %v vs %v", dto, back)
	}

	// the same value nested behind a sharing source: the enclosing tree
	// runs the full path, the pure subtree stays identical bytes
	type wrapper struct {
		Tag string
		DT  c8PureDTO
	}
	w := wrapper{Tag: "t", DT: dto}
	wb, err := Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	wb2, err := Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wb, wb2) {
		t.Fatal("nested pure encoding unstable")
	}
	var wback wrapper
	if err := Unmarshal(wb, &wback); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, wback) {
		t.Fatalf("nested pure round trip mismatch: %v vs %v", w, wback)
	}
}

// TestC8TypedBatchEquiv: for every builtin primitive kind the typed
// batch read produces the same body bytes as the guarded per-value
// frame (reflect read through encodeBody).
func TestC8TypedBatchEquiv(t *testing.T) {
	values := []struct {
		k reflect.Kind
		v any
	}{
		{reflect.Bool, true},
		{reflect.Int, int(-5)},
		{reflect.Int8, int8(-1)},
		{reflect.Int16, int16(-300)},
		{reflect.Int32, int32(70000)},
		{reflect.Int64, int64(1 << 62)},
		{reflect.Uint, uint(9)},
		{reflect.Uint8, uint8(200)},
		{reflect.Uint16, uint16(60000)},
		{reflect.Uint32, uint32(1 << 31)},
		{reflect.Uint64, uint64(1 << 63)},
		{reflect.Float32, float32(math.Copysign(0, -1))},
		{reflect.Float64, float64(-1e300)},
		{reflect.Complex64, complex64(complex(1, negZ64C8))},
		{reflect.Complex128, complex128(complex(negZ64C8, 2.5))},
	}
	for _, tc := range values {
		e := newCodecEncoder()
		rv := reflect.ValueOf(tc.v)
		if err := e.encodeBody(rv, pathNode{}); err != nil {
			t.Fatalf("%s: encodeBody: %v", tc.k, err)
		}
		w2 := newCodecEncoder()
		vp := typedPtr(tc.v)
		if err := writePrimAt(w2.w, tc.k, vp); err != nil {
			t.Fatalf("%s: typed write: %v", tc.k, err)
		}
		if !bytes.Equal(e.w.Bytes(), w2.w.Bytes()) {
			t.Fatalf("%s: guarded % x vs typed % x", tc.k, e.w.Bytes(), w2.w.Bytes())
		}
	}
}
