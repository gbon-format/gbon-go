package gbon_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"fmt"
	"github.com/gbon-format/gbon-go"
)

// The equivalence operator — the single comparison
// source for the round-trip and property suites.
func equiv(a, b reflect.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case reflect.Bool:
		return a.Bool() == b.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() == b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return a.Uint() == b.Uint()
	case reflect.Float32:
		return math.Float32bits(float32(a.Float())) == math.Float32bits(float32(b.Float()))
	case reflect.Float64:
		return math.Float64bits(a.Float()) == math.Float64bits(b.Float())
	case reflect.Complex64:
		ca, cb := complex64(a.Complex()), complex64(b.Complex())
		return math.Float32bits(float32(real(ca))) == math.Float32bits(float32(real(cb))) &&
			math.Float32bits(float32(imag(ca))) == math.Float32bits(float32(imag(cb)))
	case reflect.Complex128:
		ca, cb := a.Complex(), b.Complex()
		return math.Float64bits(real(ca)) == math.Float64bits(real(cb)) &&
			math.Float64bits(imag(ca)) == math.Float64bits(imag(cb))
	case reflect.String:
		return a.String() == b.String()
	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() || a.Cap() != b.Cap() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !equiv(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !equiv(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		iter := a.MapRange()
		for iter.Next() {
			bv := b.MapIndex(iter.Key())
			if !bv.IsValid() || !equiv(iter.Value(), bv) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !equiv(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() && b.IsNil()
		}
		return equiv(a.Elem(), b.Elem())
	case reflect.Interface:
		return a.IsNil() && b.IsNil()
	}
	return false
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	data, err := gbon.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", safeDescValue(v), err)
	}
	out := reflect.New(reflect.TypeOf(v))
	if err := gbon.Unmarshal(data, out.Interface()); err != nil {
		t.Fatalf("Unmarshal(%s): %v", safeDescValue(v), err)
	}
	return out.Elem().Interface()
}

func assertEquiv(t *testing.T, v any) {
	t.Helper()
	out := roundTrip(t, v)
	if !equiv(reflect.ValueOf(v), reflect.ValueOf(out)) {
		t.Fatalf("round-trip mismatch:\n in  %s\n out %s", safeDescValue(v), safeDescValue(out))
	}
}

type rtNode struct {
	Val  int
	Next *rtNode
}

type twoPtr struct{ A, B *int }

type blankField struct {
	_ int
	X int
}

type boxAny struct{ V any }

type selfRef []selfRef

// Unexported named TYPES with fully exported fields are
// legal (the descriptor name is just a string).
type lowercase struct{ X int }

// RT-rt: every covered kind round-trips under the equivalence
// operator.
func TestRoundTripKinds(t *testing.T) {
	cases := []any{
		true, false,
		int(-1), int8(-128), int8(127), int16(-32768), int32(-2147483648),
		int64(math.MinInt64), int64(math.MaxInt64), uint(0), uint8(255),
		uint16(65535), uint32(4294967295), uint64(math.MaxUint64),
		"", "hi", "\xff\xfe\x00bad utf8", strings.Repeat("x", 300),
		[]byte(nil), []byte{}, []byte{0}, []byte{0xAB, 0xCD},
		[]int(nil), []int{}, []int{1, 2, 3},
		[]string{"a", "", "b"},
		[]float64{0, math.Copysign(0, -1), math.NaN()},
		[2]int64{1, 2}, [3]string{"", "x", ""},
		map[string]int(nil), map[string]int{}, map[string]int{"a": 1, "b": 2},
		map[int]string{1: "a", -5: "b"}, map[uint8]int{7: 7},
		map[float64]int{1.5: 1, math.Copysign(0, -1): 2, 0.0: 3},
		map[bool]int{true: 1, false: 2},
		map[complex128]int{complex(1, 2): 5},
		map[string][]int{"a": {1, 2}, "b": nil},
		Point{X: 1, Y: -2}, blankField{X: 9}, Celsius(-3.5), lowercase{X: 3},
		[]Point{{1, 2}, {3, 4}},
		[]Celsius{1.5, -1.5},
		map[string]Point{"p": {5, 6}},
		struct{ P *Point }{},
	}
	for _, v := range cases {
		assertEquiv(t, v)
	}
	// nil interface root: Marshal(nil) → nil.
	data, err := gbon.Marshal(nil)
	if err != nil {
		t.Fatalf("Marshal(nil): %v", err)
	}
	out := any(5)
	if err := gbon.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal(nil): %v", err)
	}
	if out != nil {
		t.Fatalf("want nil interface, got %s", safeDescValue(out))
	}
}

// Float rawbits round-trip — NaN payloads, ±0, subnormals.
func TestFloatRawbitsRT(t *testing.T) {
	bits64 := []uint64{
		0x7FF8000000000000, 0x7FF8000000000001, 0xFFF8000000000002,
		0x7FF0000000000000, 0xFFF0000000000000, 0x0000000000000000,
		0x8000000000000000, 0x0000000000000001, 0x000FFFFFFFFFFFFF,
		0x3FF0000000000000, 0x7FEFFFFFFFFFFFFF,
	}
	for _, b := range bits64 {
		assertEquiv(t, math.Float64frombits(b))
	}
	bits32 := []uint32{0x7FC00000, 0x7FC00001, 0xFF800000, 0x80000000, 0x00000001, 0x7F7FFFFF}
	for _, b := range bits32 {
		assertEquiv(t, math.Float32frombits(b))
	}
}

// RT-cycle: exact pointer sharing for cycles.
func TestCycleRoundTrip(t *testing.T) {
	a, b := rtNode{Val: 1}, rtNode{Val: 2}
	a.Next, b.Next = &b, &a
	data, err := gbon.Marshal(&a)
	if err != nil {
		t.Fatal(err)
	}
	var out *rtNode
	if err := gbon.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Next.Next != out {
		t.Fatal("cycle identity broken: out.Next.Next != out")
	}
	if out.Val != 1 || out.Next.Val != 2 {
		t.Fatalf("cycle values: %d %d", out.Val, out.Next.Val)
	}
}

// RT-selfref: two-phase — co-inductive for the value root, exact
// identity for the pointer root (documented dropout).
func TestSelfReferencingFill(t *testing.T) {
	var n rtNode
	n.Val = 7
	n.Next = &n

	data, err := gbon.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var out rtNode
	if err := gbon.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Next.Next != out.Next {
		t.Fatal("co-inductive self-reference broken")
	}
	if out.Next == &out {
		t.Fatal("value-rooted identity must be a documented dropout")
	}

	data2, err := gbon.Marshal(&n)
	if err != nil {
		t.Fatal(err)
	}
	var outp *rtNode
	if err := gbon.Unmarshal(data2, &outp); err != nil {
		t.Fatal(err)
	}
	if outp.Next != outp {
		t.Fatal("pointer-rooted identity broken: outp.Next != outp")
	}
}

// RT-identity: two fields on one target collapse to one
// reconstructed object.
func TestPointerIdentity(t *testing.T) {
	x := 42
	s := twoPtr{A: &x, B: &x}
	out := roundTrip(t, s).(twoPtr)
	if out.A != out.B {
		t.Fatal("A and B must point to one object")
	}
	*out.A = 7
	if *out.B != 7 {
		t.Fatal("mutation through A not visible in B")
	}
}

// RT-nil: nil-ness survives round-trip.
func TestNilVsEmpty(t *testing.T) {
	vals := []any{[]int(nil), []int{}, []byte(nil), []byte{},
		map[string]int(nil), map[string]int{}, (*int)(nil)}
	for _, v := range vals {
		out := roundTrip(t, v)
		a, b := reflect.ValueOf(v), reflect.ValueOf(out)
		if a.IsNil() != b.IsNil() {
			t.Fatalf("nil-ness changed for %T: %v → %v", v, a.IsNil(), b.IsNil())
		}
		if a.Kind() == reflect.Map && !b.IsNil() {
			m := reflect.MakeMap(b.Type())
			b.SetMapIndex(reflect.ValueOf("k").Convert(b.Type().Key()), reflect.New(b.Type().Elem()).Elem())
			_ = m
		}
	}
}

// RT-slice: fresh-owner form — len, cap, elements, zero tail.
func TestSliceFreshOwnerRT(t *testing.T) {
	s := make([]int, 2, 5)
	s[0] = 7
	out := roundTrip(t, s).([]int)
	if len(out) != 2 || cap(out) != 5 {
		t.Fatalf("len/cap: got %d/%d, want 2/5", len(out), cap(out))
	}
	if out[0] != 7 || out[1] != 0 {
		t.Fatalf("elements: %v", out)
	}
	full := out[:5]
	for i, v := range full[2:] {
		if v != 0 {
			t.Fatalf("zero tail broken at %d: %d", i, v)
		}
	}
}

// RT-map: primitive keys, bitwise float keys, usable after RT.
func TestMapPrimitiveKeysRT(t *testing.T) {
	m := map[float64]int{1.5: 1, math.Copysign(0, -1): 2, math.MaxFloat64: 4}
	out := roundTrip(t, m).(map[float64]int)
	if len(out) != 3 {
		t.Fatalf("len: %d", len(out))
	}
	if out[math.Copysign(0, -1)] != 2 {
		t.Fatal("negative-zero key lost")
	}
	out[2.5] = 9
	if out[2.5] != 9 {
		t.Fatal("map not writable after RT")
	}
}

// ST-stream: multi-value stream, header once, io.EOF at the end.
func TestStreamMultiValue(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	vals := []any{int(5), "str", []int{1, 2, 3}}
	for _, v := range vals {
		if err := enc.Encode(v); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if !bytes.HasPrefix(buf.Bytes(), hdr) {
		t.Fatal("stream must start with header")
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	for i, want := range vals {
		out := reflect.New(reflect.TypeOf(want))
		if err := dec.Decode(out.Interface()); err != nil {
			t.Fatalf("Decode %d: %v", i, err)
		}
		if !equiv(reflect.ValueOf(want), out.Elem()) {
			t.Fatalf("stream value %d mismatch", i)
		}
	}
	var extra int
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

// ST-budget: MaxSliceLen budget branch, zero-limits branch,
// negative misuse.
func TestSetLimitsBudget(t *testing.T) {
	mkBlob := func(lhex string) []byte {
		b, _ := hexDecode("67626F6E0000 DC0D665B5D62797465" + lhex)
		return b
	}
	huge := mkBlob("7F000400000000000000") // BLOB L=2^50, E=0
	budget := mkBlob("7C9600" + "9002")    // BLOB L=150, E=0, view form0 (id 2)

	dec := gbon.NewDecoder(bytes.NewReader(huge))
	dec.SetLimits(gbon.Limits{MaxSliceLen: 100})
	var b []byte
	err := dec.Decode(&b)
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("budget branch: want ErrBudget, got %v", err)
	}
	if err != nil {
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "budget_bytes" {
			t.Fatalf("budget branch: want class budget_bytes, got %v", err)
		}
	}

	arr := func() []byte {
		d, _ := hexDecode("67626F6E0000 D1655B5D696E74 D863696E7408" + "8F0004000000000000" + "00")
		return d
	}
	dec2 := gbon.NewDecoder(bytes.NewReader(arr()))
	dec2.SetLimits(gbon.Limits{MaxSliceLen: 100})
	var s []int
	if err := dec2.Decode(&s); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("array budget branch: want ErrBudget, got %v", err)
	}

	dec3 := gbon.NewDecoder(bytes.NewReader(budget))
	var b3 []byte
	if err := dec3.Decode(&b3); err != nil {
		t.Fatalf("zero-limits branch must decode: %v", err)
	}
	if len(b3) != 150 {
		t.Fatalf("zero-limits len: %d", len(b3))
	}

	dec4 := gbon.NewDecoder(bytes.NewReader(budget))
	dec4.SetLimits(gbon.Limits{MaxSliceLen: -1})
	if err := dec4.Decode(&b3); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("negative MaxSliceLen: want ErrUnsupported, got %v", err)
	}
}

// ST-match: desc↔target strict mismatch and width overflow.
func TestTargetMismatch(t *testing.T) {
	data, err := gbon.Marshal(1.0)
	if err != nil {
		t.Fatal(err)
	}
	var i int
	if err := gbon.Unmarshal(data, &i); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("float→int: want ErrFormat, got %v", err)
	}
	crafted, _ := hexDecode("67626F6E0000" + "D864696E743801" + "2D012C")
	var i8 int8
	err = gbon.Unmarshal(crafted, &i8)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("int8 width overflow: want ErrFormat, got %v", err)
	}
	var ov *gbon.Error
	if err != nil && !(errors.As(err, &ov) && ov.Class() == "overflow_value" && fmt.Sprint(ov.Got) == "150") {
		t.Fatalf("error must name the overflowing value: %v", err)
	}
}

func hexDecode(s string) ([]byte, error) {
	return hex.DecodeString(strings.ReplaceAll(s, " ", ""))
}

// ST-width: crafted INT values against the target's width-derived range —
// boundaries for int8/int16/int32 taken from the target type's size, not
// from the host platform's int. Each crafted stream pairs a narrow target
// descriptor with a width-8 value token, so the full int64 range reaches
// the range check.
func TestIntWidthBoundaries(t *testing.T) {
	const (
		magic  = "67626F6E0000"
		desc8  = "D864696E743801"   // DESC int8, width 1
		desc16 = "D865696E74313602" // DESC int16, width 2
		desc32 = "D865696E74333204" // DESC int32, width 4
	)
	mustDecode := func(t *testing.T, stream string, target, want any) {
		t.Helper()
		data, err := hexDecode(stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := gbon.Unmarshal(data, target); err != nil {
			t.Fatalf("in-range %T: unexpected error: %v", want, err)
		}
		if got := reflect.Indirect(reflect.ValueOf(target)).Interface(); got != want {
			t.Fatalf("%T: got %s, want %v", want, safeDescValue(got), want)
		}
	}
	mustOverflow := func(t *testing.T, stream string, target any, valueSub string) {
		t.Helper()
		data, err := hexDecode(stream)
		if err != nil {
			t.Fatal(err)
		}
		err = gbon.Unmarshal(data, target)
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("out-of-range: want ErrFormat, got %v", err)
		}
		var ov *gbon.Error
		if !errors.As(err, &ov) || ov.Class() != "overflow_value" || fmt.Sprint(ov.Got) != valueSub {
			t.Fatalf("error must name the overflowing value %q: %v", valueSub, err)
		}
	}

	t.Run("int8 boundary", func(t *testing.T) {
		mustDecode(t, magic+desc8+"2CFE", new(int8), int8(math.MaxInt8))
		mustOverflow(t, magic+desc8+"2D0100", new(int8), "128")
	})
	t.Run("int16 boundary", func(t *testing.T) {
		mustDecode(t, magic+desc16+"2DFFFE", new(int16), int16(math.MaxInt16))
		mustOverflow(t, magic+desc16+"2E00010000", new(int16), "32768")
	})
	t.Run("int32 boundary", func(t *testing.T) {
		mustDecode(t, magic+desc32+"2EFFFFFFFE", new(int32), int32(math.MaxInt32))
		mustOverflow(t, magic+desc32+"2F0000000100000000", new(int32), "2147483648")
		mustOverflow(t, magic+desc32+"2F0000000100000001", new(int32), "-2147483649")
	})
}

// ST-reject: table-driven rejects with paths.
func TestRejects(t *testing.T) {
	var ch chan int
	var fn func()
	var up unsafe.Pointer
	cases := []struct {
		name     string
		input    any
		sentinel error
		path     string
	}{
		{"chan", ch, gbon.ErrUnsupported, ""},
		{"field-chan", struct{ C chan int }{}, gbon.ErrUnsupported, ".C"},
		{"func", fn, gbon.ErrUnsupported, ""},
		{"unsafe", up, gbon.ErrUnsupported, ""},
		{"unexported", struct{ a int }{a: 1}, gbon.ErrUnsupported, ".a"},
		{"nan-key", map[float64]int{math.NaN(): 1}, gbon.ErrUnsupported, "["},
	}
	// Interface values and keys are encodable.
	if _, err := gbon.Marshal(boxAny{V: 5}); err != nil {
		t.Fatalf("iface value must encode: %v", err)
	}
	if _, err := gbon.Marshal(map[any]int{}); err != nil {
		t.Fatalf("iface keys must encode: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := gbon.Marshal(tc.input)
			if err == nil {
				t.Fatal("want reject, got nil")
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("sentinel: got %v", err)
			}
			var ae *gbon.Error
			if !errors.As(err, &ae) || ae.Class() != "unsupported_kind" {
				t.Fatalf("class: got %v, want unsupported_kind", err)
			}
			if !strings.Contains(ae.Path, tc.path) {
				t.Fatalf("path %q missing in %v", tc.path, err)
			}
		})
	}
	// positive rows: blank fields and nil interfaces are fine
	assertEquiv(t, blankField{X: 1})
	assertEquiv(t, boxAny{})
	// misuse: non-pointer Unmarshal target
	if err := gbon.Unmarshal([]byte{}, 5); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("misuse: got %v", err)
	}
}

// ST-maperr: every failure wraps exactly one sentinel; crafted
// duplicate keys (non-adjacent, non-canonical order) and the ±0 pair are
// ErrFormat; a recover guard proves no panic escapes.
func TestErrorMapping(t *testing.T) {
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic escaped: %s", safeDescValue(p))
			}
		}()
		check := func(name string, data []byte, v any) {
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("%s: panic: %s", name, safeDescValue(p))
					}
				}()
				return gbon.Unmarshal(data, v)
			}()
			if err == nil {
				return
			}
			count := 0
			for _, s := range []error{gbon.ErrUnsupported, gbon.ErrBudget, gbon.ErrFormat} {
				if errors.Is(err, s) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%s: wraps %d sentinels: %v", name, count, err)
			}
		}
		base := "67626F6E0000"
		dup, _ := hexDecode(base + "D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408" +
			"A3 616122 616224 616122")
		check("dup-key", dup, new(map[string]int))
		pm, _ := hexDecode(base + "D36C0E6D61705B666C6F617436345D696E74 DA67666C6F6174363408 D863696E7408" +
			"A2 41000000000000000022 41800000000000000024")
		check("pm-zero-pair", pm, new(map[float64]int))
		// truncations of a golden vector
		full := mustMarshal(t, map[string]int{"b": 2, "a": 1})
		for n := range full {
			check("truncate", full[:n], new(map[string]int))
		}
	}()
}

// ST-depth: value-axis depth cap on crafted nested ARRAY tokens.
func TestDepthCap(t *testing.T) {
	name := qn(selfRef{})
	var b bytes.Buffer
	b.Write(hdr)
	b.WriteByte(0xD1)
	b.WriteByte(0x6C)
	b.WriteByte(byte(len(name)))
	b.WriteString(name)
	b.WriteByte(0xC0) // self type-ref
	for range 10002 {
		b.WriteByte(0x81)
		b.WriteByte(0x01)
	}
	b.WriteByte(0x20)
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic: %s", safeDescValue(p))
			}
		}()
		var out selfRef
		return gbon.Unmarshal(b.Bytes(), &out)
	}()
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("want ErrBudget, got %v", err)
	}
	if err != nil {
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "budget_depth" {
			t.Fatalf("want class budget_depth, got %v", err)
		}
	}
}

// ST-depth-desc: DESC-parse axis — crafted D5 60 chain.
func TestDepthCapDesc(t *testing.T) {
	var b bytes.Buffer
	b.Write(hdr)
	for range 10002 {
		b.WriteByte(0xD5)
		b.WriteByte(0x60)
	}
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic: %s", safeDescValue(p))
			}
		}()
		var out *int
		return gbon.Unmarshal(b.Bytes(), &out)
	}()
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("want ErrBudget, got %v", err)
	}
}

// RT-map-share: two fields over one map decode as one map object —
// mutation through one field is visible through the other (identity by
// mutation observability, not by ==).
func TestRTMapShare(t *testing.T) {
	m := map[string]int{"a": 1}
	out := roundTrip(t, mapShare{X: m, Y: m}).(mapShare)
	out.X["b"] = 2
	if _, visible := out.Y["b"]; !visible {
		t.Fatal("mutation through X not visible in Y: map identity lost")
	}
	if len(out.X) != len(out.Y) || len(out.X) != 2 {
		t.Fatalf("slot len mismatch: X=%d Y=%d", len(out.X), len(out.Y))
	}
	out.Y["a"] = 9
	if out.X["a"] != 9 {
		t.Fatal("mutation through Y not visible in X: map identity lost")
	}
}

// mapShare is the two-map-field fixture over one map object.
type mapShare struct{ X, Y map[string]int }

// RT-map-self: a map stored under its own any-value key position — encode
// terminates (visited set), decode closes the cycle onto the same map.
func TestRTMapSelf(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	data, err := marshalGuarded(t, m)
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(data))
	if err := dec.Register(map[string]any{}, int64(0), ""); err != nil {
		t.Fatal(err)
	}
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	m2 := out.(map[string]any)
	self, ok := m2["self"].(map[string]any)
	if !ok {
		t.Fatalf("m[\"self\"] dynamic type = %T", m2["self"])
	}
	self["witness"] = int64(1)
	if _, visible := m2["witness"]; !visible {
		t.Fatal("m[\"self\"] does not alias m: self-cycle broken")
	}
}

// RT-map-mutual: two maps referencing each other through any values — both
// directions close.
func TestRTMapMutual(t *testing.T) {
	m1, m2 := map[string]any{}, map[string]any{}
	m1["a"], m2["b"] = m2, m1
	data, err := marshalGuarded(t, m1)
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(data))
	if err := dec.Register(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	o1 := out.(map[string]any)
	o2 := o1["a"].(map[string]any)
	if back, ok := o2["b"].(map[string]any); !ok || back["a"] == nil {
		t.Fatal("mutual cycle broken on the m2→m1 side")
	}
	o2["witness"] = int64(1)
	if _, visible := o1["a"].(map[string]any)["witness"]; !visible {
		t.Fatal("m2 does not alias through m1[\"a\"]: mutual identity broken")
	}
}

// RT-slice-self: a slice stored as its own element through any — encode
// terminates, decode yields an element window over the same backing.
func TestRTSliceSelf(t *testing.T) {
	s := make([]any, 2)
	s[0] = int64(1)
	s[1] = s
	data, err := marshalGuarded(t, s)
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(data))
	if err := dec.Register([]any{}, int64(0)); err != nil {
		t.Fatal(err)
	}
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	s2 := out.([]any)
	e, ok := s2[1].([]any)
	if !ok {
		t.Fatalf("s[1] dynamic type = %T", s2[1])
	}
	if len(e) != 2 || cap(e) != 2 {
		t.Fatalf("self-window geometry: len=%d cap=%d", len(e), cap(e))
	}
	s2[0] = int64(42)
	if e[0] != int64(42) {
		t.Fatal("s[1] is not a window over the same backing: slice self-cycle broken")
	}
}

// --- Cross-value sharing (per-stream backings) ---

// encodeStream runs a sequence of Encodes over one fresh encoder and
// returns the buffered stream bytes.
func encodeStream(t *testing.T, vals ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	for _, v := range vals {
		if err := e.Encode(v); err != nil {
			t.Fatalf("Encode(%s): %v", safeDescValue(v), err)
		}
	}
	return buf.Bytes()
}

// The same memory encoded twice — decoded values share one backing
// (identity proven through mutation), across stream values.
func TestCrossValueBackingShare(t *testing.T) {
	b := []byte{0xAA}
	stream := encodeStream(t, b, b)
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	if len(d1) != 1 || len(d2) != 1 || d1[0] != 0xAA {
		t.Fatalf("decoded: % x / % x", d1, d2)
	}
	d2[0] = 0x42
	if d1[0] != 0x42 {
		t.Fatal("value-2 does not share the backing of value-1")
	}
}

// Append within spare capacity writes the shared cross-value
// backing, exactly as in memory.
func TestCrossValueAppendThreshold(t *testing.T) {
	b := make([]byte, 2, 4)
	b[0], b[1] = 1, 2
	stream := encodeStream(t, b, b[:1])
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	if len(d1) != 2 || cap(d1) != 4 || len(d2) != 1 || cap(d2) != 4 {
		t.Fatalf("geometry: d1=%d/%d d2=%d/%d", len(d1), cap(d1), len(d2), cap(d2))
	}
	d2 = append(d2, 0xFF)
	if d1[1] != 0xFF {
		t.Fatal("append in spare cap did not write the shared backing")
	}
	d1[0] = 0x99
	if d2[0] != 0x99 {
		t.Fatal("overlapping windows do not share memory")
	}
}

// A sub-window of value-1's memory encodes as a view and decodes as
// a window over value-1's backing.
func TestCrossValueViewWindow(t *testing.T) {
	v := []byte{01, 02, 03, 00}
	stream := encodeStream(t, v, v[1:3])
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	if len(d2) != 2 || cap(d2) != 3 || d2[0] != 02 || d2[1] != 03 {
		t.Fatalf("window decode: len=%d cap=%d bytes=% x", len(d2), cap(d2), d2)
	}
	d2[0] = 0x77
	if d1[1] != 0x77 {
		t.Fatal("value-2 window is not over value-1's backing")
	}
}

// A mutated elided tail between values is unrepresentable over the
// closed record — the repeat falls back to a fresh record carrying the new
// bytes (the groupOf trap: never a view over the stale snapshot).
func TestCrossValueTailMutationFallback(t *testing.T) {
	v := []byte{01, 02, 00, 00}
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	v[2] = 0xAA
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	want := []byte{01, 02, 0xAA, 00}
	if !bytes.Equal(d2, want) {
		t.Fatalf("fallback record: got % x want % x (stale-record corruption)", d2, want)
	}
	if bytes.Equal(d1, d2) {
		t.Fatal("values must not alias: the repeat was unrepresentable")
	}
}

// A window wider than the closed record's region falls back to a
// fresh record even without any tail mutation.
func TestCrossValueWiderWindowFallback(t *testing.T) {
	b := make([]byte, 4)
	b[2], b[3] = 5, 6
	w := b[2:4]
	stream := encodeStream(t, w, b)
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 5, 6}
	if !bytes.Equal(d2, want) {
		t.Fatalf("wider window: got % x want % x", d2, want)
	}
	if &d1[0] == &d2[2] {
		t.Fatal("wider-window fallback must not alias the closed record")
	}
}

// A mutated dense prefix between values is invisible — the closed
// record is a first-encounter snapshot, mirroring ptr/map semantics.
func TestCrossValueSnapshotSemantics(t *testing.T) {
	v := []byte{1, 2, 3, 0}
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	v[0] = 9
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d2, []byte{1, 2, 3, 0}) {
		t.Fatalf("snapshot: got % x want 01 02 03 00 (live prefix leaked)", d2)
	}
	d2[0] = 0x11
	if d1[0] != 0x11 {
		t.Fatal("the representable repeat must still alias the backing")
	}
}

// Independent non-nil empty slices collapse into one zerobase group
// per stream; the identity is unobservable (cap=0), the bytes are pinned
// by g55.
func TestCrossValueZerobaseEmpty(t *testing.T) {
	stream := encodeStream(t, []byte{}, []byte{})
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	var d1, d2 []byte
	if err := dec.Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&d2); err != nil {
		t.Fatal(err)
	}
	if d1 == nil || d2 == nil || len(d1) != 0 || len(d2) != 0 {
		t.Fatalf("empty non-nil: %v %v", d1, d2)
	}
}

// A view onto a backing skipped under the evolution contract
// stays unavailable — the unification does not weaken skip semantics.
type SA struct {
	X []byte
	Y int8
}

type SB struct {
	Y int8
}

func TestSkippedBackingUnavailableCrossValue(t *testing.T) {
	a := SA{X: []byte{1, 2, 3}, Y: 7}
	stream := encodeStream(t, a)
	// equal-length name splice: the stream must present the target's name
	// (SA→SB, one occurrence, intern lengths intact)
	if bytes.Count(stream, []byte(".SA")) != 1 {
		t.Fatalf("splice anchor: % x", stream)
	}
	stream = bytes.Replace(stream, []byte(".SA"), []byte(".SB"), 1)
	// value-2: type REF id3 ([]byte desc) + view form0 over the blob
	// record id8 of a.X (id-scan: SA0 name1 "X"2 desc3 "[]byte"4 "Y"5
	// int8-desc6 name7 blob8)
	stream = append(stream, 0xC3, 0x90, 0x08)
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	var b SB
	if err := dec.Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.Y != 7 {
		t.Fatalf("skipped field pair: %v", b)
	}
	var v []byte
	err := dec.Decode(&v)
	var vb *gbon.Error
	if err == nil || !errors.As(err, &vb) || vb.Class() != "bad_ref" {
		t.Fatalf("view over skipped backing: err = %v, want class bad_ref", err)
	}
}

// Multi-value streams without representable sharing stay
// byte-stable against the per-value bodies (the guard fallback reproduces
// per-value bytes; type REFs are the pre-existing stream behavior).
func TestMultiValueNoSharingByteStable(t *testing.T) {
	stream := encodeStream(t, []byte{1, 2}, []byte{3, 4})
	// id-scan: DESC(id0) name(id1) BLOB L=2 E=2 "0102" (id2) view(id2);
	// value-2: type REF(id0) + fresh BLOB (id3) + view — the per-value
	// record bytes of a standalone Marshal, ids shifted by the stream.
	w, err := hex.DecodeString(strings.ReplaceAll(
		"67626F6E0000 DC0D665B5D62797465 720201029002 C0720203049003", " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stream, w) {
		t.Fatalf("no-sharing stream:\n got  % x\n want % x", stream, w)
	}
	// Primitives: value-2 body is the bare token, only the type position
	// turns into the stream's REF.
	stream = encodeStream(t, 5, 6)
	w2, _ := hex.DecodeString(strings.ReplaceAll("67626F6E0000 D863696E7408 2A C02C0C", " ", ""))
	if !bytes.Equal(stream, w2) {
		t.Fatalf("primitive stream:\n got  % x\n want % x", stream, w2)
	}
}

type atomRec struct {
	ID   int64
	When time.Time
	Name string
	Tags []string
	Nums []int64
}

func atomFixture() atomRec {
	return atomRec{
		ID:   7,
		When: time.Date(2026, 8, 29, 12, 0, 0, 0, time.FixedZone("TST", 3600)),
		Name: "rec",
		Tags: []string{"a", "b"},
		Nums: []int64{1, 2, 3},
	}
}

func atomSentinel() atomRec {
	return atomRec{ID: -1, Name: "sentinel", Tags: []string{"keep"}, Nums: []int64{42}}
}

// TestAtomicityRegression pins the strong exception guarantee: on any
// decode error — format, budget, or unsupported — the target is left
// exactly as before the call (deep-equal), for the stateless Unmarshal
// root, the streaming Decoder root, and mid-struct failure positions.
func TestAtomicityRegression(t *testing.T) {
	fixture := atomFixture()
	data, err := gbon.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ErrFormat truncated mid-struct, stateless", func(t *testing.T) {
		for cut := 1; cut <= 24 && cut < len(data); cut++ {
			target := atomSentinel()
			err := gbon.Unmarshal(data[:len(data)-cut], &target)
			if err == nil {
				t.Fatalf("cut=%d: truncated stream decoded", cut)
			}
			if !reflect.DeepEqual(target, atomSentinel()) {
				t.Fatalf("cut=%d: target mutated: %s", cut, safeDescValue(target))
			}
		}
	})

	t.Run("ErrFormat truncated mid-struct, streaming", func(t *testing.T) {
		for cut := 1; cut <= 24 && cut < len(data); cut++ {
			target := atomSentinel()
			d := gbon.NewDecoder(bytes.NewReader(data[:len(data)-cut]))
			err := d.Decode(&target)
			if err == nil {
				t.Fatalf("cut=%d: truncated stream decoded", cut)
			}
			if !reflect.DeepEqual(target, atomSentinel()) {
				t.Fatalf("cut=%d: target mutated: %s", cut, safeDescValue(target))
			}
		}
	})

	t.Run("ErrBudget mid-struct", func(t *testing.T) {
		for maxNodes := 2; maxNodes <= 6; maxNodes++ {
			target := atomSentinel()
			d := gbon.NewDecoder(bytes.NewReader(data))
			d.SetLimits(gbon.Limits{MaxNodes: maxNodes})
			err := d.Decode(&target)
			if !errors.Is(err, gbon.ErrBudget) {
				t.Fatalf("MaxNodes=%d: want ErrBudget, got %v", maxNodes, err)
			}
			if !reflect.DeepEqual(target, atomSentinel()) {
				t.Fatalf("MaxNodes=%d: target mutated: %s", maxNodes, safeDescValue(target))
			}
		}
	})

	t.Run("ErrUnsupported mid-struct", func(t *testing.T) {
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		if err := enc.RegisterCoder(atomStamp(0), atomStampCoder{}); err != nil {
			t.Fatal(err)
		}
		if err := enc.RegisterAs("gbon.atomC", evoCustomV2{}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(evoCustomV2{ID: 5, Stamp: 11, Name: "s"}); err != nil {
			t.Fatal(err)
		}
		type atomV1 struct {
			ID   int64
			Name string
		}
		d := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
		if err := d.RegisterAs("gbon.atomC", atomV1{}); err != nil {
			t.Fatal(err)
		}
		before := atomV1{ID: -9, Name: "sentinel"}
		target := before
		err := d.Decode(&target)
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("want ErrFormat, got %v", err)
		}
		var an *gbon.Error
		if !errors.As(err, &an) || an.Class() != "unknown_name" {
			t.Fatalf("unsupported skip: want class unknown_name, got %v", err)
		}
		if !reflect.DeepEqual(target, before) {
			t.Fatalf("target mutated: %s", safeDescValue(target))
		}
	})

	t.Run("success still overwrites the target", func(t *testing.T) {
		target := atomSentinel()
		if err := gbon.Unmarshal(data, &target); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(target, fixture) {
			t.Fatalf("success path: got %s, want %s", safeDescValue(target), safeDescValue(fixture))
		}
	})
}

// tk-elision-bigint-zero — the elision predicate over BIGINT
// projections: a trailing element whose only content is a nil-or-zero big
// integer is wire-zero (nil ≡ inline zero) and must elide. Before
// the fix the encoder emitted a stream its own decoder rejected ("slice
// dense prefix ends in a zero element").
func TestRTBigintZeroTailElision(t *testing.T) {
	type tx struct{ Ref *big.Int }
	type vv struct{ N big.Int }
	type mx struct {
		A *big.Int
		N int
	}
	rt := func(t *testing.T, in, want any) []byte {
		t.Helper()
		b, err := gbon.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out := reflect.New(reflect.TypeOf(in)).Interface()
		if err := gbon.Unmarshal(b, out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got := reflect.ValueOf(out).Elem().Interface()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip: got %s want %s", safeDescValue(got), safeDescValue(want))
		}
		b2, err := gbon.Marshal(got)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if !bytes.Equal(b, b2) {
			t.Fatalf("re-marshal drift: %x vs %x", b, b2)
		}
		return b
	}

	// SB-1: the rl8 repro shape — nil followed by zero; both wire-zero,
	// so the dense prefix collapses and the stream decodes. Pointer
	// positions materialize wire-zero as nil.
	t.Run("sb1-repro-nil-zero", func(t *testing.T) {
		rt(t, []tx{{Ref: nil}, {Ref: big.NewInt(0)}},
			[]tx{{Ref: nil}, {Ref: nil}})
	})

	// SB-2: a multi-element zero tail mixes nil and zero freely.
	t.Run("sb2-multi-tail-mixed", func(t *testing.T) {
		rt(t, []tx{{Ref: big.NewInt(7)}, {Ref: nil}, {Ref: big.NewInt(0)}, {Ref: nil}},
			[]tx{{Ref: big.NewInt(7)}, {Ref: nil}, {Ref: nil}, {Ref: nil}})
	})

	// SB-2: value-shape big.Int fields behave the same through struct
	// recursion; value positions materialize zero.
	t.Run("sb2-value-shape", func(t *testing.T) {
		rt(t, []vv{{N: *big.NewInt(7)}, {}, {N: *big.NewInt(0)}},
			[]vv{{N: *big.NewInt(7)}, {}, {N: *big.NewInt(0)}})
	})

	// SB-2: the zero tail sits at struct depth, not only at the root.
	t.Run("sb2-depth", func(t *testing.T) {
		type leaf struct{ W *big.Int }
		type mid struct {
			L []leaf
			S string
		}
		type root struct{ M []mid }
		rt(t, root{M: []mid{{L: []leaf{{W: big.NewInt(3)}, {W: nil}, {W: big.NewInt(0)}}, S: "x"}}},
			root{M: []mid{{L: []leaf{{W: big.NewInt(3)}, {W: nil}, {W: nil}}, S: "x"}}})
	})

	// SB-3: a mixed struct elides when every field is wire-zero.
	t.Run("sb3-mixed-all-zero-elides", func(t *testing.T) {
		rt(t, []mx{{A: big.NewInt(9), N: 2}, {A: nil, N: 0}, {A: big.NewInt(0), N: 0}},
			[]mx{{A: big.NewInt(9), N: 2}, {A: nil, N: 0}, {A: nil, N: 0}})
	})

	// SB-3: a non-bigint non-zero field keeps the element dense.
	t.Run("sb3-mixed-nonzero-tail-stays", func(t *testing.T) {
		rt(t, []mx{{A: big.NewInt(1), N: 0}, {A: nil, N: 1}},
			[]mx{{A: big.NewInt(1), N: 0}, {A: nil, N: 1}})
	})

	// SB-4: a non-zero integer stops elision (Sign() ≠ 0 is not wire-zero).
	t.Run("sb4-sign-nonzero-tail", func(t *testing.T) {
		rt(t, []tx{{Ref: big.NewInt(0)}, {Ref: big.NewInt(5)}},
			[]tx{{Ref: nil}, {Ref: big.NewInt(5)}})
	})

	// SB-4: negative zero stays materialized (bit-for-bit promise).
	t.Run("sb4-negative-zero-float-tail", func(t *testing.T) {
		in := []float64{1.5, math.Copysign(0, -1)}
		b, err := gbon.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out []float64
		if err := gbon.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out) != 2 || math.Float64bits(out[1]) != math.Float64bits(in[1]) {
			t.Fatalf("negative zero lost: %v", out)
		}
	})

	// SB-4: a pure nil tail was already correct — symmetry control.
	t.Run("sb4-nil-tail-preexisting", func(t *testing.T) {
		rt(t, []tx{{Ref: big.NewInt(9)}, {Ref: nil}},
			[]tx{{Ref: big.NewInt(9)}, {Ref: nil}})
	})
}

// TestSafeDescCyclicRT: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicRT(t *testing.T) {
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
