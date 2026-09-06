package gbon_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Interface resolution, typed nils, iface keys,
// budgets, sticky-positive. Bytes are never inspected here — behavior only
// (byte-level pins live in the golden corpus g34..g40).

func ifaceUnhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

func ifaceDecoder(t *testing.T, data []byte, types ...any) *gbon.Decoder {
	t.Helper()
	dec := gbon.NewDecoder(bytes.NewReader(data))
	if len(types) > 0 {
		if err := dec.Register(types...); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return dec
}

// Dynamic type and value preserved;
// interning across stream values (REF tag in the second); pointer identity
// through two iface positions.
func TestInterfaceResolution(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(S{V: X{N: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(S{V: X{N: 8}}); err != nil {
		t.Fatal(err)
	}
	dec := ifaceDecoder(t, buf.Bytes(), X{})
	var v1, v2 S
	if err := dec.Decode(&v1); err != nil {
		t.Fatal(err)
	}
	if x, ok := v1.V.(X); !ok || x.N != 7 {
		t.Fatalf("v1: got %#v", v1.V)
	}
	if err := dec.Decode(&v2); err != nil {
		t.Fatal(err)
	}
	if x, ok := v2.V.(X); !ok || x.N != 8 {
		t.Fatalf("v2 (REF tag path): got %#v", v2.V)
	}
	p := &X{N: 3}
	d2 := ifaceDecoder(t, mustMarshal(t, T{A: p, B: p}), &X{})
	var w T
	if err := d2.Decode(&w); err != nil {
		t.Fatal(err)
	}
	a, aok := w.A.(*X)
	b, bok := w.B.(*X)
	if !aok || !bok || a != b {
		t.Fatalf("unified identity lost: %v %v", w.A, w.B)
	}
}

// TestTypedNil: nil stays nil, typed nil stays
// a non-nil interface with nil dynamic value; distinct map slots.
func TestTypedNil(t *testing.T) {
	var nilOut S
	if err := gbon.Unmarshal(mustMarshal(t, S{}), &nilOut); err != nil {
		t.Fatal(err)
	}
	if nilOut.V != nil {
		t.Fatalf("nil iface must stay nil: %#v", nilOut.V)
	}
	d := ifaceDecoder(t, mustMarshal(t, S{V: (*Z)(nil)}), &Z{})
	var tn S
	if err := d.Decode(&tn); err != nil {
		t.Fatal(err)
	}
	if tn.V == nil {
		t.Fatal("typed nil collapsed to nil interface")
	}
	if p, ok := tn.V.(*Z); !ok || p != nil {
		t.Fatalf("want (*Z)(nil), got %#v", tn.V)
	}
	dm := ifaceDecoder(t, mustMarshal(t, map[any]int{nil: 1, (*Z)(nil): 2}), &Z{})
	var m map[any]int
	if err := dm.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("want 2 distinct key slots, got %d", len(m))
	}
	var z *Z
	if m[nil] != 1 || m[any(z)] != 2 {
		t.Fatalf("slot values wrong: %#v", m)
	}
}

// TestUnhashableValue: unhashable dynamic types
// register and round-trip in iface VALUE positions (g37 class).
func TestUnhashableValue(t *testing.T) {
	dec := ifaceDecoder(t, mustMarshal(t, any(map[string]int{"a": 1})), map[string]int{})
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]int)
	if !ok || m["a"] != 1 {
		t.Fatalf("got %#v", v)
	}
	sl := ifaceDecoder(t, mustMarshal(t, S{V: []int32{4, 5}}), []int32{})
	var s S
	if err := sl.Decode(&s); err != nil {
		t.Fatal(err)
	}
	got, ok := s.V.([]int32)
	if !ok || len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("slice dynamic: got %#v", s.V)
	}
}

// TestUnhashableKey: unhashable dynamic keys
// cannot exist in a Go-constructed map (insertion itself panics),
// so the reachable surface is crafted input: the decoder
// rejects with the pair path before SetMapIndex, never a Go hash panic.
func TestUnhashableKey(t *testing.T) {
	type KF struct{ F any }
	// deepStructFieldStream rebuilds the map[KF]int craft with the
	// qualified descriptor names the encoder emits.
	deepStructFieldStream := func(kf string) string {
		strLit := func(s string) string {
			return "6C" + hex.EncodeToString([]byte{byte(len(s))}) + hex.EncodeToString([]byte(s))
		}
		return "67626F6E0000" +
			"D3" + strLit("map["+kf+"]int") +
			"D0" + strLit(kf) + "016146" +
			"D66C0C696E74657266616365207B7D" + // interface{}
			"D863696E7408" + // int64
			"A1" + "B0" +
			"D1655B5D696E74" + // []int
			"D863696E7408" +
			"01" + "22"
	}
	cases := []struct {
		name   string
		stream string
		reg    []any
		path   string
	}{
		{"slice dynamic",
			"67626F6E0000 D36C14 6D61705B696E74657266616365207B7D5D696E74 D66C0C 696E74657266616365207B7D D863696E7408 A1 D1655B5D696E74 D863696E7408 01 22",
			[]any{[]int{}}, "[0]"},
		{"map dynamic",
			"67626F6E0000 D36C14 6D61705B696E74657266616365207B7D5D696E74 D66C0C 696E74657266616365207B7D D863696E7408 A1 D36C0E 6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 02 22",
			[]any{map[string]int{}}, "[0]"},
		{"deep struct field", deepStructFieldStream(qn(KF{})), []any{[]int{}}, "[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := ifaceDecoder(t, ifaceUnhex(t, tc.stream), tc.reg...)
			var err error
			if tc.name == "deep struct field" {
				m := map[KF]int{}
				err = dec.Decode(&m)
			} else {
				m := map[any]int{}
				err = dec.Decode(&m)
			}
			if !errors.Is(err, gbon.ErrFormat) {
				t.Fatalf("want ErrFormat, got %v", err)
			}
			var uh *gbon.Error
			if !errors.As(err, &uh) || uh.Class() != "malformed_op" {
				t.Fatalf("want class malformed_op, got %v", err)
			}
			if !strings.Contains(uh.Path, tc.path) {
				t.Fatalf("error must carry the position %q: %v", tc.path, err)
			}
		})
	}
}

// An unregistered name is a
// registry-contract ErrUnsupported (wire itself is valid), name in message.
// Basic types are auto-registered (NewDecoder/Unmarshal), so the
// unregistered probe is a structured type.
func TestUnregisteredName(t *testing.T) {
	dec := ifaceDecoder(t, mustMarshal(t, S{V: struct{ X int64 }{1}}))
	var v S
	err := dec.Decode(&v)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("want ErrFormat, got %v", err)
	}
	var un *gbon.Error
	if !errors.As(err, &un) || un.Class() != "unknown_name" || un.Got != "struct { X int64 }" {
		t.Fatalf("unregistered name must appear: %v", err)
	}
}

// Stateless Unmarshal rejects
// concrete iface values with the registry hint (basic types decode);
// nil ifaces decode.
func TestOneShotIface(t *testing.T) {
	var v S
	err := gbon.Unmarshal(mustMarshal(t, S{V: struct{ X int64 }{1}}), &v)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("want ErrFormat, got %v", err)
	}
	var hn *gbon.Error
	if !errors.As(err, &hn) || hn.Class() != "unknown_name" {
		t.Fatalf("hint must appear: %v", err)
	}
	var w S
	if err := gbon.Unmarshal(mustMarshal(t, S{}), &w); err != nil || w.V != nil {
		t.Fatalf("nil iface one-shot: %v %#v", err, w.V)
	}
}

// Distinct dynamic types
// with equal bodies are legal keys; a byte-patched duplicate collapses to
// ErrFormat (len-guard); a REF to a non-descriptor intern id is ErrFormat.
func TestIfaceKeyDup(t *testing.T) {
	base := mustMarshal(t, map[any]int{int64(1): 1, int32(1): 2})
	decode := func(b []byte) error {
		dec := ifaceDecoder(t, b, int64(0), int32(0))
		var m map[any]int
		return dec.Decode(&m)
	}
	if err := decode(base); err != nil {
		t.Fatalf("g38 class must be accepted: %v", err)
	}
	dup := bytes.Clone(base)
	i := bytes.Index(dup, []byte("int32"))
	if i < 0 {
		t.Fatal("int32 tag not found")
	}
	dup[i+3], dup[i+4] = '6', '4'
	if err := decode(dup); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("duplicate iface keys: want ErrFormat (len-guard), got %v", err)
	}
	redef := bytes.Clone(base)
	j := bytes.Index(redef, []byte("int32"))
	if j < 2 {
		t.Fatal("tag position invalid")
	}
	redef[j-2], redef[j-1] = 0xCC, 0xFF
	if err := decode(redef); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("intern-id redefinition: want ErrFormat, got %v", err)
	}
}

// Zero Limits applies the
// conservative defaults; an explicit large field lifts a default; negative
// fields fail before consumption without breaking the decoder; one-shot
// Unmarshal always uses the defaults.
func TestLimitsDefaults(t *testing.T) {
	deep := func(n int) []byte {
		var b bytes.Buffer
		b.Write(hdr)
		name := qn(selfRef{})
		b.WriteByte(0xD1)
		b.WriteByte(0x6C)
		b.WriteByte(byte(len(name)))
		b.WriteString(name)
		b.WriteByte(0xC0)
		for range n {
			b.WriteByte(0x81)
			b.WriteByte(0x01)
		}
		b.WriteByte(0x01) // innermost nil slice: the craft is fully valid
		return b.Bytes()
	}
	dec := gbon.NewDecoder(bytes.NewReader(deep(10002)))
	var out selfRef
	err := dec.Decode(&out)
	if !errors.Is(err, gbon.ErrBudget) || !strings.Contains(err.Error(), "MaxDepth") {
		t.Fatalf("default depth budget: %v", err)
	}
	var deepVal selfRef
	for range 9995 {
		deepVal = selfRef{deepVal}
	}
	dec2 := gbon.NewDecoder(bytes.NewReader(mustMarshal(t, deepVal)))
	var out2 selfRef
	if err := dec2.Decode(&out2); err != nil {
		t.Fatalf("within default depth budget: %v", err)
	}
	// lifted MaxDepth on the bottomless craft: the depth check does not
	// fire — the parse advances into the malformed tail instead (ErrFormat).
	dec2b := gbon.NewDecoder(bytes.NewReader(deep(10002)))
	dec2b.SetLimits(gbon.Limits{MaxDepth: 20000})
	var out2b selfRef
	err = dec2b.Decode(&out2b)
	var lf *gbon.Error
	if err == nil || !errors.As(err, &lf) || lf.Class() == "budget_depth" {
		t.Fatalf("lifted MaxDepth must not fire: %v", err)
	}
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("advanced past depth into the malformed tail: %v", err)
	}
	good := mustMarshal(t, 7)
	for _, f := range []gbon.Limits{
		{MaxDepth: -1}, {MaxNodes: -1}, {MaxBytes: -1},
		{MaxMapPairs: -1}, {MaxSliceLen: -1},
	} {
		dec3 := gbon.NewDecoder(bytes.NewReader(good))
		dec3.SetLimits(f)
		var n int
		if err := dec3.Decode(&n); !errors.Is(err, gbon.ErrUnsupported) {
			t.Fatalf("negative %#v: %v", f, err)
		}
		dec3.SetLimits(gbon.Limits{})
		var n2 int
		if err := dec3.Decode(&n2); err != nil || n2 != 7 {
			t.Fatalf("decoder must stay usable after negative limits: %v %d", err, n2)
		}
	}
	var sr selfRef
	if err := gbon.Unmarshal(deep(10002), &sr); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("one-shot defaults: %v", err)
	}
}

// Each default budget fires on a
// crafted overflow with its kind in the message.
func TestBudgetDefaults(t *testing.T) {
	big := make([]int64, 1000001)
	for i := range big {
		big[i] = 1
	}
	dec := gbon.NewDecoder(bytes.NewReader(mustMarshal(t, big)))
	var out []int64
	err := dec.Decode(&out)
	if !errors.Is(err, gbon.ErrBudget) || !strings.Contains(err.Error(), "MaxNodes") {
		t.Fatalf("MaxNodes: %v", err)
	}
	crafted := ifaceUnhex(t, "67626F6E0000"+
		"D36C0E6D61705B737472696E675D696E74"+
		"DC0C66737472696E67"+
		"D863696E7408"+
		"AE000F4241")
	var m map[string]int
	err = gbon.Unmarshal(crafted, &m)
	if !errors.Is(err, gbon.ErrBudget) || !strings.Contains(err.Error(), "MaxMapPairs") {
		t.Fatalf("MaxMapPairs: %v", err)
	}
	huge := strings.Repeat("a", 100000001)
	var so string
	err = gbon.Unmarshal(mustMarshal(t, huge), &so)
	if !errors.Is(err, gbon.ErrBudget) || !strings.Contains(err.Error(), "MaxBytes") {
		t.Fatalf("MaxBytes: %v", err)
	}
}

// Errors raised before stream
// consumption leave the decoder usable.
func TestStickyPositive(t *testing.T) {
	var sbuf bytes.Buffer
	senc := gbon.NewEncoder(&sbuf)
	if err := senc.Encode(7); err != nil {
		t.Fatal(err)
	}
	if err := senc.Encode(7); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(sbuf.Bytes()))
	if err := dec.Decode(nil); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("nil target: %v", err)
	}
	var n int
	if err := dec.Decode(&n); err != nil || n != 7 {
		t.Fatalf("decode after target error: %v %d", err, n)
	}
	dec.SetLimits(gbon.Limits{MaxDepth: -1})
	if err := dec.Decode(&n); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("negative limits: %v", err)
	}
	dec.SetLimits(gbon.Limits{})
	if err := dec.Decode(&n); err != nil || n != 7 {
		t.Fatalf("decode after limits error: %v %d", err, n)
	}
}

// A type registered after the first Decode is visible to
// the next Decode (between-Decode registration).
func TestRegisterAfterDecodeVisible(t *testing.T) {
	// One stream, two values: V=int64 first (registered before decoding),
	// V=Inner second (registered only after the first Decode).
	type Inner struct{ N int64 }
	var stream bytes.Buffer
	enc := gbon.NewEncoder(&stream)
	if err := enc.Encode(struct{ V any }{int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(struct{ V any }{Inner{7}}); err != nil {
		t.Fatal(err)
	}

	dec := gbon.NewDecoder(&stream)
	if err := dec.Register(int64(0)); err != nil {
		t.Fatal(err)
	}
	var first struct{ V any }
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("first decode (no registry needed): %v", err)
	}
	if v, ok := first.V.(int64); !ok || v != 1 {
		t.Fatalf("first value: want int64(1), got %#v", first.V)
	}
	if err := dec.Register(Inner{}); err != nil {
		t.Fatalf("register between decodes: %v", err)
	}
	var second struct{ V any }
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("decode with registry: %v", err)
	}
	if v, ok := second.V.(Inner); !ok || v.N != 7 {
		t.Fatalf("want Inner{7}, got %#v", second.V)
	}
}

// The encoder-side unhashable-key reject branch is reachable via
// pointer-wrapped unhashable content and must yield ErrUnsupported, never a
// Go hash panic.
func TestUnhashableInPointerKeyRejected(t *testing.T) {
	type KF struct{ F []int }
	type KP = *KF
	m := map[KP]int{}
	k := &KF{F: []int{1}}
	m[k] = 1
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		_, err = gbon.Marshal(m)
		return err
	}()
	var up *gbon.Error
	if err == nil || !errors.Is(err, gbon.ErrUnsupported) || !errors.As(err, &up) || up.Class() != "unsupported_kind" {
		t.Fatalf("want class unsupported_kind, got %v", err)
	}
	if !strings.Contains(up.Path, "[0].F") {
		t.Fatalf("error must name the offending field path, got %q", up.Path)
	}
}

// A nil example is rejected by Register.
func TestRegisterNilExample(t *testing.T) {
	dec := gbon.NewDecoder(nil)
	if err := dec.Register(nil); err == nil || !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for nil example, got %v", err)
	}
}

// Re-registering the same type is a no-op success.
func TestRegisterIdempotent(t *testing.T) {
	dec := gbon.NewDecoder(nil)
	if err := dec.Register(1, 1, 1); err != nil {
		t.Fatalf("re-register same type: %v", err)
	}
}

// TestStatelessCompositeAny pins the stateless Unmarshal registry: the
// basic composites ([]any, map[string]any, []string, []int64,
// map[string]string) resolve interface slots without Register.
func TestStatelessCompositeAny(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"[]any", []any{int64(1), "a", true}},
		{"map[string]any", map[string]any{"k": int64(2), "j": "v"}},
		{"[]string", []string{"x", "y"}},
		{"[]int64", []int64{7, 8, 9}},
		{"map[string]string", map[string]string{"a": "1", "b": "2"}},
		{"nested any slice in map", map[string]any{"m": []any{int64(3)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := gbon.Marshal(tc.val)
			if err != nil {
				t.Fatal(err)
			}
			var out any
			if err := gbon.Unmarshal(data, &out); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out, tc.val) {
				t.Fatalf("RT: got %#v, want %#v", out, tc.val)
			}
		})
	}
}
