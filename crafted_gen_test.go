package gbon_test

// Crafted-stream generator: a grammatical
// token-builder over the wire format. The stream grammar — header,
// DESC context, value tokens, minimal ARG forms, intern-space ids in DFS
// preorder — is the single source of the bytes; ids are assigned by
// the builder (no hand-written id literals in valid streams), and
// every stream carries a constructive oracle: a value, ErrFormat, or
// ErrBudget with a message substring.

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// craft is a stream builder: bytes plus the intern-space model
// (one id space for strings, descriptors, backings, maps, objects).
type craft struct {
	buf    []byte
	nextID uint64
	strs   map[string]uint64
	descs  map[string]uint64
}

// craftRec is a handle to one intern-space record (its assigned id).
type craftRec struct{ id uint64 }

func newCraft() *craft {
	c := &craft{strs: map[string]uint64{}, descs: map[string]uint64{}}
	c.buf = append(c.buf, 0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00) // stream header
	return c
}

func (c *craft) alloc() craftRec {
	r := craftRec{id: c.nextID}
	c.nextID++
	return r
}

// arg appends a bare minimal ARG: inline ≤ 11, else u8/u16/u32/u64.
func (c *craft) arg(n uint64) {
	switch {
	case n <= 11:
		c.buf = append(c.buf, byte(n))
	case n <= 0xFF:
		c.buf = append(c.buf, 0x0C, byte(n))
	case n <= 0xFFFF:
		c.buf = append(c.buf, 0x0D, byte(n>>8), byte(n))
	case n <= 0xFFFFFFFF:
		c.buf = append(c.buf, 0x0E, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default:
		c.buf = append(c.buf, 0x0F, byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
}

func zz(n int64) uint64 { return uint64(n<<1) ^ uint64(n>>63) }

// tokenArg writes a token whose low nibble is the minimal ARG form of n
// (class<<4 | form, then the payload).
func (c *craft) tokenArg(class, n uint64) {
	switch {
	case n <= 11:
		c.buf = append(c.buf, byte(class<<4|n))
	case n <= 0xFF:
		c.buf = append(c.buf, byte(class<<4|0x0C), byte(n))
	case n <= 0xFFFF:
		c.buf = append(c.buf, byte(class<<4|0x0D), byte(n>>8), byte(n))
	case n <= 0xFFFFFFFF:
		c.buf = append(c.buf, byte(class<<4|0x0E), byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default:
		c.buf = append(c.buf, byte(class<<4|0x0F), byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
}

// --- value tokens ---

func (c *craft) nilTok(sel int) { c.buf = append(c.buf, byte(sel)) } // class 0 high nibble zero
func (c *craft) boolTok(v bool) {
	b := byte(0x10)
	if v {
		b = 0x11
	}
	c.buf = append(c.buf, b)
}
func (c *craft) intTok(n int64)   { c.tokenArg(0x2, zz(n)) }
func (c *craft) uintTok(n uint64) { c.tokenArg(0x3, n) }
func (c *craft) float32Tok(f float32) {
	c.buf = append(c.buf, 0x40)
	u := math.Float32bits(f)
	c.buf = append(c.buf, byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
}
func (c *craft) float64Tok(f float64) {
	c.buf = append(c.buf, 0x41)
	u := math.Float64bits(f)
	c.buf = append(c.buf, byte(u>>56), byte(u>>48), byte(u>>40), byte(u>>32),
		byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
}
func (c *craft) complex64Tok(v complex64) {
	re, im := math.Float32bits(real(v)), math.Float32bits(imag(v))
	c.buf = append(c.buf, 0x50,
		byte(re>>24), byte(re>>16), byte(re>>8), byte(re),
		byte(im>>24), byte(im>>16), byte(im>>8), byte(im))
}

func (c *craft) complex128Tok(v complex128) {
	c.buf = append(c.buf, 0x51)
	for _, u := range [...]uint64{math.Float64bits(real(v)), math.Float64bits(imag(v))} {
		c.buf = append(c.buf, byte(u>>56), byte(u>>48), byte(u>>40), byte(u>>32),
			byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
	}
}

// strPos writes a string position: literal on first encounter,
// REF on repeat. The length rides the token's low nibble (inline ≤ 11,
// minimal forms); interning follows the unified id space.
func (c *craft) strPos(s string) {
	if id, ok := c.strs[s]; ok {
		c.tokenArg(0xC, id)
		return
	}
	c.strs[s] = c.nextID
	c.alloc()
	n := uint64(len(s))
	switch {
	case n <= 11:
		c.buf = append(c.buf, 0x60|byte(n))
	case n <= 0xFF:
		c.buf = append(c.buf, 0x6C, byte(n))
	case n <= 0xFFFF:
		c.buf = append(c.buf, 0x6D, byte(n>>8), byte(n))
	case n <= 0xFFFFFFFF:
		c.buf = append(c.buf, 0x6E, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default:
		c.buf = append(c.buf, 0x6F, byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	c.buf = append(c.buf, s...)
}

// strWide writes a STRING literal with a forced width form carrying the
// value — the wide-form injector (leading zeros are non-minimal).
func (c *craft) strWide(form byte, val uint64, body string) {
	c.buf = append(c.buf, 0x60|form)
	switch form {
	case 0x0C:
		c.buf = append(c.buf, byte(val))
	case 0x0D:
		c.buf = append(c.buf, byte(val>>8), byte(val))
	case 0x0E:
		c.buf = append(c.buf, byte(val>>24), byte(val>>16), byte(val>>8), byte(val))
	case 0x0F:
		c.buf = append(c.buf, byte(val>>56), byte(val>>48), byte(val>>40), byte(val>>32),
			byte(val>>24), byte(val>>16), byte(val>>8), byte(val))
	}
	c.buf = append(c.buf, body...)
}

// blobRec writes a BLOB record header: registers the record id
// before the body, then E raw bytes follow immediately. L rides
// the token nibble; E is a bare argument.
func (c *craft) blobRec(L, E uint64, dense []byte) craftRec {
	r := c.alloc()
	c.tokenArg(0x7, L)
	c.arg(E)
	c.buf = append(c.buf, dense...)
	return r
}

// arrayRec writes an ARRAY record header; E element tokens follow.
func (c *craft) arrayRec(L, E uint64) craftRec {
	r := c.alloc()
	c.tokenArg(0x8, L)
	c.arg(E)
	return r
}

// mapRec writes a MAP record header; 2·count tokens follow.
func (c *craft) mapRec(n uint64) craftRec {
	r := c.alloc()
	c.tokenArg(0xA, n)
	return r
}

// ptrRec reserves an object-record id (pointer target).
func (c *craft) ptrRec() craftRec { return c.alloc() }

func (c *craft) structTok() { c.buf = append(c.buf, 0xB0) }

func (c *craft) refTok(r craftRec) { c.tokenArg(0xC, r.id) }

// refUnregistered injects a REF to an id outside the intern space — the
// semantic-invalid injector; never used for valid streams.
func (c *craft) refUnregistered(id uint64) { c.tokenArg(0xC, id) }

// view tokens: the form rides the low nibble; id/off/len/cap are
// bare arguments after the first byte.
func (c *craft) view0(r craftRec) {
	c.buf = append(c.buf, 0x90)
	c.arg(r.id)
}
func (c *craft) view1(r craftRec, off, length uint64) {
	c.buf = append(c.buf, 0x91)
	c.arg(r.id)
	c.arg(off)
	c.arg(length)
}
func (c *craft) view2(r craftRec, off, length, capacity uint64) {
	c.buf = append(c.buf, 0x92)
	c.arg(r.id)
	c.arg(off)
	c.arg(length)
	c.arg(capacity)
}

// --- type descriptors ---

// cDesc is a descriptor tree for the builder. Names are plain strings:
// typed-target decoding matches them by name (codec-layer concern).
type cDesc struct {
	kind   byte // 0..14
	name   string
	width  uint64 // INT/UINT {1,2,4,8}, FLOAT/COMPLEX {4,8}
	length uint64 // ARRAY N
	tag    uint64 // CODER
	fields []cField
	refs   []*cDesc
}

type cField struct {
	name string
	typ  *cDesc
}

// descPos writes a type-ref position: literal on first encounter of the
// name, REF on repeat. The descriptor registers before its
// children, so recursive types close through REF.
func (c *craft) descPos(d *cDesc) {
	if id, ok := c.descs[d.name]; ok {
		c.tokenArg(0xC, id)
		return
	}
	c.descs[d.name] = c.nextID
	c.alloc()
	if d.kind <= 11 {
		c.buf = append(c.buf, 0xD0|d.kind)
	} else {
		c.buf = append(c.buf, 0xDC, d.kind) // u8 kind form, minimal
	}
	c.strPos(d.name)
	switch d.kind {
	case 0: // STRUCT: ARG n, then {field name, type-ref} pairs
		c.arg(uint64(len(d.fields)))
		for _, f := range d.fields {
			c.strPos(f.name)
			c.descPos(f.typ)
		}
	case 1, 4, 5: // SLICE, NAMED, POINTER: one type-ref
		c.descPos(d.refs[0])
	case 2: // ARRAY: ARG N, one type-ref
		c.arg(d.length)
		c.descPos(d.refs[0])
	case 3: // MAP: key then elem type-refs
		c.descPos(d.refs[0])
		c.descPos(d.refs[1])
	case 8, 9, 10, 11, 14: // INT/UINT/FLOAT/COMPLEX width, CODER tag
		c.arg(d.width + d.tag)
	default: // BOOL, INTERFACE, STRING, BLOB: empty body
	}
}

// descRawWidth writes a descriptor literal with a forced width ARG —
// the boundary injector for width thresholds (valid {1,2,4,8}, invalid
// {0,3,5} / FLOAT {2}); id order mirrors descPos.
func (c *craft) descRawWidth(kind byte, name string, width uint64) {
	c.descs[name] = c.nextID
	c.alloc()
	if kind <= 11 {
		c.buf = append(c.buf, 0xD0|kind)
	} else {
		c.buf = append(c.buf, 0xDC, kind)
	}
	c.strPos(name)
	c.arg(width)
}

// Descriptor helpers over canonical names (codec nameOf mirrors these
// strings; TestCraftedValidAllKinds pins the sync behaviorally).
var (
	dBool    = &cDesc{kind: 7, name: "bool"}
	dString  = &cDesc{kind: 12, name: "string"}
	dBlob    = &cDesc{kind: 13, name: "[]byte"}
	dIface   = &cDesc{kind: 6, name: "interface {}"}
	dInt64   = &cDesc{kind: 8, name: "int64", width: 8}
	dIntT    = &cDesc{kind: 8, name: "int", width: 8} // rtNode.Val
	dInt32   = &cDesc{kind: 8, name: "int32", width: 4}
	dInt16   = &cDesc{kind: 8, name: "int16", width: 2}
	dInt8    = &cDesc{kind: 8, name: "int8", width: 1}
	dUint64  = &cDesc{kind: 9, name: "uint64", width: 8}
	dFloat32 = &cDesc{kind: 10, name: "float32", width: 4}
	dFloat64 = &cDesc{kind: 10, name: "float64", width: 8}
	dC64     = &cDesc{kind: 11, name: "complex64", width: 4}
	dC128    = &cDesc{kind: 11, name: "complex128", width: 8}
)

func dUint(w uint64, name string) *cDesc {
	return &cDesc{kind: 9, name: name, width: w}
}
func dSlice(elem *cDesc) *cDesc {
	return &cDesc{kind: 1, name: "[]" + elem.name, refs: []*cDesc{elem}}
}
func dArray(n uint64, elem *cDesc) *cDesc {
	return &cDesc{kind: 2, name: fmt.Sprintf("[%d]%s", n, elem.name), length: n, refs: []*cDesc{elem}}
}
func dMap(key, elem *cDesc) *cDesc {
	return &cDesc{kind: 3, name: "map[" + key.name + "]" + elem.name, refs: []*cDesc{key, elem}}
}
func dPtr(elem *cDesc) *cDesc {
	return &cDesc{kind: 5, name: "*" + elem.name, refs: []*cDesc{elem}}
}
func dNamed(name string, base *cDesc) *cDesc {
	return &cDesc{kind: 4, name: name, refs: []*cDesc{base}}
}
func dCoder(name string, tag uint64) *cDesc {
	return &cDesc{kind: 14, name: name, tag: tag}
}
func dStructT(name string, fields ...cField) *cDesc {
	return &cDesc{kind: 0, name: name, fields: fields}
}

// --- oracle ---

// craftCase is one generated stream plus its constructive oracle:
// a decoded value, or an error that must be exactly one sentinel
// (optionally excluding another — the strict-error-class form).
type craftCase struct {
	name      string
	subclass  string // coverage-class label: pins class counts, drives fuzz seeding
	stream    []byte
	setup     func(dec *gbon.Decoder) // nil → stateless Unmarshal path
	limits    *gbon.Limits
	newTarget func() any
	wantVal   any
	wantNil   bool // oracle: target must decode to the nil of its kind
	wantErrIs error
	notErrIs  error
	wantClass string // expected gbon.Error class ID (class assert, no text matching)
}

// runCrafted drives one case against the real decoder (in-process seam:
// bytes into Unmarshal/Decode). A panic fails the test (never panic);
// hang protection is the suite timeout — streams are millisecond-class.
func runCrafted(t *testing.T, tc craftCase) {
	t.Helper()
	target := tc.newTarget()
	var err error
	if tc.setup != nil || tc.limits != nil {
		dec := gbon.NewDecoder(bytes.NewReader(tc.stream))
		if tc.limits != nil {
			dec.SetLimits(*tc.limits)
		}
		if tc.setup != nil {
			tc.setup(dec)
		}
		err = dec.Decode(target)
	} else {
		err = gbon.Unmarshal(tc.stream, target)
	}
	if tc.wantErrIs != nil {
		if err == nil {
			t.Fatalf("%s: decoded without error, want %v", tc.name, tc.wantErrIs)
		}
		if !errors.Is(err, tc.wantErrIs) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.wantErrIs)
		}
		if tc.notErrIs != nil && errors.Is(err, tc.notErrIs) {
			t.Fatalf("%s: err = %v: error-class drift (must not wrap %v)", tc.name, err, tc.notErrIs)
		}
		if tc.wantClass != "" {
			var ae *gbon.Error
			if !errors.As(err, &ae) {
				t.Fatalf("%s: err = %v, want *gbon.Error (class %q)", tc.name, err, tc.wantClass)
			}
			if ae.Class() != tc.wantClass {
				t.Fatalf("%s: class = %q, want %q (err %v)", tc.name, ae.Class(), tc.wantClass, err)
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", tc.name, err)
	}
	got := reflect.ValueOf(target).Elem()
	if tc.wantNil {
		switch got.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
			if !got.IsNil() {
				t.Fatalf("%s: decoded %s, want nil", tc.name, safeDescValue(got))
			}
		default:
			t.Fatalf("%s: wantNil on non-nilable kind %s", tc.name, got.Kind())
		}
		return
	}
	if !equiv(reflect.ValueOf(tc.wantVal), got) {
		t.Fatalf("%s: value mismatch:\n got %s\nwant %s", tc.name, safeDescValue(got), safeDescValue(tc.wantVal))
	}
}

// ptrTarget adapts a literal into a target factory.
func ptrTo[T any](v T) func() any {
	return func() any { return &v }
}

// indexedArrayKeyMap builds the 12-pair oracle for the map-pair charge
// window: keys [8192]byte distinguished by their first byte (nonzero:
// a zero first byte would leave the array record's dense prefix E=1
// non-minimal).
func indexedArrayKeyMap() map[[8192]byte]uint8 {
	m := make(map[[8192]byte]uint8, 12)
	for i := range 12 {
		var k [8192]byte
		k[0] = byte(i + 1)
		m[k] = uint8(i)
	}
	return m
}

// craftedValidPrims builds class (a): one valid stream per value class
// per DESC kind 0..14 — the critical-combination table.
func craftedValidPrims() []craftCase {
	mk := func(name, subclass string, build func(c *craft), target func() any,
		want any, opts ...func(*craftCase)) craftCase {
		c := newCraft()
		build(c)
		tc := craftCase{name: name, subclass: subclass, stream: c.buf,
			newTarget: target, wantVal: want}
		for _, o := range opts {
			o(&tc)
		}
		return tc
	}
	withNilOracle := func(tc *craftCase) { tc.wantVal, tc.wantNil = nil, true }
	return []craftCase{
		// nil selectors — all four kinds
		mk("nil-ptr", "nil-selector", func(c *craft) { c.descPos(dPtr(dInt64)); c.nilTok(0) },
			ptrTo((*int64)(nil)), nil, withNilOracle),
		mk("nil-slice", "nil-selector", func(c *craft) { c.descPos(dSlice(dInt64)); c.nilTok(1) },
			ptrTo([]int64(nil)), nil, withNilOracle),
		mk("nil-map", "nil-selector", func(c *craft) { c.descPos(dMap(dString, dInt64)); c.nilTok(2) },
			ptrTo(map[string]int64(nil)), nil, withNilOracle),
		mk("nil-iface", "nil-selector", func(c *craft) { c.descPos(dIface); c.nilTok(3) },
			ptrTo(any(nil)), nil, withNilOracle),
		// BOOL, INT (negative zigzag), UINT above MaxInt64
		mk("bool-true", "bool", func(c *craft) { c.descPos(dBool); c.boolTok(true) },
			ptrTo(true), true),
		mk("int64-neg", "int", func(c *craft) { c.descPos(dInt64); c.intTok(-5) },
			ptrTo(int64(-5)), int64(-5)),
		mk("int8", "int-width", func(c *craft) { c.descPos(dInt8); c.intTok(100) },
			ptrTo(int8(100)), int8(100)),
		mk("int16", "int-width", func(c *craft) { c.descPos(dInt16); c.intTok(300) },
			ptrTo(int16(300)), int16(300)),
		mk("int32", "int-width", func(c *craft) { c.descPos(dInt32); c.intTok(70000) },
			ptrTo(int32(70000)), int32(70000)),
		mk("uint64-big", "uint", func(c *craft) { c.descPos(dUint64); c.uintTok(1 << 63) },
			ptrTo(uint64(1)<<63), uint64(1)<<63),
		// FLOAT both forms (NaN payload survives bitwise), COMPLEX both
		mk("float32", "float", func(c *craft) {
			c.descPos(dFloat32)
			c.float32Tok(math.Float32frombits(0x7FC00042))
		}, ptrTo(float32(0)), math.Float32frombits(0x7FC00042)),
		mk("float64", "float", func(c *craft) { c.descPos(dFloat64); c.float64Tok(math.Float64frombits(0x7FF8000000000042)) },
			ptrTo(float64(0)), math.Float64frombits(0x7FF8000000000042)),
		mk("complex64", "complex", func(c *craft) { c.descPos(dC64); c.complex64Tok(1 + 2i) },
			ptrTo(complex64(0)), complex64(1+2i)),
		mk("complex128", "complex", func(c *craft) { c.descPos(dC128); c.complex128Tok(-1.5 - 0.5i) },
			ptrTo(complex128(0)), complex128(-1.5-0.5i)),
	}
}

// craftedValidContainers continues the valid class: reference records —
// strings with REF repeats, blobs with view forms, arrays, maps (KO
// order), structs, NAMED, POINTER dereference, CODER kind 14.
func craftedValidContainers() []craftCase {
	mk := func(name, subclass string, build func(c *craft), target func() any,
		want any, opts ...func(*craftCase)) craftCase {
		c := newCraft()
		build(c)
		tc := craftCase{name: name, subclass: subclass, stream: c.buf,
			newTarget: target, wantVal: want}
		for _, o := range opts {
			o(&tc)
		}
		return tc
	}
	five := int64(5)
	p5 := &five
	w22 := make([]byte, 2, 4)
	copy(w22, "bc") // window [1,3) over cap 4: len 2, cap 4
	i20 := make([]int64, 2, 3)
	i20[0] = 2 // view [1,3) cap 3 over {1,2,0,0}: elements 2,0
	return []craftCase{
		// STRING + REF repeat: second position is a REF token.
		mk("string-ref-repeat", "string-intern", func(c *craft) {
			c.descPos(dStructT(qn(strBox{}), cField{"A", dString}, cField{"B", dString}))
			c.structTok()
			c.strPos("xx")
			c.strPos("xx")
		}, ptrTo(strBox{A: "x", B: "x"}), strBox{A: "xx", B: "xx"}),
		// BLOB with trailing-zero elision + all three view forms
		mk("blob-view0", "view-form0", func(c *craft) {
			c.descPos(dBlob)
			r := c.blobRec(6, 3, []byte("abc"))
			c.view0(r)
		}, ptrTo([]byte{1, 2, 3, 4, 5, 6}), []byte("abc\x00\x00\x00")),
		mk("blob-view1", "view-form1", func(c *craft) {
			c.descPos(dBlob)
			r := c.blobRec(6, 6, []byte("abcdef"))
			c.view1(r, 1, 2)
		}, ptrTo([]byte{}), []byte("bc")),
		mk("blob-view2", "view-form2", func(c *craft) {
			c.descPos(dBlob)
			r := c.blobRec(6, 6, []byte("abcdef"))
			c.view2(r, 1, 2, 4)
		}, ptrTo([]byte{}), w22),
		// ARRAY with dense prefix E < L + view over [1,3) cap 3
		mk("slice-view2", "view-form2-slice", func(c *craft) {
			c.descPos(dSlice(dInt64))
			r := c.arrayRec(4, 2)
			c.intTok(1)
			c.intTok(2)
			c.view2(r, 1, 2, 3)
		}, ptrTo([]int64{9, 9}), i20),
		mk("array-fixed", "array-record", func(c *craft) {
			c.descPos(dArray(3, dInt64))
			c.arrayRec(3, 1)
			c.intTok(7)
		}, ptrTo([3]int64{}), [3]int64{7}),
		// MAP with canonical KO order: pairs sorted by encoded
		// key bytes — "a" before "b", zz(-1) before zz(1).
		mk("map-string-int", "map-record", func(c *craft) {
			c.descPos(dMap(dString, dInt64))
			c.mapRec(2)
			c.strPos("a")
			c.intTok(1)
			c.strPos("b")
			c.intTok(2)
		}, ptrTo(map[string]int64{}), map[string]int64{"a": 1, "b": 2}),
		mk("map-int-int", "map-int-keys", func(c *craft) {
			c.descPos(dMap(dInt64, dInt64))
			c.mapRec(2)
			c.intTok(-1)
			c.intTok(1)
			c.intTok(1)
			c.intTok(2)
		}, ptrTo(map[int64]int64{}), map[int64]int64{-1: 1, 1: 2}),
		// Map-pair charge window discrimination: under the same narrow
		// MaxBytes that rejects a 13-pair stream (overdraft class below),
		// 12 pairs of map[[8192]byte]uint8 decode — the pair charge is
		// count×(keysize+valuesize), not a blanket map rejection.
		mk("map-pairs-fit-window", "map-overdraft-window", func(c *craft) {
			c.descPos(dMap(dArray(8192, dUint(1, "uint8")), dUint(1, "uint8")))
			c.mapRec(12)
			for i := range 12 {
				c.arrayRec(8192, 1)
				c.uintTok(uint64(i + 1)) // key: first byte distinguishes, nonzero (minimal E)
				c.uintTok(uint64(i))     // value
			}
		}, ptrTo(map[[8192]byte]uint8{}), indexedArrayKeyMap(),
			func(tc *craftCase) {
				lim := gbon.Limits{MaxBytes: 100000}
				tc.limits = &lim
			}),
		// STRUCT with a recursive descriptor closing through REF
		mk("struct-recursive-desc", "struct-record", func(c *craft) {
			node := &cDesc{kind: 0, name: qn(rtNode{})}
			nodePtr := &cDesc{kind: 5, name: "*" + qn(rtNode{}), refs: []*cDesc{node}}
			node.fields = []cField{{"Val", dIntT}, {"Next", nodePtr}}
			c.descPos(node)
			c.structTok()
			c.intTok(9)
			c.nilTok(0)
		}, ptrTo(rtNode{}), rtNode{Val: 9}),
		// NAMED (kind 4)
		mk("named-int", "named-desc", func(c *craft) {
			c.descPos(dNamed(qn(myDur(0)), dInt64))
			c.intTok(42)
		}, ptrTo(myDur(0)), myDur(42)),
		// POINTER dereference: default branch reserves an object id, then
		// the pointee body follows.
		mk("ptr-deref", "pointer-record", func(c *craft) {
			c.descPos(dPtr(dInt64))
			c.ptrRec()
			c.intTok(5)
		}, func() any { return new(*int64) }, p5),
		// CODER kind 14 with the ccIntCoder test coder
		mk("coder-kind14", "coder-desc", func(c *craft) {
			c.descPos(dCoder(qn(ccA{}), 1))
			c.descPos(dInt64) // coder body: a self-described sub-value
			c.intTok(7)
		}, ptrTo(ccA{}), ccA{N: 7},
			func(tc *craftCase) {
				tc.setup = func(dec *gbon.Decoder) {
					if err := dec.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
						panic(err)
					}
					if err := dec.Register(int64(0)); err != nil {
						panic(err)
					}
				}
			}),
	}
}

// craftedInvalid builds the semantic-invalid class: REF-sort matrix,
// unregistered ids, overdraft, view desync. Every id is a builder
// handle (interned name, record handle) — no hand-written id literals;
// unregistered references are the
// explicit injector refUnregistered.
func craftedInvalid() []craftCase {
	// REF of a foreign record sort, position × sort matrix.
	// map-position base: mapShare64{X map, Y map} — X carries a MAP
	// record; Y injects the REF. Foreign sorts in-stream: interned
	// field-name string, the map descriptor.
	mapPos := func(name, sub string, idsel func(c *craft) uint64) craftCase {
		c := newCraft()
		md := dMap(dString, dInt64)
		c.descPos(dStructT("gbon_test.mapShare64", cField{"X", md}, cField{"Y", md}))
		c.structTok()
		c.mapRec(0) // X: empty non-nil map record
		c.refUnregistered(idsel(c))
		return craftCase{name: name, subclass: sub, stream: c.buf,
			newTarget: ptrTo(mapShare64{}), wantErrIs: gbon.ErrFormat,
			notErrIs: gbon.ErrBudget}
	}
	// (matrix assembled below after the bases)
	pointerPos := func(name, sub string, inject func(c *craft, ids map[string]uint64)) craftCase {
		c := newCraft()
		pd := dPtr(dInt64)
		md := dMap(dString, dInt64)
		sd := dSlice(dInt64)
		bd := dBlob
		c.descPos(dStructT("gbon_test.ptrForeign",
			cField{"M", md}, cField{"Z", sd}, cField{"D", bd}, cField{"P", pd}))
		c.structTok()
		c.strPos("k")
		m := c.mapRec(1) // M: one pair "k" → 1
		c.intTok(1)
		z := c.arrayRec(1, 0)
		d := c.blobRec(2, 0, nil)
		ids := map[string]uint64{
			"string": c.strs["P"], "desc": c.descs["*int64"],
			"map": m.id, "array": z.id, "blob": d.id,
		}
		inject(c, ids)
		return craftCase{name: name, subclass: sub, stream: c.buf,
			newTarget: func() any { return new(ptrForeign) }, wantErrIs: gbon.ErrFormat,
			notErrIs: gbon.ErrBudget}
	}
	stringPos := func(name, sub string, inject func(c *craft, ids map[string]uint64)) craftCase {
		c := newCraft()
		md := dMap(dString, dInt64)
		sd := dSlice(dInt64)
		c.descPos(dStructT("gbon_test.strForeign",
			cField{"M", md}, cField{"Z", sd}, cField{"D", dBlob},
			cField{"A", dString}, cField{"B", dString}))
		c.structTok()
		c.strPos("k")
		m := c.mapRec(1) // M: one pair "k" → 1
		c.intTok(1)
		z := c.arrayRec(1, 0)
		d := c.blobRec(2, 0, nil)
		c.strPos("v")
		ids := map[string]uint64{
			"desc": c.descs["string"],
			"map":  m.id, "array": z.id, "blob": d.id,
		}
		inject(c, ids)
		return craftCase{name: name, subclass: sub, stream: c.buf,
			newTarget: func() any { return new(strForeign) }, wantErrIs: gbon.ErrFormat,
			notErrIs: gbon.ErrBudget}
	}
	var out []craftCase
	// map-position: foreign sorts string / descriptor / array / blob
	// (map→map is legal sharing, excluded by construction).
	out = append(out,
		mapPos("map-pos→string-record", "ref-sort-matrix", func(c *craft) uint64 {
			return c.strs["X"]
		}),
		mapPos("map-pos→desc-record", "ref-sort-matrix", func(c *craft) uint64 {
			return c.descs["map[string]int64"]
		}))
	for _, tc := range []struct {
		name string
		sort string
	}{{"map-pos→array-record", "array"}, {"map-pos→blob-record", "blob"}} {
		t := tc
		c := newCraft()
		md := dMap(dString, dInt64)
		sd := dSlice(dInt64)
		if t.sort == "blob" {
			sd = dBlob
		}
		c.descPos(dStructT("gbon_test.mapForeignArr", cField{"Z", sd}, cField{"M", md}, cField{"Y", md}))
		c.structTok()
		var rec craftRec
		if t.sort == "array" {
			rec = c.arrayRec(1, 0)
		} else {
			rec = c.blobRec(1, 0, nil)
		}
		c.strPos("k")
		c.mapRec(1) // M: one pair "k" → 1
		c.intTok(1)
		c.refUnregistered(rec.id) // Y: REF to the foreign record
		out = append(out, craftCase{name: t.name, subclass: "ref-sort-matrix", stream: c.buf,
			newTarget: func() any { return new(mapForeignArr) }, wantErrIs: gbon.ErrFormat,
			notErrIs: gbon.ErrBudget})
	}
	// pointer-position: all five foreign sorts.
	for _, s := range []string{"string", "desc", "array", "blob", "map"} {
		sort := s
		out = append(out, pointerPos("ptr-pos→"+sort+"-record", "ref-sort-matrix",
			func(c *craft, ids map[string]uint64) { c.refUnregistered(ids[sort]) }))
	}
	// string-position: four foreign sorts (string→string is the legal
	// intern repeat, covered by class (a)).
	for _, s := range []string{"desc", "array", "blob", "map"} {
		sort := s
		out = append(out, stringPos("str-pos→"+sort+"-record", "ref-sort-matrix",
			func(c *craft, ids map[string]uint64) { c.refUnregistered(ids[sort]) }))
	}
	// REF to an id outside the intern space — forward (unregistered
	// at the REF point) and far beyond next (never appears).
	{
		c := newCraft()
		c.descPos(dMap(dString, dInt64))
		c.refUnregistered(c.nextID + 5) // forward: unregistered at this point
		out = append(out, craftCase{name: "ref-unregistered-forward", subclass: "ref-unregistered",
			stream: c.buf, newTarget: ptrTo(map[string]int64{}),
			wantErrIs: gbon.ErrFormat, wantClass: "bad_ref"})
	}
	{
		c := newCraft()
		c.descPos(dMap(dString, dInt64))
		c.refUnregistered(100) // far beyond any next id of this stream
		out = append(out, craftCase{name: "ref-unregistered-beyond", subclass: "ref-unregistered",
			stream: c.buf, newTarget: ptrTo(map[string]int64{}),
			wantErrIs: gbon.ErrFormat, wantClass: "bad_ref"})
	}
	// Overdraft L·es > MaxBytes under a narrow budget (SetLimits)
	// — ErrBudget before the allocation; L, es are parameters.
	for _, tc := range []struct {
		name   string
		blob   bool
		L      uint64
		budget int
	}{
		{"overdraft-blob-es1", true, 100, 32},
		{"overdraft-slice-es8", false, 8, 32},
	} {
		t := tc
		c := newCraft()
		var r craftRec
		if t.blob {
			c.descPos(dBlob)
			r = c.blobRec(t.L, 0, nil)
		} else {
			c.descPos(dSlice(dInt64))
			r = c.arrayRec(t.L, 0)
		}
		c.view0(r)
		target := func() any { return new([]int64) }
		if t.blob {
			target = func() any { return new([]byte) }
		}
		lim := gbon.Limits{MaxBytes: t.budget}
		out = append(out, craftCase{name: t.name, subclass: "overdraft",
			stream: c.buf, newTarget: target,
			wantErrIs: gbon.ErrBudget, limits: &lim})
	}
	// Map-pair overdraft (charge count×(keysize+valuesize)): the
	// default-budget form — 500K pairs of map[[8192]byte]uint8 claim
	// ~4.1 GB of derived pair memory against the 100 MB default MaxBytes.
	// The charge fires after the MAP header, before MakeMap and before
	// any pair token is read, so the stream is truncated there.
	{
		c := newCraft()
		c.descPos(dMap(dArray(8192, dUint(1, "uint8")), dUint(1, "uint8")))
		c.mapRec(500000)
		out = append(out, craftCase{name: "overdraft-map-pairs", subclass: "overdraft",
			stream: c.buf, newTarget: ptrTo(map[[8192]byte]uint8{}),
			wantErrIs: gbon.ErrBudget, wantClass: "budget_bytes"})
	}
	// Narrow-window leg of the same charge: MaxBytes=100000 rejects a
	// 13-pair stream (13×8193 charged) where 12 pairs decode fine
	// (the map-pairs-fit-window case in the valid class).
	{
		c := newCraft()
		c.descPos(dMap(dArray(8192, dUint(1, "uint8")), dUint(1, "uint8")))
		c.mapRec(13)
		lim := gbon.Limits{MaxBytes: 100000}
		out = append(out, craftCase{name: "overdraft-map-window", subclass: "overdraft",
			stream: c.buf, newTarget: ptrTo(map[[8192]byte]uint8{}),
			wantErrIs: gbon.ErrBudget, limits: &lim,
			wantClass: "budget_bytes"})
	}
	// Descriptor-graph overdraft: a crafted flat STRUCT descriptor with
	// 200 fields charges one node per field against MaxNodes — the
	// materialization amplification of descriptor parsing is bounded by
	// the node budget, not only by input bytes.
	{
		c := newCraft()
		fields := make([]cField, 200)
		for i := range fields {
			fields[i] = cField{fmt.Sprintf("F%d", i), dInt64}
		}
		c.descPos(dStructT("gbon_test.wideDescStruct", fields...))
		lim := gbon.Limits{MaxNodes: 100}
		out = append(out, craftCase{name: "desc-field-overdraft", subclass: "desc-overdraft",
			stream: c.buf, newTarget: ptrTo(map[string]int64{}),
			wantErrIs: gbon.ErrBudget, limits: &lim,
			wantClass: "budget_nodes"})
	}
	// View-desync axis: view geometry past L of the referenced record
	// — strictly ErrFormat (never a budget wrap, never a panic).
	{
		c := newCraft()
		c.descPos(dSlice(dInt64))
		r := c.arrayRec(2, 0)
		c.view2(r, 0, 2, 6) // cap 6 past L=2
		out = append(out, craftCase{name: "view-desync-cap-past-L", subclass: "view-desync",
			stream: c.buf, newTarget: ptrTo([]int64{}),
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget, wantClass: "bad_view"})
	}
	{
		// Two records of one slice type: a wide backing A (L=8) and a
		// narrow backing B (L=2); field B carries a window with A-grade
		// geometry over B — the foreign-record form.
		c := newCraft()
		sl := dSlice(dInt64)
		c.descPos(dStructT(qn(rtShare{}), cField{"X", sl}, cField{"Y", sl}))
		c.structTok()
		big := c.arrayRec(8, 0)
		c.view0(big)
		small := c.arrayRec(2, 0)
		c.view2(small, 0, 2, 6)
		out = append(out, craftCase{name: "view-desync-foreign-L", subclass: "view-desync",
			stream: c.buf, newTarget: func() any { return new(rtShare) },
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget, wantClass: "bad_view"})
	}
	{
		c := newCraft()
		c.descPos(dBlob)
		r := c.blobRec(3, 3, []byte("abc"))
		c.view1(r, 2, 2) // off+len 4 past L=3
		out = append(out, craftCase{name: "view-desync-blob-window", subclass: "view-desync",
			stream: c.buf, newTarget: ptrTo([]byte{}),
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget, wantClass: "bad_view"})
	}
	// Degenerate NAMED cycles: readDescLit interns a descriptor
	// before its name and body, so a crafted REF closes a self-loop on
	// the wire. Root unwrap and ARRAY element hops terminate on the
	// cycle detector: format error, never a hang and never a budget
	// overdraft — no Limits value legalizes a self-referential wrapper.
	ncTarget := func() any { return new(any) }
	{
		self := &cDesc{kind: 4, name: "gbon_test.selfNamed"}
		self.refs = []*cDesc{self}
		c := newCraft()
		c.descPos(self)
		out = append(out, craftCase{name: "root-named-self-cycle", subclass: "named-cycle",
			stream: c.buf, newTarget: ncTarget,
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget})
	}
	{
		a := &cDesc{kind: 4, name: "gbon_test.namedA"}
		b := &cDesc{kind: 4, name: "gbon_test.namedB"}
		a.refs, b.refs = []*cDesc{b}, []*cDesc{a}
		c := newCraft()
		c.descPos(a)
		out = append(out, craftCase{name: "root-named-mutual-cycle", subclass: "named-cycle",
			stream: c.buf, newTarget: ncTarget,
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget})
	}
	{
		// repro-shaped tail: a REF series onto the interned root
		// descriptor id (the named self-loop form); the
		// tail never reaches the reader — the cycle rejects first
		self := &cDesc{kind: 4, name: "gbon_test.selfNamed"}
		self.refs = []*cDesc{self}
		c := newCraft()
		c.descPos(self)
		for range 5 {
			c.refUnregistered(c.descs["gbon_test.selfNamed"])
		}
		out = append(out, craftCase{name: "root-named-cycle-repro-tail", subclass: "named-cycle",
			stream: c.buf, newTarget: ncTarget,
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget})
	}
	{
		// ARRAY element hops: the slice's element descriptor closes the
		// self-loop (decodeElems site); a typed named target passes the
		// strict match — match's visiting set assumes a match on the
		// back edge — and reaches the element loop
		self := &cDesc{kind: 4, name: qn(ncSelf(0))}
		self.refs = []*cDesc{self}
		c := newCraft()
		c.descPos(dSlice(self))
		c.arrayRec(1, 1)
		c.intTok(42)
		out = append(out, craftCase{name: "elems-named-self-cycle", subclass: "named-cycle",
			stream: c.buf, newTarget: ptrTo([]ncSelf(nil)),
			wantErrIs: gbon.ErrFormat, notErrIs: gbon.ErrBudget})
	}
	return out
}

// ptrForeign / strForeign / mapForeignArr / mapShare64: matrix fixture targets.
type ptrForeign struct {
	M map[string]int64
	Z []int64
	D []byte
	P *int64
}

type mapShare64 struct{ X, Y map[string]int64 }

type strForeign struct {
	M map[string]int64
	Z []int64
	D []byte
	A string
	B string
}

type mapForeignArr struct {
	Z []int64
	M map[string]int64
	Y map[string]int64
}

// rtShare is the shared-backing fixture target (two windows, one record).
type rtShare struct{ X, Y []int64 }

// strBox is the string-repeat fixture target; myDur the NAMED target.
type strBox struct{ A, B string }

type myDur int64

// ncSelf is the self-loop NAMED element target (decodeElems site).
type ncSelf int64

// craftedBoundary builds the boundary class: ARG-minimality edges and
// descriptor width thresholds.
func craftedBoundary() []craftCase {
	var out []craftCase
	// Minimal forms at every width boundary, carried by
	// string lengths (11→inline, 12→u8, 255→u8, 256→u16, 65535→u16,
	// 65536→u32; 2^32−1/2^32 are unreachable validly inside the test
	// byte budget — their forms are pinned by the wide-form rejects
	// below, which cover the full selector lattice).
	for _, n := range []int{11, 12, 255, 256, 65535, 65536} {
		s := bytes.Repeat([]byte{'a'}, n)
		c := newCraft()
		c.descPos(dString)
		c.strPos(string(s))
		out = append(out, craftCase{name: fmt.Sprintf("arg-minimal-len-%d", n),
			subclass: "arg-minimal", stream: c.buf, newTarget: ptrTo(""),
			wantVal: string(s)})
	}
	// UINT value-carrier at the u32/u64 boundary (cheap 9-byte
	// streams: 2^32−1 fits u32, 2^32 requires u64).
	for _, tc := range []struct {
		name string
		val  uint64
	}{
		{"arg-minimal-uint-2p32minus1", 0xFFFFFFFF},
		{"arg-minimal-uint-2p32", 0x100000000},
	} {
		c := newCraft()
		c.descPos(dUint64)
		c.uintTok(tc.val)
		out = append(out, craftCase{name: tc.name,
			subclass: "arg-minimal", stream: c.buf, newTarget: ptrTo(uint64(0)),
			wantVal: tc.val})
	}
	// Wide forms carrying narrow values (leading zeros).
	for _, tc := range []struct {
		name string
		form byte
		val  uint64
	}{
		{"arg-wide-u8-inline", 0x0C, 11},
		{"arg-wide-u16-u8", 0x0D, 255},
		{"arg-wide-u32-u16", 0x0E, 65535},
		{"arg-wide-u64-u32", 0x0F, 0xFFFFFFFF},
		{"arg-wide-u64-tiny", 0x0F, 5},
	} {
		t := tc
		c := newCraft()
		c.descPos(dString)
		c.strWide(t.form, t.val, "x")
		out = append(out, craftCase{name: t.name, subclass: "arg-wide",
			stream: c.buf, newTarget: ptrTo(""),
			wantErrIs: gbon.ErrFormat, wantClass: "malformed_arg"})
	}
	// Valid widths: UINT {1,2,4,8} (INT/FLOAT/COMPLEX widths are
	// covered by the class-(a) primitive targets).
	for _, w := range []uint64{1, 2, 4, 8} {
		width := w
		name := map[uint64]string{1: "uint8", 2: "uint16", 4: "uint32", 8: "uint64"}[width]
		c := newCraft()
		c.descPos(dUint(width, name))
		c.uintTok(7)
		var want any
		var target func() any
		switch width {
		case 1:
			want, target = uint8(7), ptrTo(uint8(0))
		case 2:
			want, target = uint16(7), ptrTo(uint16(0))
		case 4:
			want, target = uint32(7), ptrTo(uint32(0))
		default:
			want, target = uint64(7), ptrTo(uint64(0))
		}
		out = append(out, craftCase{name: fmt.Sprintf("uint-width-%d", width),
			subclass: "desc-width", stream: c.buf, newTarget: target, wantVal: want})
	}
	// Invalid widths: {3,5,0} for INT, {0} UINT, {2} FLOAT, {3} COMPLEX
	// (readDescLit checkDescWidth).
	for _, tc := range []struct {
		name  string
		kind  byte
		width uint64
	}{
		{"int-width-3", 8, 3},
		{"int-width-5", 8, 5},
		{"int-width-0", 8, 0},
		{"uint-width-0", 9, 0},
		{"float-width-2", 10, 2},
		{"complex-width-3", 11, 3},
	} {
		t := tc
		c := newCraft()
		name := "int64"
		if t.kind == 9 {
			name = "uint64"
		} else if t.kind == 10 {
			name = "float64"
		} else if t.kind == 11 {
			name = "complex128"
		}
		c.descRawWidth(t.kind, name, t.width)
		c.buf = append(c.buf, 0x21) // a value token follows the header
		out = append(out, craftCase{name: t.name, subclass: "desc-width",
			stream: c.buf, newTarget: ptrTo(int64(0)),
			wantErrIs: gbon.ErrFormat, wantClass: "malformed_op"})
	}
	return out
}

// skMap is the skipped-map target: the stream's Unknown field is skipped,
// Known carries a REF to the skipped map's record id.
type skMap struct{ Known map[string]int64 }

// TestCraftedSkippedMapRef: a REF from a known map-typed
// position to a map record consumed by skipValue — the map entry exists
// in the intern space but is never materialized — is a format error
// ("not materialized"), and the documented line is present.
func TestCraftedSkippedMapRef(t *testing.T) {
	c := newCraft()
	md := dMap(dString, dInt64)
	c.descPos(dStructT(qn(skMap{}),
		cField{"Unknown", md}, cField{"Known", md}))
	c.structTok()
	skipped := c.mapRec(0) // Unknown: parse-only skip, no materialization
	c.refUnregistered(skipped.id)
	var got skMap
	err := gbon.Unmarshal(c.buf, &got)
	if err == nil {
		t.Fatal("REF to skipped map decoded without error")
	}
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "bad_ref" {
		t.Fatalf("err = %v, want the not-materialized bad_ref class", err)
	}
}

// TestCraftedValidAllKinds: every stream of the
// valid classes decodes to its oracle — primitives by value, containers
// by structure, topological twins with identity intact.
func TestCraftedValidAllKinds(t *testing.T) {
	for _, tc := range append(craftedValidPrims(), craftedValidContainers()...) {
		t.Run(tc.name, func(t *testing.T) { runCrafted(t, tc) })
	}
	for _, tc := range craftedValidTopo() {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.newGot()
			dec := gbon.NewDecoder(bytes.NewReader(tc.stream))
			if tc.setup != nil {
				tc.setup(dec)
			}
			if err := dec.Decode(target); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			tc.verify(t, reflect.ValueOf(target).Elem().Interface())
		})
	}
}

// TestCraftedSemanticInvalid: REF-sort matrix,
// unregistered ids, overdrafts — each strictly its sentinel class.
func TestCraftedSemanticInvalid(t *testing.T) {
	for _, tc := range craftedInvalid() {
		t.Run(tc.name, func(t *testing.T) { runCrafted(t, tc) })
	}
}

// TestCraftedBoundary: ARG-minimality edges and
// descriptor width thresholds.
func TestCraftedBoundary(t *testing.T) {
	for _, tc := range craftedBoundary() {
		t.Run(tc.name, func(t *testing.T) { runCrafted(t, tc) })
	}
}

// TestCraftedClassCounts pins the per-class target counts (coverage is
// a fenced fixture, not an accident). Extension of the
// grammar or the target table is a conscious bump of these numbers.
func TestCraftedClassCounts(t *testing.T) {
	counts := map[string]int{}
	for _, tc := range append(craftedValidPrims(), craftedValidContainers()...) {
		counts[tc.subclass]++
	}
	for _, tc := range craftedValidTopo() {
		counts[tc.subclass]++
	}
	for _, tc := range craftedInvalid() {
		counts[tc.subclass]++
	}
	for _, tc := range craftedBoundary() {
		counts[tc.subclass]++
	}
	// Valid class: ≥1 per value class and per DESC kind 0..14 — the
	// critical-combination table.
	want := map[string]int{
		"nil-selector": 4, "bool": 1, "int": 1, "int-width": 3, "uint": 1,
		"float": 2, "complex": 2, "string-intern": 1,
		"view-form0": 1, "view-form1": 1, "view-form2": 1, "view-form2-slice": 1,
		"array-record": 1, "map-record": 1, "map-int-keys": 1,
		"map-overdraft-window": 1,
		"struct-record":        1, "named-desc": 1, "pointer-record": 1,
		"coder-desc": 1,
		"self-view":  1, "map-self": 1, "mutual": 1, "ptr-cycle": 1, "dag-shared": 1,
		"ref-sort-matrix": 13, "ref-unregistered": 2, "overdraft": 4, "view-desync": 3,
		"desc-overdraft": 1, "named-cycle": 4,
		"arg-minimal": 8, "arg-wide": 5,
		"desc-width": 10,
	}
	for sub, n := range want {
		if counts[sub] < n {
			t.Errorf("subclass %q: %d targets, want ≥ %d", sub, counts[sub], n)
		}
	}
	// total valid — the ≈29 critical combinations + the
	// invalid/boundary carriers.
	total := 0
	for _, n := range counts {
		total += n
	}
	if total < 29 {
		t.Fatalf("crafted corpus collapsed: %d total targets", total)
	}
}

// TestCraftedLegacyEquivalence: the generator reproduces the
// hand-computed g51 alloc-bomb stream byte-for-byte.
func TestCraftedLegacyEquivalence(t *testing.T) {
	c := newCraft()
	c.descPos(dSlice(dInt64))
	r := c.arrayRec(100000000, 0)
	c.view0(r)
	want := mustHex(t, g51Hex)
	if !bytes.Equal(c.buf, want) {
		t.Fatalf("generator g51 divergence:\n got % x\nwant % x", c.buf, want)
	}
}

// craftedSeeds returns one deterministic stream per coverage subclass —
// the fuzz corpus seeding set: generated, not hand-written, enumerated
// from the same class table the oracle tests walk (the f.Add loop in
// FuzzDecode).
func craftedSeeds() []craftCase {
	var out []craftCase
	seen := map[string]bool{}
	take := func(tcs []craftCase) {
		for _, tc := range tcs {
			if !seen[tc.subclass] {
				seen[tc.subclass] = true
				out = append(out, tc)
			}
		}
	}
	take(craftedValidPrims())
	take(craftedValidContainers())
	take(craftedInvalid())
	take(craftedBoundary())
	// topological cycle/DAG twins decode
	// into interface targets — seeded through their own carrier below.
	return out
}

// craftTopo is a valid crafted stream whose oracle is identity — shared
// backing, self-reference, mutual containers: mutation through one
// path must be observable through the other.
type craftTopo struct {
	name     string
	subclass string
	stream   []byte
	setup    func(dec *gbon.Decoder)
	newGot   func() any
	verify   func(t *testing.T, got any)
}

// craftedValidTopo builds the class (a) topological twins of the RT
// tests: REF-repeated records, view-over-own-backing (record-then-fill,
// map-self/mutual through REF, pointer cycles,
// DAG-shared backing.
func craftedValidTopo() []craftTopo {
	regAny := func(dec *gbon.Decoder) {
		if err := dec.Register(map[string]any{}, []any{}, int64(0), &rtNode{}); err != nil {
			panic(err)
		}
	}
	return []craftTopo{
		{
			name: "view-over-own-backing", subclass: "self-view",
			// []any{s0=int64, s1=view over this very array} — the slice
			// stored as its own element through an interface.
			stream: func() []byte {
				c := newCraft()
				sa := dSlice(dIface)
				c.descPos(sa)
				r := c.arrayRec(2, 2)
				c.descPos(dInt64)
				c.intTok(1)
				c.descPos(sa) // REF: "[]interface {}" already interned
				c.view0(r)    // element 1: window over the mid-fill backing
				c.view0(r)    // trailing view of the array record itself
				return c.buf
			}(),
			setup:  regAny,
			newGot: func() any { return new(any) },
			verify: func(t *testing.T, got any) {
				s := got.([]any)
				e, ok := s[1].([]any)
				if !ok || len(e) != 2 || cap(e) != 2 {
					t.Fatalf("self-window geometry: %s", safeDescValue(s[1]))
				}
				s[0] = int64(42)
				if e[0] != int64(42) {
					t.Fatal("self-view is not a window over the same backing")
				}
			},
		},
		{
			name: "map-self", subclass: "map-self",
			// map[string]any{"self": REF to the map record}.
			stream: func() []byte {
				c := newCraft()
				ms := dMap(dString, dIface)
				c.descPos(ms)
				m := c.mapRec(1)
				c.strPos("self")
				c.descPos(ms) // interface position: desc REF
				c.refTok(m)   // value: REF to the map record
				return c.buf
			}(),
			setup:  regAny,
			newGot: func() any { return new(any) },
			verify: func(t *testing.T, got any) {
				m := got.(map[string]any)
				self, ok := m["self"].(map[string]any)
				if !ok {
					t.Fatalf("m[self] dynamic type %T", m["self"])
				}
				// identical map object: the REF resolved to the record
				m["witness"] = int64(1)
				if self["witness"] != int64(1) {
					t.Fatal("map-self identity broken")
				}
			},
		},
		{
			name: "map-mutual", subclass: "mutual",
			// m1["b"] = m2; m2["a"] = m1 — two containers referencing each
			// other through REF (directional construction, not a random
			// back-edge).
			stream: func() []byte {
				c := newCraft()
				ms := dMap(dString, dIface)
				c.descPos(ms)
				m1 := c.mapRec(1)
				c.strPos("b")
				c.descPos(ms)
				c.mapRec(1) // m2: fresh map record (referenced inline)
				c.strPos("a")
				c.descPos(ms)
				c.refTok(m1)
				return c.buf
			}(),
			setup:  regAny,
			newGot: func() any { return new(any) },
			verify: func(t *testing.T, got any) {
				m1 := got.(map[string]any)
				m2 := m1["b"].(map[string]any)
				if back, ok := m2["a"].(map[string]any); !ok || back["b"] == nil {
					t.Fatalf("mutual cycle broken: %s", safeDescValue(m2))
				}
				m2["witness"] = int64(7)
				if m1["b"].(map[string]any)["witness"] != int64(7) {
					t.Fatal("mutual identity broken")
				}
			},
		},
		{
			name: "ptr-cycle", subclass: "ptr-cycle",
			// *rtNode whose Next points to itself (cycle closes
			// through a REF to the object record).
			stream: func() []byte {
				c := newCraft()
				node := &cDesc{kind: 0, name: qn(rtNode{})}
				nodePtr := &cDesc{kind: 5, name: "*" + qn(rtNode{}), refs: []*cDesc{node}}
				node.fields = []cField{{"Val", dIntT}, {"Next", nodePtr}}
				c.descPos(nodePtr)
				p := c.ptrRec()
				c.structTok()
				c.intTok(3)
				c.refTok(p)
				return c.buf
			}(),
			setup: regAny,
			newGot: func() any {
				return new(*rtNode)
			},
			verify: func(t *testing.T, got any) {
				n := got.(*rtNode)
				if n.Next != n || n.Val != 3 {
					t.Fatalf("pointer cycle broken: %s", safeDescValue(n))
				}
			},
		},
		{
			name: "dag-shared-backing", subclass: "dag-shared",
			// rtShare{X, Y}: two slice positions, one ARRAY record — Y is
			// a bare VIEW token over X's backing (REF-grade sharing).
			stream: func() []byte {
				c := newCraft()
				sl := dSlice(dInt64)
				c.descPos(dStructT(qn(rtShare{}), cField{"X", sl}, cField{"Y", sl}))
				c.structTok()
				r := c.arrayRec(2, 2)
				c.intTok(5)
				c.intTok(6)
				c.view0(r)
				c.view1(r, 1, 1) // Y: window [1,2) over the same backing
				return c.buf
			}(),
			newGot: func() any { return new(rtShare) },
			verify: func(t *testing.T, got any) {
				s := got.(rtShare)
				if len(s.Y) != 1 || s.Y[0] != 6 {
					t.Fatalf("Y window: %s", safeDescValue(s.Y))
				}
				s.Y[0] = 99 // Y[0] is X[1]: one backing, two windows
				if s.X[1] != 99 {
					t.Fatal("shared backing split into two")
				}
			},
		},
	}
}

// TestSafeDescCyclicCrafted: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicCrafted(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	s := safeDescValue(m)
	if len(s) == 0 || len(s) > descMaxRunes {
		t.Fatalf("safeDescValue render out of bounds (%d runes): %q", len(s), s)
	}
	if !strings.Contains(s, "<cycle>") {
		t.Fatalf("safeDescValue misses the cycle marker: %q", s)
	}
}
