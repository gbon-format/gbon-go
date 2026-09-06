package gbon_test

// Format-canonicality regression corpus: elision bit-predicate,
// view-form minimality, dense-prefix minimality, the BLOB/SLICE dual,
// skip-path map duplicate slots, and the KO-2a pointer-key positive
// controls.

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// --- elision predicate is bit-level ---

func rtSignbit[T float32 | float64](t *testing.T, name string, v T) {
	t.Helper()
	b, err := gbon.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var got T
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	if math.Signbit(float64(got)) != math.Signbit(float64(v)) {
		t.Fatalf("%s: sign lost, got %v want %v", name, got, v)
	}
}

func TestElisionBitZeroTail(t *testing.T) {
	neg := math.Copysign(0, -1) // literal −0.0 normalizes to +0.0 in Go
	if !math.Signbit(neg) {
		t.Fatal("setup: Copysign did not produce −0.0")
	}

	// Tail −0.0 in a slice materializes as a dense element.
	b, err := gbon.Marshal([]float64{neg})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte{0x41, 0x80, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("−0.0 element not materialized: % x", b)
	}
	var s []float64
	if err := gbon.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s) != 1 || !math.Signbit(s[0]) {
		t.Fatalf("decode gave %v, want [−0.0]", s)
	}

	// Arrays take the same predicate.
	a := [1]float64{neg}
	ab, err := gbon.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(ab, []byte{0x41, 0x80, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("−0.0 array element not materialized: % x", ab)
	}
	var ga [1]float64
	if err := gbon.Unmarshal(ab, &ga); err != nil {
		t.Fatal(err)
	}
	if !math.Signbit(ga[0]) {
		t.Fatalf("decode gave %v, want [−0.0]", ga)
	}

	// Mid + tail: only the bit-zero tail is elision fodder.
	rt := []float64{neg, 1.5, neg, 0.0}
	rb, err := gbon.Marshal(rt)
	if err != nil {
		t.Fatal(err)
	}
	var grt []float64
	if err := gbon.Unmarshal(rb, &grt); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, true, false} {
		if math.Signbit(grt[i]) != want {
			t.Fatalf("rt[%d]: sign %v, want %v (full: %v)", i, math.Signbit(grt[i]), want, grt)
		}
	}

	// Complex: imag=−0.0 keeps the element dense.
	cb, err := gbon.Marshal([]complex128{complex(0, neg)})
	if err != nil {
		t.Fatal(err)
	}
	var gc []complex128
	if err := gbon.Unmarshal(cb, &gc); err != nil {
		t.Fatal(err)
	}
	if len(gc) != 1 || !math.Signbit(imag(gc[0])) {
		t.Fatalf("complex tail: decode gave %v, want [0−0i]", gc)
	}

	// Transitive: struct whose only content is −0.0.
	type negZero struct{ F float64 }
	sb, err := gbon.Marshal([]negZero{{neg}})
	if err != nil {
		t.Fatal(err)
	}
	var gs []negZero
	if err := gbon.Unmarshal(sb, &gs); err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 || !math.Signbit(gs[0].F) {
		t.Fatalf("struct tail: decode gave %v", gs)
	}

	// Control: scalar −0.0 and float32 keep the sign.
	rtSignbit(t, "scalar-f64", neg)
	rtSignbit(t, "scalar-f32", float32(neg))
}

// --- view-form minimality ---

func TestViewFormMinimality(t *testing.T) {
	cases := []struct {
		name string
		gen  func(c *craft)
	}{
		{"form2-form0-geometry", func(c *craft) {
			c.descPos(dSlice(dInt64))
			r := c.arrayRec(1, 1)
			c.intTok(7)
			c.view2(r, 0, 1, 1)
		}},
		{"form2-cap-eq-len", func(c *craft) {
			c.descPos(dSlice(dInt64))
			r := c.arrayRec(2, 1)
			c.intTok(7)
			c.view2(r, 0, 1, 1)
		}},
		{"form1-form0-geometry", func(c *craft) {
			c.descPos(dSlice(dInt64))
			r := c.arrayRec(1, 1)
			c.intTok(7)
			c.view1(r, 0, 1)
		}},
	}
	for _, tc := range cases {
		c := newCraft()
		tc.gen(c)
		var s []int64
		err := gbon.Unmarshal(c.buf, &s)
		if err == nil {
			t.Fatalf("%s: decoded without error", tc.name)
		}
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("%s: err = %v, want ErrFormat", tc.name, err)
		}
	}

	// Positive control: form 2 with cap > len stays legal.
	c := newCraft()
	c.descPos(dSlice(dInt64))
	r := c.arrayRec(2, 1)
	c.intTok(7)
	c.view2(r, 0, 1, 2)
	var s []int64
	if err := gbon.Unmarshal(c.buf, &s); err != nil {
		t.Fatalf("minimal form2: %v", err)
	}
	if len(s) != 1 || s[0] != 7 || cap(s) != 2 {
		t.Fatalf("minimal form2: got %#v", s)
	}
}

// --- dense-prefix minimality ---

func TestDensePrefixMinimality(t *testing.T) {
	cases := []struct {
		name      string
		gen       func(c *craft)
		wantClass string
	}{
		{"slice-explicit-zero-tail", func(c *craft) {
			c.descPos(dSlice(dInt64))
			c.arrayRec(2, 2)
			c.intTok(7)
			c.intTok(0)
			// no view token needed: the E check fires first
		}, "malformed_op"},
		{"array-explicit-zero-tail", func(c *craft) {
			c.descPos(dArray(2, dInt64))
			c.arrayRec(2, 2)
			c.intTok(7)
			c.intTok(0)
		}, "malformed_op"},
		{"blob-zero-dense-tail", func(c *craft) {
			c.descPos(dBlob)
			c.blobRec(2, 2, []byte{7, 0})
		}, "malformed_op"},
	}
	for _, tc := range cases {
		c := newCraft()
		tc.gen(c)
		var target any = new([]int64)
		if tc.name == "array-explicit-zero-tail" {
			target = new([2]int64)
		} else if tc.name == "blob-zero-dense-tail" {
			target = new([]byte)
		}
		err := gbon.Unmarshal(c.buf, target)
		if err == nil {
			t.Fatalf("%s: decoded without error", tc.name)
		}
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("%s: err = %v, want ErrFormat", tc.name, err)
		}
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != tc.wantClass {
			t.Fatalf("%s: class = %v, want %q", tc.name, err, tc.wantClass)
		}
	}

	// Positive control: E exactly one past the last nonzero element.
	c := newCraft()
	c.descPos(dSlice(dInt64))
	r := c.arrayRec(2, 1)
	c.intTok(7)
	c.view0(r)
	var s []int64
	if err := gbon.Unmarshal(c.buf, &s); err != nil {
		t.Fatalf("minimal E: %v", err)
	}
	if len(s) != 2 || s[0] != 7 || s[1] != 0 {
		t.Fatalf("minimal E: got %#v", s)
	}
}

// --- BLOB/SLICE dual ---

type canonMyByte uint8

func TestBlobDualReject(t *testing.T) {
	// The canonical NAME ("[]byte") with the non-canonical KIND (SLICE of
	// UINT-8): a name-level splice would mask the dual under test.
	c := newCraft()
	c.descPos(&cDesc{kind: 1, name: "[]byte", refs: []*cDesc{dUint(1, "uint8")}})
	r := c.arrayRec(1, 1)
	c.uintTok(7)
	c.view0(r)
	var bs []byte
	err := gbon.Unmarshal(c.buf, &bs)
	if err == nil {
		t.Fatal("SLICE(uint8) decoded into []byte without error")
	}
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "type_mismatch" {
		t.Fatalf("err = %v, want the BLOB-dual type_mismatch class", err)
	}

	// The same class of stream serves a []named-byte target: the element
	// type is a defined type, so the target is not a byte-slice base and
	// SLICE is its canonical descriptor.
	cb := newCraft()
	cb.descPos(dSlice(dNamed(qn(canonMyByte(0)), dUint(1, "uint8"))))
	rb := cb.arrayRec(1, 1)
	cb.uintTok(7)
	cb.view0(rb)
	var mb []canonMyByte
	if err := gbon.Unmarshal(cb.buf, &mb); err != nil {
		t.Fatalf("named elem: %v", err)
	}
	if len(mb) != 1 || mb[0] != 7 {
		t.Fatalf("named elem: got %#v", mb)
	}
}

// --- skip-path duplicate map keys ---

type canonDropMap struct{ K int64 }

func TestSkipMapDuplicateReject(t *testing.T) {
	md := dMap(dInt64, dInt64)
	c := newCraft()
	c.descPos(dStructT(qn(canonDropMap{}), cField{"M", md}, cField{"K", dInt64}))
	c.structTok()
	c.mapRec(2)
	c.intTok(5) // key A
	c.intTok(1)
	c.intTok(5) // key B: byte-identical token sequence → duplicate slot
	c.intTok(2)
	c.intTok(9)
	var got canonDropMap
	err := gbon.Unmarshal(c.buf, &got)
	if err == nil {
		t.Fatal("duplicate key in skipped map decoded without error")
	}
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
	var de *gbon.Error
	if !errors.As(err, &de) || de.Class() != "duplicate_key" {
		t.Fatalf("err = %v, want the duplicate-slot duplicate_key class", err)
	}

	// Pointer keys are exempt on the skip path: distinct pointers may
	// share byte-identical encodings (KO-2a) — no false reject.
	pd := dMap(dPtr(dInt64), dInt64)
	c2 := newCraft()
	c2.descPos(dStructT(qn(canonDropMap{}), cField{"M", pd}, cField{"K", dInt64}))
	c2.structTok()
	c2.mapRec(2)
	c2.ptrRec()
	c2.intTok(7)
	c2.intTok(1)
	c2.ptrRec()
	c2.intTok(7)
	c2.intTok(2)
	c2.intTok(9)
	var ok canonDropMap
	if err := gbon.Unmarshal(c2.buf, &ok); err != nil {
		t.Fatalf("byte-equal pointer keys in skipped map: %v", err)
	}
	if ok.K != 9 {
		t.Fatalf("kept field lost: %+v", ok)
	}
}

// --- KO-2a positive control: byte-equal pointer keys stay legal ---

func TestPointerKeyByteEqualPositive(t *testing.T) {
	a, b := int64(5), int64(5)
	m := map[*int64]int64{&a: 1, &b: 2}
	buf, err := gbon.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[*int64]int64
	if err := gbon.Unmarshal(buf, &got); err != nil {
		t.Fatalf("byte-equal pointer keys: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Determinism control: re-encode of the decoded map is stable.
	buf2, err := gbon.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	buf3, err := gbon.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf2, buf3) {
		t.Fatal("repeated encode of one map is not deterministic")
	}
}

// --- companion: skipped blob keeps the E-minimality mirror ---

type canonDropBlob struct{ K int64 }

func TestSkipBlobDensePrefix(t *testing.T) {
	c := newCraft()
	c.descPos(dStructT(qn(canonDropBlob{}), cField{"B", dBlob}, cField{"K", dInt64}))
	c.structTok()
	br := c.blobRec(2, 2, []byte{7, 0})
	c.view0(br)
	c.intTok(9)
	var got canonDropBlob
	err := gbon.Unmarshal(c.buf, &got)
	if err == nil {
		t.Fatal("non-minimal E in skipped blob decoded without error")
	}
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
}
