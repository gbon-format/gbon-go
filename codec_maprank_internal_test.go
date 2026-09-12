package gbon

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file carries the property probes of the rank-based generic map
// order: the rank cells must reproduce the bytewise order of the
// canonical skeleton renders for every key category, ties included. Two
// independent oracles are used: the skeleton renderer itself (decode-side
// machinery, byte output) and hand-built grammatical token models written
// from the format tables alone, independent of the wire package.

// rankProbe extracts the rank cells of one key through one walker mode.
func rankProbe(k reflect.Value, i int, track bool) ([]uint64, error) {
	rw := newRankWalker(track)
	rw.resetKey()
	if err := rw.walk(k, pathNode{idx: i}); err != nil {
		return nil, err
	}
	return slices.Clone(rw.cells), nil
}

// assertRankMatchesSkeleton is the renderer-oracle probe: per ordered
// key pair the rank verdict equals the skeleton bytewise verdict, ties
// included.
func assertRankMatchesSkeleton(t *testing.T, name string, keys []reflect.Value, track bool) {
	t.Helper()
	skels := make([][]byte, len(keys))
	ranks := make([][]uint64, len(keys))
	sc := newSkeletonScratch()
	for i, k := range keys {
		pn := pathNode{idx: i}
		sk, err := skeletonKeyBytes(k, pn, sc)
		if err != nil {
			t.Fatalf("%s: skeleton render of key %d: %v", name, i, err)
		}
		skels[i] = sk
		rk, err := rankProbe(k, i, track)
		if err != nil {
			t.Fatalf("%s: rank walk of key %d: %v", name, i, err)
		}
		ranks[i] = rk
	}
	for i := range keys {
		for j := range keys {
			want := bytes.Compare(skels[i], skels[j])
			got := rankLess(ranks[i], ranks[j])
			if (got < 0) != (want < 0) || (got > 0) != (want > 0) {
				t.Fatalf("%s: pair (%d,%d) rank=%d skeleton=%d", name, i, j, got, want)
			}
		}
	}
}

// assertRankMatchesSkeletonBothModes runs the renderer-oracle probe in
// both walker modes — key categories without interface positions must be
// mode-independent (the intern simulation is compiled out for them).
func assertRankMatchesSkeletonBothModes(t *testing.T, name string, keys []reflect.Value) {
	t.Helper()
	assertRankMatchesSkeleton(t, name, keys, true)
	assertRankMatchesSkeleton(t, name, keys, false)
}

// assertRankMatchesModel is the grammatical-oracle probe: the rank order
// equals the bytewise order of independently rendered token models.
func assertRankMatchesModel(t *testing.T, name string, keys []reflect.Value, models [][]byte) {
	t.Helper()
	if len(keys) != len(models) {
		t.Fatalf("%s: %d keys, %d models", name, len(keys), len(models))
	}
	ranks := make([][]uint64, len(keys))
	for i, k := range keys {
		rk, err := rankProbe(k, i, true)
		if err != nil {
			t.Fatalf("%s: rank walk of key %d: %v", name, i, err)
		}
		ranks[i] = rk
	}
	for i := range keys {
		for j := range keys {
			want := bytes.Compare(models[i], models[j])
			got := rankLess(ranks[i], ranks[j])
			if (got < 0) != (want < 0) || (got > 0) != (want > 0) {
				t.Fatalf("%s: pair (%d,%d) rank=%d model=%d", name, i, j, got, want)
			}
		}
	}
}

// boxedAny stores v into a fresh interface-typed value, so nil dynamics
// are valid interface-kind reflect values exactly like map keys.
func boxedAny(v any) reflect.Value {
	box := reflect.New(reflect.TypeFor[any]()).Elem()
	if v != nil {
		box.Set(reflect.ValueOf(v))
	}
	return box
}

// argFormSel is the minimal ARG selector from the format table: inline
// 0..11, then the width forms.
func argFormSel(n uint64) byte {
	switch {
	case n <= 11:
		return byte(n)
	case n <= 0xFF:
		return 0x0C
	case n <= 0xFFFF:
		return 0x0D
	case n <= 0xFFFFFFFF:
		return 0x0E
	default:
		return 0x0F
	}
}

func beBytes(v uint64, w int) []byte {
	out := make([]byte, w)
	for i := range w {
		out[i] = byte(v >> (8 * (w - 1 - i)))
	}
	return out
}

// renderTokenArgModel renders an ARG-prefixed token first byte plus
// payload from the format table.
func renderTokenArgModel(class byte, n uint64) []byte {
	f := argFormSel(n)
	out := []byte{class<<4 | f}
	switch f {
	case 0x0C:
		out = append(out, beBytes(n, 1)...)
	case 0x0D:
		out = append(out, beBytes(n, 2)...)
	case 0x0E:
		out = append(out, beBytes(n, 4)...)
	case 0x0F:
		out = append(out, beBytes(n, 8)...)
	}
	return out
}

func renderBareArgModel(n uint64) []byte {
	return renderTokenArgModel(0x0, n)
}

func renderBoolModel(b bool) []byte {
	f := byte(0)
	if b {
		f = 1
	}
	return []byte{0x10 | f}
}

func renderF32Model(f float32) []byte {
	return append([]byte{0x40}, beBytes(uint64(math.Float32bits(f)), 4)...)
}

func renderF64Model(f float64) []byte {
	return append([]byte{0x41}, beBytes(math.Float64bits(f), 8)...)
}

func renderC64Model(c complex64) []byte {
	out := []byte{0x50}
	out = append(out, beBytes(uint64(math.Float32bits(real(c))), 4)...)
	return append(out, beBytes(uint64(math.Float32bits(imag(c))), 4)...)
}

func renderC128Model(c complex128) []byte {
	out := []byte{0x51}
	out = append(out, beBytes(math.Float64bits(real(c)), 8)...)
	return append(out, beBytes(math.Float64bits(imag(c)), 8)...)
}

func renderIntModel(n int64) []byte {
	u := uint64(n<<1) ^ uint64(n>>63)
	return renderTokenArgModel(0x2, u)
}

func renderUintModel(n uint64) []byte {
	return renderTokenArgModel(0x3, n)
}

// renderBigintModel renders the bigint value body: the bare minimal
// argument of the zigzag image, the ext form beyond 64 bits, nil as the
// nil selector (byte-identical to the inline zero).
func renderBigintModel(v *big.Int) []byte {
	if v == nil {
		return []byte{0x00}
	}
	u := new(big.Int).Abs(v)
	u.Lsh(u, 1)
	if v.Sign() < 0 {
		u.Sub(u, big.NewInt(1))
	}
	if u.IsUint64() {
		return renderBareArgModel(u.Uint64())
	}
	b := u.Bytes()
	out := []byte{0x10}
	out = append(out, renderBareArgModel(uint64(len(b)))...)
	return append(out, b...)
}

func renderStructModel(parts ...[]byte) []byte {
	out := []byte{0xB0}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// renderArrayModel renders an array header (backing length, dense
// prefix) followed by the dense element tokens.
func renderArrayModel(L uint64, elems ...[]byte) []byte {
	out := renderTokenArgModel(0x8, L)
	out = append(out, renderBareArgModel(uint64(len(elems)))...)
	for _, e := range elems {
		out = append(out, e...)
	}
	return out
}

// renderDescLitModel renders a descriptor literal with a fresh intern
// state: kind header, name as a STRING literal, kind body.
func renderDescLitModel(kind byte, name string, body []byte) []byte {
	var out []byte
	if kind <= 11 {
		out = []byte{0xD0 | kind}
	} else {
		out = []byte{0xDC, kind}
	}
	out = append(out, renderStringSkel(name)...)
	return append(out, body...)
}

// mkKeys lifts a typed key slice to reflect values.
func mkKeys[T any](vs []T) []reflect.Value {
	out := make([]reflect.Value, len(vs))
	for i, v := range vs {
		out[i] = reflect.ValueOf(v)
	}
	return out
}

// TestRankScalarOrderMatchesSkeleton: integer widths, bool, floats and
// complexes (NaN excluded) against the renderer oracle in both walker
// modes and the token models.
func TestRankScalarOrderMatchesSkeleton(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	bools := []bool{false, true}
	for range 8 {
		bools = append(bools, rng.Intn(2) == 0)
	}
	assertRankMatchesSkeletonBothModes(t, "bool", mkKeys(bools))

	for _, tc := range []struct {
		name  string
		fixed []int64
	}{
		{"int8", []int64{-128, -13, -12, -11, -1, 0, 1, 11, 12, 127}},
		{"int16", []int64{-32768, -257, -256, -255, -13, 0, 12, 255, 256, 32767}},
		{"int32", []int64{math.MinInt32, -65537, -65536, -256, 0, 11, 12, 65535, 65536, math.MaxInt32}},
		{"int64", []int64{math.MinInt64, -4294967297, -4294966, -65536, -256, -13, -12, -11, -1, 0, 1, 11, 12, 255, 256, 65535, 65536, 4294967295, 4294967296, math.MaxInt64}},
	} {
		vals := slices.Clone(tc.fixed)
		for range 120 {
			vals = append(vals, rng.Int63n(1<<40)-(1<<39))
		}
		keys := make([]reflect.Value, len(vals))
		models := make([][]byte, len(vals))
		for i, v := range vals {
			switch tc.name {
			case "int8":
				keys[i] = reflect.ValueOf(int8(v))
				models[i] = renderIntModel(int64(int8(v)))
			case "int16":
				keys[i] = reflect.ValueOf(int16(v))
				models[i] = renderIntModel(int64(int16(v)))
			case "int32":
				keys[i] = reflect.ValueOf(int32(v))
				models[i] = renderIntModel(int64(int32(v)))
			case "int64":
				keys[i] = reflect.ValueOf(v)
				models[i] = renderIntModel(v)
			}
		}
		assertRankMatchesSkeletonBothModes(t, tc.name, keys)
		assertRankMatchesModel(t, tc.name, keys, models)
	}

	for _, tc := range []struct {
		name  string
		fixed []uint64
	}{
		{"uint8", []uint64{0, 1, 11, 12, 254, 255}},
		{"uint16", []uint64{0, 11, 12, 255, 256, 65535}},
		{"uint32", []uint64{0, 12, 65535, 65536, 4294967295}},
		{"uint64", []uint64{0, 1, 11, 12, 255, 256, 65535, 65536, 4294967295, 4294967296, 1<<63 - 1, math.MaxUint64}},
	} {
		keys := make([]reflect.Value, len(tc.fixed))
		models := make([][]byte, len(tc.fixed))
		for i, v := range tc.fixed {
			switch tc.name {
			case "uint8":
				keys[i] = reflect.ValueOf(uint8(v))
				models[i] = renderUintModel(uint64(uint8(v)))
			case "uint16":
				keys[i] = reflect.ValueOf(uint16(v))
				models[i] = renderUintModel(uint64(uint16(v)))
			case "uint32":
				keys[i] = reflect.ValueOf(uint32(v))
				models[i] = renderUintModel(uint64(uint32(v)))
			case "uint64":
				keys[i] = reflect.ValueOf(v)
				models[i] = renderUintModel(v)
			}
		}
		assertRankMatchesSkeletonBothModes(t, tc.name, keys)
		assertRankMatchesModel(t, tc.name, keys, models)
	}

	negZero32 := float32(math.Copysign(0, -1))
	f32s := []float32{0, negZero32, 1, -1, 1.5, -1.5, 3.4e38, 1e-38, math.MaxFloat32}
	for range 120 {
		f32s = append(f32s, float32(rng.NormFloat64())*100)
	}
	f32keys := mkKeys(f32s)
	f32models := make([][]byte, len(f32s))
	for i, v := range f32s {
		f32models[i] = renderF32Model(v)
	}
	assertRankMatchesSkeletonBothModes(t, "float32", f32keys)
	assertRankMatchesModel(t, "float32", f32keys, f32models)

	negZero := math.Copysign(0, -1)
	f64s := []float64{0, negZero, 1, -1, 2.5, -2.5, 1e300, 5e-324, math.MaxFloat64}
	for range 120 {
		f64s = append(f64s, rng.NormFloat64()*1e6)
	}
	f64keys := mkKeys(f64s)
	f64models := make([][]byte, len(f64s))
	for i, v := range f64s {
		f64models[i] = renderF64Model(v)
	}
	assertRankMatchesSkeletonBothModes(t, "float64", f64keys)
	assertRankMatchesModel(t, "float64", f64keys, f64models)

	c64s := []complex64{0, complex(0, 1), complex(negZero32, 0), complex(1.5, -2.5), complex(3e38, 1e-38)}
	c64keys := mkKeys(c64s)
	c64models := make([][]byte, len(c64s))
	for i, v := range c64s {
		c64models[i] = renderC64Model(v)
	}
	assertRankMatchesSkeletonBothModes(t, "complex64", c64keys)
	assertRankMatchesModel(t, "complex64", c64keys, c64models)

	c128s := []complex128{0, complex(0, 1), complex(negZero, 0), complex(1.5, -2.5), complex(1e300, 5e-324)}
	c128keys := mkKeys(c128s)
	c128models := make([][]byte, len(c128s))
	for i, v := range c128s {
		c128models[i] = renderC128Model(v)
	}
	assertRankMatchesSkeletonBothModes(t, "complex128", c128keys)
	assertRankMatchesModel(t, "complex128", c128keys, c128models)
}

// TestRankStringOrderMatchesSkeleton: generic-path string keys at every
// ARG-form boundary, prefix ties and raw-byte inversions, both walker
// modes.
func TestRankStringOrderMatchesSkeleton(t *testing.T) {
	strs := fixedKeys()
	rng := rand.New(rand.NewSource(20260912))
	for range 200 {
		n := rng.Intn(700)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(rng.Intn(256))
		}
		strs = append(strs, string(b))
	}
	keys := mkKeys(strs)
	models := make([][]byte, len(strs))
	for i, s := range strs {
		models[i] = renderStringSkel(s)
	}
	assertRankMatchesSkeletonBothModes(t, "string", keys)
	assertRankMatchesModel(t, "string", keys, models)
}

// TestRankBigintNilOrderMatchesSkeleton: bigint keys across the bare
// argument forms and the ext boundary (|n| ≥ 2^63), nil markers and nil
// pointer keys.
func TestRankBigintNilOrderMatchesSkeleton(t *testing.T) {
	bigs := []*big.Int{
		nil, big.NewInt(0), big.NewInt(1), big.NewInt(-1),
		big.NewInt(11), big.NewInt(12), big.NewInt(-12),
		big.NewInt(255), big.NewInt(256), big.NewInt(-256),
		big.NewInt(65535), big.NewInt(65536), big.NewInt(-65536),
		new(big.Int).SetInt64(math.MaxInt64), new(big.Int).SetInt64(math.MinInt64),
		new(big.Int).Lsh(big.NewInt(1), 63), new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63)),
		new(big.Int).Lsh(big.NewInt(1), 64), new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 64)),
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 100), big.NewInt(1)),
		new(big.Int).Neg(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 100), big.NewInt(1))),
	}
	keys := mkKeys(bigs)
	models := make([][]byte, len(bigs))
	for i, v := range bigs {
		models[i] = renderBigintModel(v)
	}
	assertRankMatchesSkeletonBothModes(t, "bigint", keys)
	assertRankMatchesModel(t, "bigint", keys, models)

	p1, p2 := 3, -4
	ptrs := []*int{nil, &p1, &p2}
	pkeys := mkKeys(ptrs)
	pmodels := make([][]byte, len(ptrs))
	for i, p := range ptrs {
		if p == nil {
			pmodels[i] = []byte{0x00}
		} else {
			pmodels[i] = renderIntModel(int64(*p))
		}
	}
	assertRankMatchesSkeletonBothModes(t, "nil-mixed pointers", pkeys)
	assertRankMatchesModel(t, "nil-mixed pointers", pkeys, pmodels)
}

// TestRankCompositeOrderMatchesSkeleton: struct field order (blank
// fields excluded), nested structs, arrays with dense-prefix tails and
// pointer components inside composites.
func TestRankCompositeOrderMatchesSkeleton(t *testing.T) {
	type blankKey struct {
		X int32
		_ int32
		Y string
	}
	type inner struct {
		B bool
		F float32
	}
	type outer struct {
		I int16
		N inner
		S string
	}
	type ptrInner struct{ V int64 }
	type ptrKey struct {
		P *ptrInner
		T string
	}
	rng := rand.New(rand.NewSource(5))
	var blanks []blankKey
	var outers []outer
	var ptrKeys []ptrKey
	for range 150 {
		blanks = append(blanks, blankKey{X: int32(rng.Int63n(1 << 20)), Y: strconv.Itoa(rng.Intn(50))})
		outers = append(outers, outer{
			I: int16(rng.Int63n(1 << 12)),
			N: inner{B: rng.Intn(2) == 0, F: float32(rng.NormFloat64())},
			S: strings.Repeat("s", rng.Intn(30)),
		})
		if rng.Intn(4) == 0 {
			ptrKeys = append(ptrKeys, ptrKey{P: nil, T: "n"})
		} else {
			ptrKeys = append(ptrKeys, ptrKey{P: &ptrInner{V: rng.Int63()}, T: strconv.Itoa(rng.Intn(9))})
		}
	}
	assertRankMatchesSkeletonBothModes(t, "blank struct", mkKeys(blanks))
	assertRankMatchesSkeletonBothModes(t, "nested struct", mkKeys(outers))
	assertRankMatchesSkeletonBothModes(t, "pointer field struct", mkKeys(ptrKeys))

	type arr5 [5]int32
	arrs := []arr5{
		{1, 2, 3, 4, 5}, {1, 2, 3, 4, 0}, {1, 2, 0, 0, 0}, {0, 0, 0, 0, 0},
		{-1, 0, 0, 0, 0}, {1, 2, 3, 5, 0}, {12, 0, 0, 0, 0}, {11, 0, 0, 0, 0},
	}
	for range 100 {
		var a arr5
		for i := range a {
			a[i] = int32(rng.Int63n(1 << 10))
		}
		arrs = append(arrs, a)
	}
	akeys := mkKeys(arrs)
	amodels := make([][]byte, len(arrs))
	for i, a := range arrs {
		e := len(a)
		for e > 0 && a[e-1] == 0 {
			e--
		}
		elems := make([][]byte, 0, e)
		for _, x := range a[:e] {
			elems = append(elems, renderIntModel(int64(x)))
		}
		amodels[i] = renderArrayModel(uint64(len(a)), elems...)
	}
	assertRankMatchesSkeletonBothModes(t, "array", akeys)
	assertRankMatchesModel(t, "array", akeys, amodels)

	bmodels := make([][]byte, len(blanks))
	for i, b := range blanks {
		bmodels[i] = renderStructModel(renderIntModel(int64(b.X)), renderStringSkel(b.Y))
	}
	assertRankMatchesModel(t, "blank struct", mkKeys(blanks), bmodels)
	omodels := make([][]byte, len(outers))
	for i, o := range outers {
		omodels[i] = renderStructModel(
			renderIntModel(int64(o.I)),
			renderStructModel(renderBoolModel(o.N.B), renderF32Model(o.N.F)),
			renderStringSkel(o.S))
	}
	assertRankMatchesModel(t, "nested struct", mkKeys(outers), omodels)
}

// rankMixedFixture builds one mixed dynamic-key list where a leading
// string field claims descriptor names before the interface position
// renders them (collide=true), or distinct harmless names otherwise.
func rankMixedFixture(collide bool) ([]reflect.Value, []any) {
	type mixedKey struct {
		Pre string
		Dyn any
	}
	type scalarBox struct {
		A int32
		B string
	}
	// dynamic *big.Int stays out: the descriptor walk of the interface
	// position rejects it on the unexported big.Int fields (static
	// map[*big.Int]T has no descriptor position and remains supported)
	dyns := []any{
		nil, 5, -7, int64(12), "abc", strings.Repeat("a", 12), true,
		3.5, math.Copysign(0, -1), scalarBox{A: 1, B: "x"}, scalarBox{A: -1, B: "y"},
		[3]int16{1, 0, 0}, [2]string{"p", "q"},
	}
	pre := func(i int) string {
		if !collide {
			return "pre" + strconv.Itoa(i)
		}
		switch dyns[i].(type) {
		case nil:
			return "nil"
		case int, int64:
			return "int"
		case string:
			return "string"
		case bool:
			return "bool"
		case float64:
			return "float64"
		default:
			return "gbon.scalarBox"
		}
	}
	ks := make([]mixedKey, len(dyns))
	for i := range dyns {
		ks[i] = mixedKey{Pre: pre(i), Dyn: dyns[i]}
	}
	return mkKeys(ks), dyns
}

// TestRankMixedInterfaceOrderMatchesSkeleton: interface dynamic keys of
// mixed categories — descriptor sections, REF crossings against earlier
// string claims, and repeated dynamic types inside one key.
func TestRankMixedInterfaceOrderMatchesSkeleton(t *testing.T) {
	fresh, _ := rankMixedFixture(false)
	assertRankMatchesSkeleton(t, "mixed fresh names", fresh, true)
	colliding, _ := rankMixedFixture(true)
	assertRankMatchesSkeleton(t, "mixed colliding names", colliding, true)

	type twiceKey struct {
		A int
		L any
		R any
	}
	var twices []twiceKey
	for _, v := range []int{3, -4, 100} {
		twices = append(twices, twiceKey{A: v, L: 7, R: 7})
		twices = append(twices, twiceKey{A: v, L: "s", R: "s"})
		twices = append(twices, twiceKey{A: v, L: 7, R: "s"})
	}
	assertRankMatchesSkeleton(t, "repeated dynamic type", mkKeys(twices), true)

	// grammatical model for fresh-name scalar dynamics: hand-rendered
	// descriptor literals over the format tables
	descInt := renderDescLitModel(8, "int", renderBareArgModel(8))
	descString := renderDescLitModel(12, "string", nil)
	descBool := renderDescLitModel(7, "bool", nil)
	descF64 := renderDescLitModel(10, "float64", renderBareArgModel(8))
	type dynModel struct {
		dyn  any
		skel []byte
	}
	fix := []dynModel{
		{nil, []byte{0x03}},
		{5, concatModel(descInt, renderIntModel(5))},
		{-7, concatModel(descInt, renderIntModel(-7))},
		{int64(12), concatModel(descInt, renderIntModel(12))},
		{"abc", concatModel(descString, renderStringSkel("abc"))},
		{strings.Repeat("a", 12), concatModel(descString, renderStringSkel(strings.Repeat("a", 12)))},
		{true, concatModel(descBool, renderBoolModel(true))},
		{3.5, concatModel(descF64, renderF64Model(3.5))},
	}
	keys := make([]reflect.Value, len(fix))
	models := make([][]byte, len(fix))
	for i, f := range fix {
		keys[i] = boxedAny(f.dyn)
		models[i] = f.skel
	}
	assertRankMatchesModel(t, "mixed scalar dynamics", keys, models)
}

func concatModel(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// rankRec is a self-referential dynamic key: the descriptor walk closes
// through a REF token.
type rankRec struct {
	V int
	N *rankRec
}

// TestRankCyclicAndRecursiveDynamics: cyclic pointer keys (collapse to
// the nil marker) and a recursive dynamic type (descriptor back-edge).
func TestRankCyclicAndRecursiveDynamics(t *testing.T) {
	self1 := &rankRec{V: 1}
	self1.N = self1
	self2 := &rankRec{V: 2}
	self2.N = self2
	tail := &rankRec{V: 3}
	mid := &rankRec{V: 4, N: tail}
	keys := []*rankRec{nil, self1, self2, mid, tail, &rankRec{V: 5}}
	assertRankMatchesSkeleton(t, "cyclic keys", mkKeys(keys), true)

	ifaceKeys := make([]reflect.Value, 0, len(keys)+3)
	for _, k := range keys {
		ifaceKeys = append(ifaceKeys, boxedAny(k))
	}
	ifaceKeys = append(ifaceKeys, boxedAny(7), boxedAny("z"), boxedAny(nil))
	assertRankMatchesSkeleton(t, "recursive dynamics", ifaceKeys, true)
}

// TestRankTieClassesAndMultiSeedStable: address-distinct equal pointer
// keys (pointer-sequence phase) and multi-seed stability of tie-rich
// maps.
func TestRankTieClassesAndMultiSeedStable(t *testing.T) {
	type tieNode struct {
		S string
		P *tieNode
	}
	n1 := &tieNode{S: "k"}
	n2 := &tieNode{S: "k"}
	n3 := &tieNode{S: "k"}
	c1 := &tieNode{S: "c"}
	c1.P = c1
	c2 := &tieNode{S: "c"}
	c2.P = c2
	c3 := &tieNode{S: "d"}
	c3.P = c3
	keys := []*tieNode{n1, n2, n3, c1, c2, c3, nil}
	assertRankMatchesSkeleton(t, "tie keys", mkKeys(keys), true)

	vals := []*tieNode{n1, n2, n3, c1, c2, c3}
	runSeeds := func(val func(i int) int) {
		t.Helper()
		var first []byte
		for seed := range int64(16) {
			rng := rand.New(rand.NewSource(seed))
			perm := rng.Perm(len(vals))
			m := make(map[*tieNode]int, len(vals))
			for _, j := range perm {
				m[vals[j]] = val(j)
			}
			b, err := Marshal(m)
			if err != nil {
				t.Fatalf("tie map marshal: %v", err)
			}
			if first == nil {
				first = b
				continue
			}
			if !bytes.Equal(b, first) {
				t.Fatalf("seed %d: tie map stream diverges from seed 0", seed)
			}
		}
	}
	// distinct values: the value-bytes phase decides the skeleton ties
	runSeeds(func(i int) int { return i })
	// equal values: the pointer-sequence phase decides them
	runSeeds(func(i int) int { return 42 })

	_, dyns := rankMixedFixture(true)
	var mfirst []byte
	for seed := range int64(16) {
		rng := rand.New(rand.NewSource(seed))
		perm := rng.Perm(len(dyns))
		m := make(map[any]int, len(dyns))
		for _, j := range perm {
			m[dyns[j]] = j
		}
		b, err := Marshal(m)
		if err != nil {
			t.Fatalf("mixed map marshal: %v", err)
		}
		if mfirst == nil {
			mfirst = b
			continue
		}
		if !bytes.Equal(b, mfirst) {
			t.Fatalf("seed %d: mixed map stream diverges from seed 0", seed)
		}
	}
}

// Two function-local types share one name with different structures:
// each call site yields a distinct reflect.Type whose descriptor name
// collides — the rebound scenario.
func rankConflictTypeA() any {
	type rbClash struct{ X int }
	return rbClash{X: 1}
}

func rankConflictTypeB() any {
	type rbClash struct{ Y string }
	return rbClash{Y: "s"}
}

// rankConflictErrField reads one field of the rank-walk verdict by
// name: the verdict type lives in the wire layer, which root tests do
// not import; the fields themselves are exported.
func rankConflictErrField(t *testing.T, err error, field string) string {
	t.Helper()
	rv := reflect.ValueOf(err)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		t.Fatalf("verdict %v: not a struct pointer", err)
	}
	f := rv.Elem().FieldByName(field)
	if !f.IsValid() {
		t.Fatalf("verdict %T: field %s missing", err, field)
	}
	return fmt.Sprint(f.Interface())
}

// TestRankDescConflictMatchesWriterRejection pins the error parity
// of the two rejection paths for a name rebound: rank-walk verdict
// fields, wire-render verdict, and both lifted codec errors agree.
func TestRankDescConflictMatchesWriterRejection(t *testing.T) {
	const wantKind = "register_conflict"
	const wantOff = "-1"
	const wantMsg = "gbon/wire: descriptor name rebound to a structurally different type: distinct types sharing one name are ambiguous (qualify the module path)"

	rw := newRankWalker(true)
	rw.resetKey()
	if err := rw.walk(boxedAny(rankConflictTypeA()), pathNode{idx: 0}); err != nil {
		t.Fatalf("rank walk first key: %v", err)
	}
	errRankWalk := rw.walk(boxedAny(rankConflictTypeB()), pathNode{idx: 1})
	if errRankWalk == nil {
		t.Fatal("rank walk: want a conflict rejection")
	}
	wantGot := rankConflictErrField(t, errRankWalk, "Got")
	if !strings.HasSuffix(wantGot, ".rbClash") {
		t.Fatalf("rank verdict Got = %q, want the local type name", wantGot)
	}
	for field, want := range map[string]string{
		"Kind": wantKind,
		"Off":  wantOff,
		"Msg":  wantMsg,
	} {
		if got := rankConflictErrField(t, errRankWalk, field); got != want {
			t.Fatalf("rank verdict %s = %q, want %q", field, got, want)
		}
	}

	_, errMap := Marshal(map[any]int{
		rankConflictTypeA(): 1,
		rankConflictTypeB(): 2,
	})
	_, errSlice := Marshal([]any{
		rankConflictTypeA(),
		rankConflictTypeB(),
	})
	for name, err := range map[string]error{"map keys": errMap, "slice values": errSlice} {
		if err == nil {
			t.Fatalf("%s: want a conflict rejection", name)
		}
		var ce *Error
		if !errors.As(err, &ce) {
			t.Fatalf("%s: no core Error in %v", name, err)
		}
		if ce.class != wantKind {
			t.Fatalf("%s: class %q, want %q", name, ce.class, wantKind)
		}
		if fmt.Sprint(ce.Offset) != wantOff {
			t.Fatalf("%s: offset %d, want %s", name, ce.Offset, wantOff)
		}
		if fmt.Sprint(ce.Got) != wantGot {
			t.Fatalf("%s: got %v, want %q", name, ce.Got, wantGot)
		}
		if ce.err.Error() != wantMsg {
			t.Fatalf("%s: detail %q, want %q", name, ce.err.Error(), wantMsg)
		}
	}
}
