package gbon_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// deriveIfacePtrChainForTest aliases the codec family-grammar hook for
// the corpus reader in specvectors_test.go.
func deriveIfacePtrChainForTest(name string) (reflect.Type, bool) {
	return gbon.DeriveIfacePtrChainForTest(name)
}

// buildShapeChain returns a pointer chain of the given depth over one any
// slot holding int64(7): depth 1 is *any, depth 4 is ****any.
func buildShapeChain(depth int) any {
	slot := reflect.New(reflect.TypeFor[any]()).Elem()
	slot.Set(reflect.ValueOf(int64(7)))
	cur := slot.Addr()
	for range depth - 1 {
		cell := reflect.New(cur.Type()).Elem()
		cell.Set(cur)
		cur = cell.Addr()
	}
	return cur.Interface()
}

// Plain Unmarshal derives unnamed pointer chains to an interface point on
// a registry miss; the decoded chain keeps the exact Go type, the any
// slot content, and re-encodes byte-identically.
func TestShapeDeriveChainRoundTrip(t *testing.T) {
	for depth := 1; depth <= 4; depth++ {
		root := buildShapeChain(depth)
		b := mustMarshal(t, root)
		out := reflect.New(reflect.TypeOf(root))
		if err := gbon.Unmarshal(b, out.Interface()); err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		got := out.Elem()
		if got.Type() != reflect.TypeOf(root) || got.IsNil() {
			t.Fatalf("depth %d: decoded type %v (nil=%v)", depth, got.Type(), got.IsNil())
		}
		tip := got
		for tip.Kind() == reflect.Pointer {
			if tip.IsNil() {
				t.Fatalf("depth %d: nil link", depth)
			}
			tip = tip.Elem()
		}
		if tip.Interface() != any(int64(7)) {
			t.Fatalf("depth %d: slot content %v", depth, tip.Interface())
		}
		if !bytes.Equal(mustMarshal(t, got.Interface()), b) {
			t.Fatalf("depth %d: re-encode drift", depth)
		}
	}
}

// One composite level over a chain derives too: []*interface {} and
// map[string]*interface {} round-trip through plain Unmarshal.
func TestShapeDeriveCompositeOverChain(t *testing.T) {
	v := any(int64(7))
	p := &v
	b := mustMarshal(t, []*any{p})
	var osl []*any
	if err := gbon.Unmarshal(b, &osl); err != nil {
		t.Fatalf("slice: %v", err)
	}
	if len(osl) != 1 || osl[0] == nil || *osl[0] != v {
		t.Fatalf("slice: %#v", osl)
	}
	if !bytes.Equal(mustMarshal(t, osl), b) {
		t.Fatalf("slice: re-encode drift")
	}
	b = mustMarshal(t, map[string]*any{"k": p})
	var om map[string]*any
	if err := gbon.Unmarshal(b, &om); err != nil {
		t.Fatalf("map: %v", err)
	}
	if q := om["k"]; q == nil || *q != v {
		t.Fatalf("map: %#v", om)
	}
	if !bytes.Equal(mustMarshal(t, om), b) {
		t.Fatalf("map: re-encode drift")
	}
}

// Family boundary (behavioral): names outside the derivable family keep
// the registry-contract miss (unknown_name), a family name over a
// mismatching structure reports type_mismatch.
func TestShapeDeriveFamilyBoundary(t *testing.T) {
	cases := []struct {
		label string
		desc  *cDesc
		body  func(c *craft)
		class string
	}{
		{"array over chain", dArray(3, dPtr(dIface)), nil, "unknown_name"},
		{"iface-key map over chain", dMap(dIface, dPtr(dIface)), nil, "unknown_name"},
		{"pointer over composite", &cDesc{kind: 5, name: "**[]interface {}", refs: []*cDesc{dSlice(dIface)}}, nil, "unknown_name"},
		{"named type", dNamed("vec.pair", dInt64), nil, "unknown_name"},
		{"named pointer", dNamed("*vec.pair", dPtr(dInt64)), nil, "unknown_name"},
		{"chain over wrong element", &cDesc{kind: 5, name: "**interface {}", refs: []*cDesc{dInt64}}, func(c *craft) { c.intTok(7) }, "type_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			c := newCraft()
			c.descPos(tc.desc)
			if tc.body != nil {
				tc.body(c)
			}
			var out any
			err := gbon.NewDecoder(bytes.NewReader(c.buf)).Decode(&out)
			if !errors.Is(err, gbon.ErrFormat) {
				t.Fatalf("want ErrFormat, got %v", err)
			}
			var ge *gbon.Error
			if !errors.As(err, &ge) || ge.Class() != tc.class {
				t.Fatalf("want class %s, got %v", tc.class, err)
			}
		})
	}
}

// Register stays stronger than derive: a bound chain name resolves
// through the registry hit, with the strict re-registration contract.
func TestShapeDeriveRegisterPrecedence(t *testing.T) {
	v := any(int64(7))
	p1 := &v
	p2 := &p1
	dec := gbon.NewDecoder(bytes.NewReader(mustMarshal(t, p2)))
	if err := dec.Register(new(*any)); err != nil {
		t.Fatalf("register: %v", err)
	}
	var out **any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out == nil || *out == nil || **out != v {
		t.Fatalf("decode: %#v", out)
	}
	if err := dec.Register(new(*any)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	dec1 := gbon.NewDecoder(bytes.NewReader(mustMarshal(t, p1)))
	if err := dec1.Register(new(any)); err != nil {
		t.Fatalf("register any: %v", err)
	}
	var o1 *any
	if err := dec1.Decode(&o1); err != nil {
		t.Fatalf("decode any: %v", err)
	}
	if o1 == nil || *o1 != v {
		t.Fatalf("decode any: %#v", o1)
	}
}

// The named-miss text stays verbatim for names outside the family.
func TestShapeDeriveNamedMissText(t *testing.T) {
	c := newCraft()
	c.descPos(dArray(3, dPtr(dIface)))
	var out any
	err := gbon.NewDecoder(bytes.NewReader(c.buf)).Decode(&out)
	if err == nil || !strings.Contains(err.Error(), "interface concrete type not registered: use Decoder.Register") {
		t.Fatalf("verbatim hint expected, got %v", err)
	}
}

// A nil pointer-chain root decodes additively as the typed nil of the
// chain in the any slot, with byte-identical re-encoding.
func TestShapeDeriveNilRoot(t *testing.T) {
	var ap *any
	b := mustMarshal(t, ap)
	var out any = 7
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatalf("nil *any root: %v", err)
	}
	if out == nil {
		t.Fatalf("nil *any root: want typed nil, got nil interface")
	}
	got, ok := out.(*any)
	if !ok || got != nil {
		t.Fatalf("nil *any root: want (*any)(nil), got %#v", out)
	}
	if !bytes.Equal(mustMarshal(t, out), b) {
		t.Fatalf("nil *any root: re-encode drift")
	}
}
