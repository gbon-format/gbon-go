package gbon

import (
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"testing"
	"unsafe"
)

// negZ64 is a runtime-produced negative zero (the literal form collapses
// to +0.0 in Go constant arithmetic).
var negZ64 = math.Copysign(0, -1)

// typedPtr materializes one addressable copy of v and returns a pointer
// to its storage — the typed predicate then reads the same bytes the
// reflect oracle reads.
func typedPtr(v any) unsafe.Pointer {
	rv := reflect.ValueOf(v)
	pv := reflect.New(rv.Type()).Elem()
	pv.Set(rv)
	return pv.Addr().UnsafePointer()
}

// typedLayoutCases enumerates boundary values per kind class: zero and
// nonzero forms, negative-zero floats, empty-yet-nonnil strings and
// slices, nil/typed interfaces, big.Int sign variants.
type typedLayoutCase struct {
	val  any
	ifac bool
}

func typedLayoutCases() []typedLayoutCase {
	nz := "abc"
	empty := nz[0:0]
	cases := []typedLayoutCase{
		{false, false}, {true, false},
		{int(0), false}, {int(-7), false},
		{int8(-1), false}, {int16(300), false}, {int32(-70000), false}, {int64(1 << 62), false},
		{uint(0), false}, {uint8(9), false}, {uint16(700), false}, {uint32(1 << 30), false}, {uint64(1 << 63), false},
		{uintptr(0), false}, {uintptr(1 << 40), false},
		{float32(0), false}, {float32(math.Copysign(0, -1)), false}, {float32(1.5), false},
		{float64(0), false}, {negZ64, false}, {float64(-1e300), false},
		{complex64(0), false}, {complex64(complex(negZ64, 0)), false}, {complex64(complex(1, negZ64)), false},
		{complex128(0), false}, {complex128(complex(negZ64, negZ64)), false}, {complex128(complex(0, 2.5)), false},
		{"", false}, {"x", false}, {empty, false},
		{[]int(nil), false}, {[]int{}, false}, {[]int{1, 0, 0}, false},
		{[]byte(nil), false}, {[]byte{}, false}, {[]byte{0xAB, 0, 0xCD}, false},
		{map[string]int(nil), false}, {map[string]int{}, false},
		{(chan int)(nil), false},
		{(*big.Int)(nil), false}, {new(big.Int), false}, {big.NewInt(1 << 40), false}, {big.NewInt(-(1 << 40)), false},
		{any(nil), true}, {any(0), true}, {any((*int)(nil)), true}, {any(""), true},
		{struct {
			A int
			B float64
			C string
		}{}, false},
		{struct {
			A int
			B float64
			C string
		}{A: 1, B: negZ64, C: ""}, false},
		{struct{ F float64 }{negZ64}, false},
		{struct {
			P *int
			S []int
			M map[int]int
			I any
		}{}, false},
		{struct {
			P *int
			S []int
			M map[int]int
			I any
		}{P: new(int), S: []int{}, M: map[int]int{}, I: 0}, false},
		{[3]int32{}, false}, {[3]int32{0, 0, 1}, false},
		{[2]string{"", empty}, false},
		{[]float64{0, negZ64, 1.5, 0}, false},
		{[]complex128{0, complex(negZ64, 0), complex(1, negZ64)}, false},
	}
	return cases
}

// TestTypedLayoutBitZeroDualRun: for every boundary value the typed
// predicate reading pinned memory (isBitZeroAt) agrees with the reflect
// oracle (isBitZero) on the same object.
func TestTypedLayoutBitZeroDualRun(t *testing.T) {
	ifaceT := reflect.TypeFor[any]()
	for _, c := range typedLayoutCases() {
		if c.ifac {
			var s any = c.val
			if got := isBitZeroAt(ifaceT, unsafe.Pointer(&s)); got != (s == nil) {
				t.Fatalf("%#v (iface): typed=%v, nil-ness=%v", c.val, got, s == nil)
			}
			continue
		}
		rv := reflect.ValueOf(c.val)
		if isBitZeroAt(rv.Type(), typedPtr(c.val)) != isBitZero(rv) {
			t.Fatalf("%#v (%s): typed and reflect predicates disagree", c.val, rv.Type())
		}
	}
}

// TestTypedLayoutBitZeroPBT: random sequences over boundary element
// values keep the typed and reflect predicates equal value by value.
func TestTypedLayoutBitZeroPBT(t *testing.T) {
	rng := rand.New(rand.NewSource(20260912))
	elems := []any{
		int32(0), int32(-5), float64(0), negZ64, float64(3.25),
		complex128(0), complex128(complex(0, negZ64)),
		"", "s", []int(nil), []int{0}, map[string]int(nil),
		(*big.Int)(nil), big.NewInt(0), big.NewInt(-3),
	}
	for round := range 200 {
		for k := range 8 {
			ev := elems[rng.Intn(len(elems))]
			rv := reflect.ValueOf(ev)
			if isBitZeroAt(rv.Type(), typedPtr(ev)) != isBitZero(rv) {
				t.Fatalf("round %d elem %d (%s): predicates disagree", round, k, rv.Type())
			}
		}
	}
}

type tlInner struct {
	N   int
	Big *big.Int
}

type tlDeep struct {
	A struct{ P, Q uint16 }
	B []float64
	C [2]struct{ S string }
	D map[string]int64
	E tlInner
}

var typedLayoutStructs = []reflect.Type{
	reflect.TypeFor[struct {
		A int8
		B int64
		C string
	}](),
	reflect.TypeFor[struct {
		X bool
		Y [3]int32
		Z complex128
	}](),
	reflect.TypeFor[struct {
		F float32
		G float64
		H *big.Int
		I any
	}](),
	reflect.TypeFor[tlDeep](),
}

// TestTypedLayoutPlanOffsets: the plan's emission-field and scan-field
// byte offsets equal the reflect struct metadata.
func TestTypedLayoutPlanOffsets(t *testing.T) {
	e := newCodecEncoder()
	for _, st := range typedLayoutStructs {
		pl := e.planFor(st)
		if pl.op != opStruct {
			t.Fatalf("%s: plan op %d, want opStruct", st, pl.op)
		}
		for i := range st.NumField() {
			f := st.Field(i)
			var off uintptr = ^uintptr(0)
			for j := range pl.scanFields {
				if pl.scanFields[j].idx == int32(i) {
					off = pl.scanFields[j].off
				}
			}
			if off != f.Offset {
				t.Fatalf("%s field %d: scan off %d, reflect %d", st, i, off, f.Offset)
			}
			if f.Name == "_" || f.PkgPath != "" {
				continue
			}
			eoff := ^uintptr(0)
			for j := range pl.fields {
				if pl.fields[j].idx == int32(i) {
					eoff = pl.fields[j].off
				}
			}
			if eoff != f.Offset {
				t.Fatalf("%s field %d: emit off %d, reflect %d", st, i, eoff, f.Offset)
			}
		}
	}
}

// TestTypedLayoutFieldBytes: reading a field through base+plan-offset
// is byte-identical to reading the same field through reflect on a live
// value.
func TestTypedLayoutFieldBytes(t *testing.T) {
	e := newCodecEncoder()
	v := reflect.New(typedLayoutStructs[3]).Elem()
	v.Field(0).Field(0).SetUint(0xABCD)
	v.Field(1).Set(reflect.ValueOf([]float64{0, negZ64, 2.5}))
	v.Field(2).Index(0).Field(0).SetString("tail")
	v.Field(4).Field(1).Set(reflect.ValueOf(big.NewInt(-42)))
	st := typedLayoutStructs[3]
	pl := e.planFor(st)
	base := v.Addr().UnsafePointer()
	var viaPlan, viaReflect []byte
	for i := range pl.scanFields {
		f := st.Field(int(pl.scanFields[i].idx))
		n := f.Type.Size()
		p := unsafe.Add(base, pl.scanFields[i].off)
		viaPlan = append(viaPlan, unsafe.Slice((*byte)(p), n)...)
		rp := v.Field(int(pl.scanFields[i].idx)).Addr().UnsafePointer()
		viaReflect = append(viaReflect, unsafe.Slice((*byte)(rp), n)...)
	}
	if string(viaPlan) != string(viaReflect) {
		t.Fatal("base+off reads diverge from reflect field reads")
	}
}

// TestTypedLayoutElemStride: plan element strides equal the type
// metadata element sizes, and marshalled component strides equal the
// plan stride of the head member's slot type.
func TestTypedLayoutElemStride(t *testing.T) {
	e := newCodecEncoder()
	elemTypes := []reflect.Type{
		reflect.TypeFor[bool](), reflect.TypeFor[int32](), reflect.TypeFor[uint64](),
		reflect.TypeFor[float64](), reflect.TypeFor[complex128](), reflect.TypeFor[string](),
		reflect.TypeFor[tlInner](), reflect.TypeFor[[]int](),
	}
	for _, et := range elemTypes {
		st := reflect.SliceOf(et)
		if got := e.planFor(st).elemStride; got != et.Size() {
			t.Fatalf("%s: plan stride %d, type size %d", st, got, et.Size())
		}
		at := reflect.ArrayOf(3, et)
		if got := e.planFor(at).elemStride; got != et.Size() {
			t.Fatalf("%s: plan stride %d, type size %d", at, got, et.Size())
		}
	}
	backing := make([]int32, 32)
	for i := range backing {
		backing[i] = int32(i + 1)
	}
	if err := e.Encode([][]int32{backing[0:12:12], backing[8:32:32]}); err != nil {
		t.Fatal(err)
	}
	if len(e.arena.comps) == 0 {
		t.Fatal("no components in the encoder arena — stride cross-check probe vacuous")
	}
	for ci := range e.arena.comps {
		et := e.arena.sElemT[e.arena.comps[ci].head]
		want := e.planFor(reflect.SliceOf(et)).elemStride
		if got := uintptr(e.arena.comps[ci].es); got != want {
			t.Fatalf("comp %d (%s): es %d, plan stride %d", ci, et, got, want)
		}
	}
}
