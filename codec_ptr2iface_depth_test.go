package gbon_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Pointer chains of depth ≥2 ending at an interface position decode with
// the depth-1 pointer-to-interface semantics: one leading token carries
// the whole chain's state, disambiguated by the sort of the named intern
// record. Encoder bytes are the golden source — each vector pins a
// byte-exact re-encode of the decoded value, never hand-computed hex.

// ptrChainTypes returns registry examples covering the pointer-chain
// descriptor names (*interface {} through ****interface {}).
func ptrChainTypes() []any {
	return []any{new(any), new(*any), new(**any), new(***any)}
}

func TestPtrChainDepthMatrix(t *testing.T) {
	// ***any cycle: the leaf interface holds the chain's own outermost
	// pointer; the dynamic-type tag re-refers to the opening descriptor.
	var x any
	p1 := &x
	p2 := &p1
	p3 := &p2
	x = p3
	data := mustMarshal(t, x)
	dec := ifaceDecoder(t, data, ptrChainTypes()...)
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	q, ok := out.(***any)
	if !ok || q == nil {
		t.Fatalf("got %#v, want non-nil ***any", out)
	}
	if *q == nil || **q == nil {
		t.Fatal("chain must decode fully allocated")
	}
	back, ok := (***q).(***any)
	if !ok || back != q {
		t.Fatalf("cycle not preserved: %#v vs %v", ***q, q)
	}
	var buf bytes.Buffer
	if err := gbon.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("re-encode differs: %x vs %x", buf.Bytes(), data)
	}
}

// ***any over a fresh int64 leaf: the acyclic fresh-descriptor guard —
// the chain decodes through the leaf's own tag.
func TestPtrChainFreshDescAcyclic(t *testing.T) {
	var q any = int64(7)
	r1 := &q
	r2 := &r1
	r3 := &r2
	data := mustMarshal(t, r3)
	types := append(ptrChainTypes(), int64(0))
	dec := ifaceDecoder(t, data, types...)
	var out ***any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if v, ok := (***out).(int64); !ok || v != 7 {
		t.Fatalf("leaf: got %#v, want int64(7)", ***out)
	}
	var buf bytes.Buffer
	if err := gbon.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("re-encode differs: %x vs %x", buf.Bytes(), data)
	}
}

// ***any over a nil-interface leaf: selector 3 unwinds the whole chain,
// every level allocated and non-nil down to the nil interface.
func TestPtrChainNilLeaf(t *testing.T) {
	var q any
	r1 := &q
	r2 := &r1
	r3 := &r2
	data := mustMarshal(t, r3)
	dec := ifaceDecoder(t, data, ptrChainTypes()...)
	var out ***any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out == nil || *out == nil || **out == nil {
		t.Fatal("chain levels must be non-nil")
	}
	if (***out) != nil {
		t.Fatalf("leaf must stay a nil interface, got %#v", ***out)
	}
	var buf bytes.Buffer
	if err := gbon.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("re-encode differs: %x vs %x", buf.Bytes(), data)
	}
}

// Two ***any fields over one chain: the first field carries the fresh
// descriptor tag and closes its cycle; the second field's body opens
// with a REF to the already-registered chain target and shares it.
func TestPtrChainStructFieldsShared(t *testing.T) {
	type chainPair struct {
		A ***any
		B ***any
	}
	var x any
	p1 := &x
	p2 := &p1
	p3 := &p2
	x = p3
	v := chainPair{A: p3, B: p3}
	data := mustMarshal(t, v)
	dec := ifaceDecoder(t, data, ptrChainTypes()...)
	var out chainPair
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.A == nil || *out.A == nil || **out.A == nil {
		t.Fatal("field A chain must decode fully allocated")
	}
	back, ok := (***out.A).(***any)
	if !ok || back != out.A {
		t.Fatalf("field A cycle not preserved: %#v vs %v", ***out.A, out.A)
	}
	if out.B != out.A {
		t.Fatal("field B must share the registered chain target")
	}
	var buf bytes.Buffer
	if err := gbon.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("re-encode differs: %x vs %x", buf.Bytes(), data)
	}
}

// ****any cycle: the rule is depth-uniform, no per-depth cases.
func TestPtrChainDepthFourCycle(t *testing.T) {
	var x any
	p1 := &x
	p2 := &p1
	p3 := &p2
	p4 := &p3
	x = p4
	data := mustMarshal(t, x)
	dec := ifaceDecoder(t, data, ptrChainTypes()...)
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	q, ok := out.(****any)
	if !ok || q == nil {
		t.Fatalf("got %#v, want non-nil ****any", out)
	}
	back, ok := (****q).(****any)
	if !ok || back != q {
		t.Fatalf("cycle not preserved: %#v vs %v", ****q, q)
	}
	var buf bytes.Buffer
	if err := gbon.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("re-encode differs: %x vs %x", buf.Bytes(), data)
	}
}

// Nil-token kinds on a **any chain: nil outer, the outer-non-nil over
// nil-inner normalization (both emit one identical stream), the
// nil-interface leaf, and the typed nil under the leaf tag.
func TestPtrChainIfaceNilKinds(t *testing.T) {
	// nil **any
	var nilChain **any
	data := mustMarshal(t, nilChain)
	dec := ifaceDecoder(t, data)
	var out **any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("nil chain must stay nil, got %#v", out)
	}
	var buf bytes.Buffer
	if err := gbon.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("re-encode differs: %x vs %x", buf.Bytes(), data)
	}

	// non-nil outer over a nil inner pointer: identical stream to the
	// nil chain, decoded to the nil outer position, re-encodes to its
	// own bytes.
	var inner *any
	outer := &inner
	data2 := mustMarshal(t, outer)
	if !bytes.Equal(data2, data) {
		t.Fatalf("normalization streams must be identical: %x vs %x", data2, data)
	}
	dec2 := ifaceDecoder(t, data2)
	var out2 **any
	if err := dec2.Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if out2 != nil {
		t.Fatalf("normalized chain must decode to nil, got %#v", out2)
	}
	var buf2 bytes.Buffer
	if err := gbon.NewEncoder(&buf2).Encode(out2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf2.Bytes(), data2) {
		t.Fatalf("re-encode differs: %x vs %x", buf2.Bytes(), data2)
	}

	// non-nil chain over a nil-interface leaf
	var q any
	i1 := &q
	i2 := &i1
	data3 := mustMarshal(t, i2)
	dec3 := ifaceDecoder(t, data3)
	var out3 **any
	if err := dec3.Decode(&out3); err != nil {
		t.Fatal(err)
	}
	if out3 == nil || *out3 == nil {
		t.Fatal("chain levels must be non-nil")
	}
	if (**out3) != nil {
		t.Fatalf("leaf must stay a nil interface, got %#v", **out3)
	}
	var buf3 bytes.Buffer
	if err := gbon.NewEncoder(&buf3).Encode(out3); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf3.Bytes(), data3) {
		t.Fatalf("re-encode differs: %x vs %x", buf3.Bytes(), data3)
	}

	// typed nil (*any)(nil) under the leaf tag
	var q4 any = (*any)(nil)
	j1 := &q4
	j2 := &j1
	data4 := mustMarshal(t, j2)
	dec4 := ifaceDecoder(t, data4, ptrChainTypes()...)
	var out4 **any
	if err := dec4.Decode(&out4); err != nil {
		t.Fatal(err)
	}
	if out4 == nil || *out4 == nil {
		t.Fatal("chain levels must be non-nil")
	}
	leaf, ok := (**out4).(*any)
	if !ok || leaf != nil {
		t.Fatalf("want typed nil (*any)(nil), got %#v", **out4)
	}
	var buf4 bytes.Buffer
	if err := gbon.NewEncoder(&buf4).Encode(out4); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf4.Bytes(), data4) {
		t.Fatalf("re-encode differs: %x vs %x", buf4.Bytes(), data4)
	}
}

// Crafted chain heads keep their error classes: unregistered id,
// non-descriptor and cross-type object REFs are bad_ref; wrong nil
// selectors and depth exhaustion are malformed_op; unknown_name stays.
func TestPtrChainIfaceNegatives(t *testing.T) {
	// REF to an unregistered id at a chain head
	c := newCraft()
	c.descPos(dPtr(dPtr(dIface)))
	c.refTok(craftRec{id: 99})
	dec := gbon.NewDecoder(bytes.NewReader(c.buf))
	var out **any
	err := dec.Decode(&out)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("(a): want ErrFormat, got %v", err)
	}
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "bad_ref" {
		t.Fatalf("(a): want class bad_ref, got %v", err)
	}

	// nil selectors 1 and 2 at a chain head
	for _, sel := range []int{1, 2} {
		c2 := newCraft()
		c2.descPos(dPtr(dPtr(dIface)))
		c2.nilTok(sel)
		dec2 := gbon.NewDecoder(bytes.NewReader(c2.buf))
		var out2 **any
		err := dec2.Decode(&out2)
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("(b sel %d): want ErrFormat, got %v", sel, err)
		}
		var be *gbon.Error
		if !errors.As(err, &be) || be.Class() != "malformed_op" {
			t.Fatalf("(b sel %d): want class malformed_op, got %v", sel, err)
		}
	}

	// chain cycle via plain Unmarshal — derivable pointer chain closes
	var x any
	p1 := &x
	x = p1
	orig := mustMarshal(t, x)
	var out3 any
	if err := gbon.Unmarshal(orig, &out3); err != nil {
		t.Fatalf("(c): %v", err)
	}
	got, ok := out3.(*any)
	if !ok {
		t.Fatalf("(c): want dynamic type *any, got %T", out3)
	}
	if *got != out3 {
		t.Fatalf("(c): cycle must close on the decoded slot")
	}
	if !bytes.Equal(mustMarshal(t, out3), orig) {
		t.Fatalf("(c): re-encode must be byte-identical")
	}

	// REF to an object of a different pointer type
	c4 := newCraft()
	c4.descPos(dPtr(dInt64))
	rec := c4.ptrRec()
	c4.intTok(7)
	c4.descPos(dPtr(dPtr(dIface)))
	c4.refTok(rec)
	dec4 := gbon.NewDecoder(bytes.NewReader(c4.buf))
	var i *int64
	if err := dec4.Decode(&i); err != nil {
		t.Fatalf("(d) value 1: %v", err)
	}
	var out4 **any
	err = dec4.Decode(&out4)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("(d): want ErrFormat, got %v", err)
	}
	var de *gbon.Error
	if !errors.As(err, &de) || de.Class() != "bad_ref" {
		t.Fatalf("(d): want class bad_ref, got %v", err)
	}

	// REF to a non-descriptor record sort
	c5 := newCraft()
	c5.descPos(dString)
	c5.strPos("x")
	c5.descPos(dPtr(dPtr(dIface)))
	c5.refTok(craftRec{id: 2})
	dec5 := gbon.NewDecoder(bytes.NewReader(c5.buf))
	var s string
	if err := dec5.Decode(&s); err != nil {
		t.Fatalf("(e) value 1: %v", err)
	}
	var out5 **any
	err = dec5.Decode(&out5)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("(e): want ErrFormat, got %v", err)
	}
	var ee *gbon.Error
	if !errors.As(err, &ee) || ee.Class() != "bad_ref" {
		t.Fatalf("(e): want class bad_ref, got %v", err)
	}
	if !strings.Contains(err.Error(), "is not an object record") {
		t.Fatalf("(e): want the object-record text, got %v", err)
	}

	// self-referential tag chain: depth budget fires, never a hang
	c6 := newCraft()
	c6.descPos(dPtr(dPtr(dIface)))
	d6 := craftRec{id: 0}
	for range 10002 {
		c6.refTok(d6)
	}
	dec6 := gbon.NewDecoder(bytes.NewReader(c6.buf))
	if err := dec6.Register(ptrChainTypes()...); err != nil {
		t.Fatal(err)
	}
	var out6 **any
	err = dec6.Decode(&out6)
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("(f): want ErrBudget, got %v", err)
	}
	var fe *gbon.Error
	if !errors.As(err, &fe) || fe.Class() != "budget_depth" {
		t.Fatalf("(f): want class budget_depth, got %v", err)
	}
}
