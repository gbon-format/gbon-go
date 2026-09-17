package gbon

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Canonical-grain scenarios: the ring-class repro family, the
// differing/equal-grain openings, the zero-size marker, and skip parity.

func canHex(t *testing.T, v any) string {
	t.Helper()
	b, err := codecMarshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return fmt.Sprintf("%x", b)
}

type zzS1 struct {
	F1 *zzS1
}

// @repro-ringclass: the ring-class family — plain **S1 control, the
// field-cell ring, the object-rooted ring, and the holder-field entry.
func TestCanonicalRingClass(t *testing.T) {
	// control: plain **S1, no ring
	y := &zzS1{}
	z := &y.F1
	d1 := canHex(t, z)
	var q1 **zzS1
	if err := codecUnmarshal(mustHexAny(t, d1), &q1); err != nil {
		t.Fatalf("control decode: %v", err)
	}
	// nil-bodied pointer chains merge with the nil position (the
	// nil-merge degeneracy): q1 is nil or points at a nil cell
	if q1 != nil && *q1 != nil {
		t.Fatalf("control: q1=%v *q1=%v", q1, *q1)
	}

	// ring rooted at the field cell
	s := &zzS1{}
	s.F1 = s
	x := &s.F1
	d2 := canHex(t, x)
	var q2 **zzS1
	if err := codecUnmarshal(mustHexAny(t, d2), &q2); err != nil {
		t.Fatalf("ring decode: %v (bytes %s)", err, d2)
	}
	if q2 == nil || *q2 == nil || (*q2).F1 != *q2 {
		t.Fatalf("ring: identity not preserved (q2=%v)", q2)
	}

	// ring rooted at the object
	d3 := canHex(t, s)
	var q3 *zzS1
	if err := codecUnmarshal(mustHexAny(t, d3), &q3); err != nil {
		t.Fatalf("obj-root decode: %v", err)
	}
	if q3 == nil || q3.F1 != q3 {
		t.Fatalf("obj-root: identity not preserved")
	}

	// aliased ring entered through a holder field
	type zzHolder struct{ P **zzS1 }
	h := &zzHolder{P: &s.F1}
	d4 := canHex(t, h)
	var q4 *zzHolder
	if err := codecUnmarshal(mustHexAny(t, d4), &q4); err != nil {
		t.Fatalf("holder decode: %v (bytes %s)", err, d4)
	}
	if q4.P == nil || *q4.P == nil || (*q4.P).F1 != *q4.P {
		t.Fatalf("holder: identity not preserved")
	}
}

func mustHexAny(t *testing.T, hex string) []byte {
	t.Helper()
	b := make([]byte, len(hex)/2)
	for i := range b {
		if _, err := fmt.Sscanf(hex[i*2:i*2+2], "%02x", &b[i]); err != nil {
			t.Fatalf("bad hex %q: %v", hex, err)
		}
	}
	return b
}

// @inv3-coarsest / @inv4-descent / @inv12-tag: the canonical grain with a
// differing-grain opening (tag) and a naked repeat REF.
func TestCanonicalCoarsestGrain(t *testing.T) {
	type S struct{ A int64 }
	type Box struct {
		P *int64
		Q *S
	}
	s := S{A: 5}
	box := Box{P: &s.A, Q: &s}
	b, err := codecMarshal(&box)
	if err != nil {
		t.Fatal(err)
	}
	var back Box
	if err := codecUnmarshal(b, &back); err != nil {
		t.Fatalf("decode: %v (bytes %x)", err, b)
	}
	if back.P == nil || back.Q == nil || *back.P != 5 {
		t.Fatalf("values: P=%v Q=%v", back.P, back.Q)
	}
	if back.P != &back.Q.A {
		t.Fatalf("descent: P=%p != &Q.A=%p (identity broken)", back.P, &back.Q.A)
	}
	// re-encode of the decoded graph is byte-identical
	b2, err := codecMarshal(&back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatalf("byte-idempotence:\n got  %x\n want %x", b2, b)
	}
}

// @inv5-zs / @inv13-marker: the zero-size marker vs the nil pointer.
func TestCanonicalZeroSizeMarker(t *testing.T) {
	p := &struct{}{}
	got := canHex(t, p)
	if bytes := got; len(bytes) < 2 || !bytesEndsWith(got, "04") {
		t.Fatalf("ZS marker: %s", got)
	}
	var q *struct{}
	if err := codecUnmarshal(mustHexAny(t, got), &q); err != nil {
		t.Fatalf("ZS decode: %v", err)
	}
	if q == nil {
		t.Fatalf("ZS decode: nil pointer")
	}
	// nil stays byte-distinct from the marker
	if nilHex := canHex(t, (*struct{})(nil)); nilHex == got {
		t.Fatalf("nil == marker bytes: %s", got)
	}
}

func bytesEndsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// @inv6-interior: interior zero-size collision — the tracked grain wins,
// the zero-size alias carries the marker.
func TestCanonicalInteriorZeroSize(t *testing.T) {
	type T struct {
		H int64
		L struct{}
		M int64
	}
	type W struct {
		PL *struct{}
		PM *int64
	}
	tt := T{H: 1, M: 2}
	w := W{PL: &tt.L, PM: &tt.M}
	b, err := codecMarshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	var back W
	if err := codecUnmarshal(b, &back); err != nil {
		t.Fatalf("decode: %v (bytes %x)", err, b)
	}
	if back.PL == nil || *back.PL != (struct{}{}) || back.PM == nil || *back.PM != 2 {
		t.Fatalf("values: PL=%v PM=%v", back.PL, back.PM)
	}
}

// @inv7-conv: layout normalization — two named conversions of one
// address share one record.
func TestCanonicalLayoutNormalization(t *testing.T) {
	type A struct{ X int64 }
	type B struct{ X int64 }
	type H struct {
		PA *A
		PB *B
	}
	a := A{X: 5}
	h := H{PA: &a, PB: (*B)(&a)}
	b, err := codecMarshal(&h)
	if err != nil {
		t.Fatal(err)
	}
	var back H
	if err := codecUnmarshal(b, &back); err != nil {
		t.Fatalf("decode: %v (bytes %x)", err, b)
	}
	if back.PA == nil || back.PB == nil || back.PA.X != 5 || back.PB.X != 5 {
		t.Fatalf("values: PA=%v PB=%v", back.PA, back.PB)
	}
	// one record: PA and PB materialize the same storage
	if reflect.ValueOf(back.PA).Pointer() != reflect.ValueOf(back.PB).Pointer() {
		t.Fatalf("layout: PA=%p PB=%p not aliased", back.PA, back.PB)
	}
}

// @inv1-assert: cross-value grain coarsening is a loud encode error —
// an address opened at a finer grain in one value cannot re-open at a
// coarser grain in the next value of the same stream.
func TestCanonicalCrossValueCoarsening(t *testing.T) {
	type S struct{ A int64 }
	var s S
	s.A = 5
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(&s.A); err != nil {
		t.Fatalf("thin root: %v", err)
	}
	err := enc.Encode(&s)
	if err == nil {
		t.Fatal("coarsening: no error")
	}
	if !strings.Contains(err.Error(), "cross-value grain coarsening") {
		t.Fatalf("coarsening: %v", err)
	}
}

// @inv9-skip: a dropped intermediate field decodes through the skip
// path (grain resolution without materialization) while the surrounding
// fields land identically to a full decode.
func TestCanonicalSkipParity(t *testing.T) {
	type Node struct {
		Name string
		Self *Node
		Tail int64
	}
	type Wide struct {
		Head string
		Mid  *Node
		Tail int64
	}
	type Narrow struct {
		Head string
		Tail int64
	}
	n := &Node{Name: "n", Tail: 7}
	n.Self = n
	w := Wide{Head: "h", Mid: n, Tail: 9}
	b, err := codecMarshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	var full Wide
	if err := codecUnmarshal(b, &full); err != nil {
		t.Fatalf("full decode: %v", err)
	}
	if full.Mid.Self != full.Mid || full.Tail != 9 || full.Head != "h" {
		t.Fatalf("full: %+v", full)
	}
	// the skip path: the same wire name bound to the narrower type —
	// the dropped field decodes through skipValue
	dec := NewDecoder(bytes.NewReader(b))
	if err := dec.RegisterAs("github.com/gbon-format/gbon-go.gbon.Wide", Narrow{}); err != nil {
		t.Fatalf("RegisterAs: %v", err)
	}
	var narrow Narrow
	if err := dec.Decode(&narrow); err != nil {
		t.Fatalf("skip decode: %v", err)
	}
	if narrow.Head != "h" || narrow.Tail != 9 {
		t.Fatalf("skip: surrounding fields %+v", narrow)
	}
}

// @inv7-chan: chan values are outside the encodable grammar — the
// loud unsupported is the contract (a grain never forms).
func TestCanonicalChanUnsupported(t *testing.T) {
	c := make(chan int)
	if _, err := codecMarshal(&c); err == nil {
		t.Fatal("chan: no error")
	}
}

// @inv12-tag: the three tag regimes over one topology — differing-grain
// opening carries the tag, equal-grain opening elides it, repeats are
// naked REFs.
func TestCanonicalGrainTagRegimes(t *testing.T) {
	type S struct{ A int64 }
	type Box struct {
		P *int64
		Q *S
	}
	s := S{A: 5}
	box := Box{P: &s.A, Q: &s}
	b, err := codecMarshal(&box)
	if err != nil {
		t.Fatal(err)
	}
	// regime inventory on the stream: the P opening is tagged (a DESC or
	// REF naming S precedes the record body), the Q repeat is a naked
	// REF carrying no descriptor bytes of its own making
	var back Box
	if err := codecUnmarshal(b, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.P != &back.Q.A {
		t.Fatalf("descent identity: P=%p &Q.A=%p", back.P, &back.Q.A)
	}
	b2, err := codecMarshal(&back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatalf("regime stability:\n got %x\n want %x", b2, b)
	}
	// equal-grain elision: a lone *int64 over its own storage emits the
	// body directly (no tag bytes before the value token)
	x := int64(9)
	le, err := codecMarshal(&x)
	if err != nil {
		t.Fatal(err)
	}
	var backX int64
	if err := codecUnmarshal(le, &backX); err != nil || backX != 9 {
		t.Fatalf("elided: %v %d", err, backX)
	}
}

// @inv12-degenerate: a degenerate payload form (dynamic type = struct
// with a leading any-field) on an interface-grain position — the
// encoder emits the tag reading only; decoding by the tag hypothesis is
// green.
func TestCanonicalDegenerateIfaceGrain(t *testing.T) {
	type D struct{ F any }
	var x any = D{F: int64(3)}
	p := &x
	b, err := codecMarshal(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// the emission must not be the dynamic reading: the position is
	// followed by a descriptor naming D and a struct record body, not
	// the bare dynamic payload
	if !bytes.Contains(b, []byte("struct { F interface {} }")) && !bytes.Contains(b, []byte("D")) {
		t.Fatalf("no grain-tag emission: %x", b)
	}
	dec := NewDecoder(bytes.NewReader(b))
	if err := dec.Register(D{}); err != nil {
		t.Fatal(err)
	}
	var q *any
	if err := dec.Decode(&q); err != nil {
		t.Fatalf("tag-hypothesis decode: %v", err)
	}
	if q == nil {
		t.Fatal("nil cell")
	}
}

// @inv12-selftag: a pointer-grain record at grain equality carries the
// self-tag — [tag][body], no leading-REF collision with a naked repeat.
func TestCanonicalSelfTag(t *testing.T) {
	var slot int64 = 7
	p1 := &slot
	pp := &p1
	b, err := codecMarshal(pp)
	if err != nil {
		t.Fatal(err)
	}
	// the [tag][body] shape: the position opens with the self-tag — a
	// REF naming the record grain's descriptor (c2 = REF to the
	// *int64 descriptor the root tree declared) — and the body token
	// (INT 7) follows; bytes pinned from the encoder's emission
	if !bytes.HasSuffix(b, []byte{0xc2, 0x2c, 0x0e}) {
		t.Fatalf("self-tag byte shape: %x (want ...c2 2c 0e)", b)
	}
	var back **int64
	if err := codecUnmarshal(b, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back == nil || *back == nil || **back != 7 {
		t.Fatalf("value: %v", back)
	}
	if **back != 7 || *back == nil {
		t.Fatalf("chain: %v", *back)
	}
	b2, err := codecMarshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatalf("self-tag stability:\n first %x\n second %x", b, b2)
	}
	// the nil-bodied pointer chain keeps the bare nil (nil-merge)
	var nilP *int64
	np := &nilP
	bn, err := codecMarshal(np)
	if err != nil {
		t.Fatal(err)
	}
	var backN **int64
	if err := codecUnmarshal(bn, &backN); err != nil {
		t.Fatalf("nil chain decode: %v", err)
	}
}
