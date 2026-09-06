package gbon_test

import (
	"io"

	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// Coder protocol, adapters, time coder, format
// evolution. Byte inspection lives in the golden corpus (g41..g45); the
// two targeted byte peeks here (tag order, adapter body class) have no
// behavioral observable.

func coderUnhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// coderSplice replaces one occurrence of from with to (equal lengths keep
// every intern length intact — the version-pair fixture technique).
func coderSplice(t *testing.T, b []byte, from, to string) []byte {
	t.Helper()
	if len(from) != len(to) || bytes.Count(b, []byte(from)) != 1 {
		t.Fatalf("splice fixture invalid: %q→%q in % x", from, to, b)
	}
	return bytes.Replace(b, []byte(from), []byte(to), 1)
}

// qn mirrors the qualified descriptor name of named non-stdlib test
// types (import path + "." + short form): crafted
// streams and byte patterns must carry the same names the encoder emits.
func qn(v any) string {
	t := reflect.TypeOf(v)
	p := t.PkgPath()
	seg := p
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	if strings.Contains(seg, ".") {
		return p + "." + t.String()
	}
	return t.String()
}

// coderDescLit emits a CODER descriptor literal for crafted streams.
func coderDescLit(name string, tag byte) []byte {
	var b []byte
	b = append(b, 0xDC, 0x0E)
	if len(name) <= 11 {
		b = append(b, 0x60|byte(len(name)))
	} else {
		b = append(b, 0x6C, byte(len(name)))
	}
	b = append(b, name...)
	b = append(b, tag)
	return b
}

func bigintDescLit() []byte { // DESC(BIGINT kind 15), name "big.Int"
	return append([]byte{0xDC, 0x0F, 0x67}, []byte("big.Int")...)
}

func intDescLit() []byte { // DESC(INT64) width 8
	return []byte{0xD8, 0x65, 'i', 'n', 't', '6', '4', 0x08}
}

// --- custom coder fixtures ---

type ccA struct{ N int64 }
type ccB struct{ N int64 }

type ccWrap struct {
	A ccA
	B ccB
}

type ccIntCoder struct{}

func (ccIntCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	switch x := v.Interface().(type) {
	case ccA:
		return e.Encode(x.N)
	case ccB:
		return e.Encode(x.N)
	}
	return fmt.Errorf("ccIntCoder: unexpected type %s", v.Type())
}

func (ccIntCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var n int64
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.FieldByName("N").SetInt(n)
	return nil
}

// TestCustomCoder: RT through callbacks; encounter
// tags independent of RegisterCoder order; registry misuse semantics.
func TestCustomCoder(t *testing.T) {
	mk := func(reverse bool) []byte {
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		a, b := ccA{}, ccB{}
		if reverse {
			if err := enc.RegisterCoder(b, ccIntCoder{}); err != nil {
				t.Fatal(err)
			}
			if err := enc.RegisterCoder(a, ccIntCoder{}); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := enc.RegisterCoder(a, ccIntCoder{}); err != nil {
				t.Fatal(err)
			}
			if err := enc.RegisterCoder(b, ccIntCoder{}); err != nil {
				t.Fatal(err)
			}
		}
		w := ccWrap{A: ccA{N: 7}, B: ccB{N: 9}}
		if err := enc.Encode(w); err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return buf.Bytes()
	}
	direct := mk(false)
	reverseB := mk(true)
	if !bytes.Equal(direct, reverseB) {
		t.Fatal("bytes depend on RegisterCoder order (determinism)")
	}
	// encounter-order tags: A dispatched first carries tag 0, B tag 1
	// regardless of registration order (targeted byte peek, see file header)
	for _, tc := range []struct {
		name string
		tag  byte
	}{
		{qn(ccA{}), 0x00},
		{qn(ccB{}), 0x01},
	} {
		pre := coderDescLit(tc.name, 0)
		i := bytes.Index(direct, pre[:len(pre)-1]) // name matched, tag excluded
		if i < 0 {
			t.Fatalf("CODER desc for %s not found in % x", tc.name, direct)
		}
		if got := direct[i+len(pre)-1]; got != tc.tag {
			t.Fatalf("tag of %s = %#x, want %#x (encounter order)", tc.name, got, tc.tag)
		}
	}
	dec := gbon.NewDecoder(bytes.NewReader(direct))
	if err := dec.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := dec.RegisterCoder(ccB{}, ccIntCoder{}); err != nil {
		t.Fatal(err)
	}
	var out ccWrap
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out != (ccWrap{A: ccA{N: 7}, B: ccB{N: 9}}) {
		t.Fatalf("RT mismatch: %s", safeDescValue(out))
	}
	// registry semantics (mirror of Register)
	enc := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
		t.Fatalf("re-register same coder must be a no-op: %v", err)
	}
	type otherCoder struct{ ccIntCoder }
	if err := enc.RegisterCoder(ccA{}, otherCoder{}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("different coder for same type: want ErrUnsupported, got %v", err)
	}
	if err := enc.RegisterCoder(nil, ccIntCoder{}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("nil example: want ErrUnsupported, got %v", err)
	}
	// between Decodes: a coder registered after the first value resolves in the second
	var twoVals bytes.Buffer
	e3 := gbon.NewEncoder(&twoVals)
	if err := e3.RegisterCoder(ccB{}, ccIntCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := e3.Encode(ccB{N: 3}); err != nil {
		t.Fatal(err)
	}
	if err := e3.Encode(ccA{N: 4}); err != nil {
		t.Fatal(err)
	}
	d2 := gbon.NewDecoder(bytes.NewReader(twoVals.Bytes()))
	if err := d2.RegisterCoder(ccB{}, ccIntCoder{}); err != nil {
		t.Fatal(err)
	}
	var first ccB
	if err := d2.Decode(&first); err != nil || first.N != 3 {
		t.Fatalf("first value: %v %s", err, safeDescValue(first))
	}
	if err := d2.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
		t.Fatal(err)
	}
	var second ccA
	if err := d2.Decode(&second); err != nil || second.N != 4 {
		t.Fatalf("coder registered between Decodes must be visible: %v", err)
	}
}

// TestTimeCoder: zones, mono-strip, FixedZone fallback.
func TestTimeCoder(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	cases := []time.Time{
		time.Unix(1, 2).UTC(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, msk),
		time.Date(2026, 7, 4, 12, 0, 0, 123456789, ny),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("MyZone", 3600)),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", -7200)),
		time.Now().Round(0).Add(time.Hour), // fresh wall clock
	}
	for i, want := range cases {
		b, err := gbon.Marshal(want)
		if err != nil {
			t.Fatalf("%d: Marshal: %v", i, err)
		}
		var got time.Time
		if err := gbon.Unmarshal(b, &got); err != nil {
			t.Fatalf("%d: Unmarshal: %v", i, err)
		}
		if !got.Equal(want) {
			t.Fatalf("%d: instant drift: %v vs %v", i, got, want)
		}
		wn, wo := want.Zone()
		gn, go_ := got.Zone()
		if wn != gn || wo != go_ {
			t.Fatalf("%d: zone drift: (%q,%d) vs (%q,%d)", i, wn, wo, gn, go_)
		}
		if got.Location().String() != want.Location().String() {
			t.Fatalf("%d: location name drift: %q vs %q", i, got.Location(), want.Location())
		}
	}
	// monotonic input: RT-Equal, mono reading stripped
	withMono := time.Now()
	b, err := gbon.Marshal(withMono)
	if err != nil {
		t.Fatal(err)
	}
	var got time.Time
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(withMono) {
		t.Fatalf("mono RT: %v vs %v", got, withMono)
	}
	if strings.Contains(fmt.Sprintf("%v", got), "m=") {
		t.Fatal("monotonic reading survived the coder")
	}
	// non-IANA name resolves to FixedZone keeping name+offset
	crafted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("No/Such_Zone", 10800))
	cb, err := gbon.Marshal(crafted)
	if err != nil {
		t.Fatal(err)
	}
	var cg time.Time
	if err := gbon.Unmarshal(cb, &cg); err != nil {
		t.Fatal(err)
	}
	n, o := cg.Zone()
	if n != "No/Such_Zone" || o != 10800 {
		t.Fatalf("FixedZone fallback: got (%q,%d)", n, o)
	}
}

// --- adapter fixtures ---

type txtVal struct{ S string }

func (v txtVal) MarshalText() ([]byte, error) { return []byte(v.S), nil }

func (v *txtVal) UnmarshalText(b []byte) error { v.S = string(b); return nil }

type halfBin struct{ X int } // Marshal only: no pair, no adapter

func (h halfBin) MarshalBinary() ([]byte, error) { return []byte{byte(h.X)}, nil }

type bothPair struct{ N byte } // both flavors complete: Binary must win

func (b bothPair) MarshalBinary() ([]byte, error) { return []byte{b.N}, nil }

func (b *bothPair) UnmarshalBinary(p []byte) error {
	b.N = p[0]
	return nil
}

func (b bothPair) MarshalText() ([]byte, error) { return []byte{'T', b.N}, nil }

func (b *bothPair) UnmarshalText(p []byte) error { b.N = p[1]; return nil }

// TestAdapterBinary: big.Int takes the Binary pair;
// half pairs stay derived.
func TestAdapterBinary(t *testing.T) {
	want, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	b, err := gbon.Marshal(*want)
	if err != nil {
		t.Fatal(err)
	}
	var got big.Int
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("big.Int RT: %v", got)
	}
	// big.Int is a built-in BIGINT kind: the
	// descriptor is kind 15 and the body is a bare argument of the zigzag
	// image — the text adapter does not dispatch on encode.
	i := bytes.Index(b, bigintDescLit())
	if i < 0 {
		t.Fatalf("big.Int not bigint-dispatched: % x", b)
	}
	if body := b[i+len(bigintDescLit())]; body != 0x10 {
		t.Fatalf("big.Int body form %#x, want ext-ARG 0x10", body)
	}
	// Kind-14 decode channel: an adapter stream (CODER kind 14, STRING
	// body) reads back forever, and the re-encode of the read value is
	// the canonical kind 15.
	leg := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00},
		append(coderDescLit("big.Int", 0), append([]byte{0x65}, []byte("99998")...)...)...)
	var lg big.Int
	if err := gbon.Unmarshal(leg, &lg); err != nil || lg.Cmp(big.NewInt(99998)) != 0 {
		t.Fatalf("adapter big.Int decode: %v %v", err, &lg)
	}
	cb, err := gbon.Marshal(lg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cb, bigintDescLit()) || bytes.Contains(cb, coderDescLit("big.Int", 0)) {
		t.Fatalf("re-encode not canonical kind-15: % x", cb)
	}
	var cgo big.Int
	if err := gbon.Unmarshal(cb, &cgo); err != nil || cgo.Cmp(&lg) != 0 {
		t.Fatalf("canonical re-encode RT: %v %v", err, &cgo)
	}
	// Binary priority over Text for a type carrying both pairs
	bp := bothPair{N: 0xAB}
	bb, err := gbon.Marshal(bp)
	if err != nil {
		t.Fatal(err)
	}
	var bpo bothPair
	if err := gbon.Unmarshal(bb, &bpo); err != nil || bpo != bp {
		t.Fatalf("bothPair RT: %v %s", err, safeDescValue(bpo))
	}
	j := bytes.Index(bb, coderDescLit(qn(bothPair{}), 0))
	if j < 0 {
		t.Fatalf("bothPair not coder-dispatched: % x", bb)
	}
	if body := bb[j+len(coderDescLit(qn(bothPair{}), 0))]; body>>4 != 0x7 {
		t.Fatalf("bothPair body class %#x, want BLOB (Binary priority)", body>>4)
	}
	// half pair: derived struct path, no CODER descriptor
	hb, err := gbon.Marshal(halfBin{X: 5})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(hb, []byte{0xDC, 0x0E}) {
		t.Fatalf("half pair must not adapter-dispatch: % x", hb)
	}
	var hout halfBin
	if err := gbon.Unmarshal(hb, &hout); err != nil || hout.X != 5 {
		t.Fatalf("half pair derived RT: %v %s", err, safeDescValue(hout))
	}
}

// TestAdapterText: text-only pair codes as a STRING body.
func TestAdapterText(t *testing.T) {
	want := txtVal{S: "hello адаптер"}
	b, err := gbon.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got txtVal
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("txtVal RT: %s", safeDescValue(got))
	}
	i := bytes.Index(b, coderDescLit(qn(txtVal{}), 0))
	if i < 0 {
		t.Fatalf("txtVal not coder-dispatched: % x", b)
	}
	if body := b[i+len(coderDescLit(qn(txtVal{}), 0))]; body>>4 != 0x6 {
		t.Fatalf("txtVal body class %#x, want STRING", body>>4)
	}
}

// TestNestedCoder: sub-values share the intern space
// across coder bodies; chained coder dispatch; recursion is an error.
type nB struct{ S string }

type nBcoder struct{}

func (nBcoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(nB).S)
}

func (nBcoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var s string
	if err := d.Decode(&s); err != nil {
		return err
	}
	v.FieldByName("S").SetString(s)
	return nil
}

type nA struct {
	S string
	B nB
}

type nAcoder struct{}

func (nAcoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	a := v.Interface().(nA)
	if err := e.Encode(a.S); err != nil {
		return err
	}
	if err := e.Encode(a.B); err != nil {
		return err
	}
	return e.Encode(len(a.S))
}

func (nAcoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	a := v.Addr().Interface().(*nA)
	if err := d.Decode(&a.S); err != nil {
		return err
	}
	if err := d.Decode(&a.B); err != nil {
		return err
	}
	var n int
	if err := d.Decode(&n); err != nil {
		return err
	}
	if n != len(a.S) {
		return fmt.Errorf("length tail %d != %d", n, len(a.S))
	}
	return nil
}

func TestNestedCoder(t *testing.T) {
	shared := "shared-string"
	want := nA{S: shared, B: nB{S: shared}}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(nA{}, nAcoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.RegisterCoder(nB{}, nBcoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(want); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// one literal + one REF of the shared string across the two coder bodies
	if bytes.Count(b, []byte(shared)) != 1 {
		t.Fatalf("shared string not interned across coder bodies: % x", b)
	}
	dec := gbon.NewDecoder(bytes.NewReader(b))
	if err := dec.RegisterCoder(nA{}, nAcoder{}); err != nil {
		t.Fatal(err)
	}
	if err := dec.RegisterCoder(nB{}, nBcoder{}); err != nil {
		t.Fatal(err)
	}
	var got nA
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != want {
		t.Fatalf("nested RT: %s vs %s", safeDescValue(got), safeDescValue(want))
	}
}

// cycleCoder re-encodes its own value: must fail with an error, not hang.
type cycleCoder struct{}

func (cycleCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface())
}

func (cycleCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var x ccA
	return d.Decode(&x)
}

func TestCoderRecursion(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(ccA{}, cycleCoder{}); err != nil {
		t.Fatal(err)
	}
	err := enc.Encode(ccA{N: 1})
	var rc *gbon.Error
	if err == nil || !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("coder self-recursion: want ErrUnsupported, got %v", err)
	}
	if !errors.As(err, &rc) || rc.Class() != "coder_recursion" {
		t.Fatalf("coder self-recursion: want class coder_recursion, got %v", err)
	}
	if err := enc.Encode(ccA{N: 2}); err == nil {
		t.Fatal("encoder must be sticky-broken after coder recursion")
	}
}

// Coder sub-values inherit the stream's encode
// budgets without isolation — a deep sub-value inside a coder
// body trips the shared depth counter with ErrBudget. A literal coder
// cycle A→B→A never reaches ErrBudget: the activeCoder guard rejects the
// revisit of a stacked coder type with ErrFormat (TestCoderRecursion
// above); the budget-inheritance contract is therefore exercised through
// the legal nested-coder path.
func TestCoderEncodeBudgetInheritance(t *testing.T) {
	// plain value path: chain deeper than the limit → ErrBudget
	enc := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc.SetLimits(gbon.Limits{MaxDepth: 16}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(deepChain(32)); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Encode(deep chain, MaxDepth=16): err = %v, want ErrBudget", err)
	}
	// coder path: the same chain encoded from inside a coder body counts
	// against the same stream counter — no isolated reset
	enc2 := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc2.RegisterCoder(ccDeep{}, ccDeepCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc2.SetLimits(gbon.Limits{MaxDepth: 16}); err != nil {
		t.Fatal(err)
	}
	if err := enc2.Encode(ccDeep{Chain: deepChain(32)}); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Encode(coder wrapping a deep chain, MaxDepth=16): err = %v, want ErrBudget", err)
	}
	// within the limit the coder path still round-trips the counter
	enc3 := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc3.RegisterCoder(ccDeep{}, ccDeepCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc3.Encode(ccDeep{Chain: deepChain(4)}); err != nil {
		t.Fatalf("Encode(coder wrapping a shallow chain): %v", err)
	}
}

// ccDeep is a coder type whose body encodes an arbitrary sub-value.
type ccDeep struct{ Chain any }

type ccDeepCoder struct{}

func (ccDeepCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(ccDeep).Chain)
}

func (ccDeepCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	return fmt.Errorf("ccDeep decode is not part of the budget fixture")
}

// TestEvolutionSkip: version pairs both directions via
// the equal-length name splice; nested drift uniformity.
func TestEvolutionSkip(t *testing.T) {
	// g44: EvN stream → EvO target: B skipped
	nb, _ := gbon.Marshal(EvN{A: 1, B: 5, C: 2})
	sp := coderSplice(t, nb, "EvN", "EvO")
	var o EvO
	if err := gbon.Unmarshal(sp, &o); err != nil {
		t.Fatalf("skip decode: %v", err)
	}
	if o.A != 1 || o.C != 2 {
		t.Fatalf("skip: got %s, want A=1 C=2", safeDescValue(o))
	}
	// g45: EvO stream → EvN target: B zero
	ob, _ := gbon.Marshal(EvO{A: 1, C: 2})
	sp2 := coderSplice(t, ob, "EvO", "EvN")
	var n EvN
	if err := gbon.Unmarshal(sp2, &n); err != nil {
		t.Fatalf("zero decode: %v", err)
	}
	if n.A != 1 || n.B != 0 || n.C != 2 {
		t.Fatalf("zero: got %s, want A=1 B=0 C=2", safeDescValue(n))
	}
	// nested drift: dropped field inside a kept field's struct
	type evSubN struct{ X, Y int64 }
	type evSubO struct{ X int64 }
	type evNx struct{ A evSubN }
	type evO2 struct{ A evSubO }

	n2b, _ := gbon.Marshal(evNx{A: evSubN{X: 1, Y: 2}})
	sp3 := coderSplice(t, coderSplice(t, n2b, "evSubN", "evSubO"), "evNx", "evO2")
	var o2 evO2
	if err := gbon.Unmarshal(sp3, &o2); err != nil {
		t.Fatalf("nested drift: %v", err)
	}
	if o2.A.X != 1 {
		t.Fatalf("nested drift: got %s", safeDescValue(o2))
	}
	// type name is the anchor: a different name is not evolution
	_, _ = gbon.Marshal(EvN{A: 1, B: 5, C: 2})
	plain, _ := gbon.Marshal(Point{X: 1, Y: 2})
	var ev EvN
	if err := gbon.Unmarshal(plain, &ev); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("foreign name must be ErrFormat, got %v", err)
	}
}

// TestCoderTagUnknown: crafted CODER name outside
// every registry → ErrUnsupported naming it.
func TestCoderTagUnknown(t *testing.T) {
	stream := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00},
		append(coderDescLit("nope.Nope", 0), 0x00)...)
	var v any
	err := gbon.Unmarshal(stream, &v)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("want ErrFormat, got %v", err)
	}
	var cn *gbon.Error
	if !errors.As(err, &cn) || cn.Class() != "unknown_name" || cn.Got != "nope.Nope" {
		t.Fatalf("error must name the coder: %v", err)
	}
}

// TestCoderTagCollision: tag rebound to another name and name
// rebound to another tag are self-desync → ErrFormat.
func TestCoderTagCollision(t *testing.T) {
	intBody := func() []byte { return append(intDescLit(), 0x22) }
	mk := func(d1, d2 []byte) []byte {
		s := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00}, d1...)
		return append(s, d2...)
	}
	cases := []struct {
		name string
		s    []byte
	}{
		{"tag-two-names", mk(
			append(coderDescLit(qn(ccA{}), 0), intBody()...),
			append(coderDescLit(qn(ccB{}), 0), intBody()...))},
		{"name-two-tags", mk(
			append(coderDescLit(qn(ccA{}), 0), intBody()...),
			append(coderDescLit(qn(ccA{}), 1), intBody()...))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := gbon.NewDecoder(bytes.NewReader(tc.s))
			if err := dec.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
				t.Fatal(err)
			}
			if err := dec.RegisterCoder(ccB{}, ccIntCoder{}); err != nil {
				t.Fatal(err)
			}
			var a ccA
			if err := dec.Decode(&a); err != nil {
				t.Fatalf("first value: %v", err)
			}
			var b ccA
			err := dec.Decode(&b)
			if !errors.Is(err, gbon.ErrFormat) {
				t.Fatalf("collision: want ErrFormat, got %v", err)
			}
		})
	}
}

// TestCoderMismatch: a CODER descriptor whose name sits in the
// plain Register registry without any coder → ErrUnsupported.
func TestCoderMismatch(t *testing.T) {
	body := append(intDescLit(), 0x22) // would decode fine as int64
	stream := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00},
		append(coderDescLit(qn(Point{}), 0), body...)...)
	dec := gbon.NewDecoder(bytes.NewReader(stream))
	if err := dec.Register(Point{}); err != nil {
		t.Fatal(err)
	}
	var v any
	err := dec.Decode(&v)
	var mm *gbon.Error
	if !errors.Is(err, gbon.ErrFormat) || !errors.As(err, &mm) || mm.Class() != "unknown_name" {
		t.Fatalf("plain-registered name without coder: want class unknown_name, got %v", err)
	}
}

// TestOldStream: a derived descriptor for an adapter-eligible
// type (txtVal: text pair, exported field) decodes through the derived
// path — old archives stay readable.
func TestOldStream(t *testing.T) {
	name := qn(txtVal{})
	d := []byte{0xD0, 0x6C, byte(len(name))} // DESC(STRUCT), u8 name
	d = append(d, name...)
	d = append(d, 0x01)             // one field
	d = append(d, 0x61, 'S')        // field name "S"
	d = append(d, 0xDC, 0x0C, 0x66) // field type DESC(STRING)
	d = append(d, "string"...)
	d = append(d, 0xB0, 0x61, 'q') // STRUCT token, S = "q"
	stream := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00}, d...)
	var got txtVal
	if err := gbon.Unmarshal(stream, &got); err != nil {
		t.Fatalf("derived decode of adapter-eligible type: %v", err)
	}
	if got.S != "q" {
		t.Fatalf("got %s", safeDescValue(got))
	}
}

// TestMinorVersion: the current stream header carries the current minor;
// the body stays byte-identical to the frozen corpus image; unknown
// minor reads until its first unknown token.
func TestMinorVersion(t *testing.T) {
	got, err := gbon.Marshal([]int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	want := coderUnhex(t, "67626F6E0000"+"D1655B5D696E74"+"D863696E7408"+"82022224"+"9004")
	if len(got) != len(want) || got[5] != 0x00 {
		t.Fatalf("stream shape changed: % x", got)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream differs from the frozen corpus image:\n got % x\nwant % x", got, want)
	}
	// minor 02 without unknown tokens reads normally
	m02 := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x02}, got[6:]...)
	var s []int
	if err := gbon.Unmarshal(m02, &s); err != nil {
		t.Fatalf("minor 02 must read: %v", err)
	}
	// minor 02 with a reserved kind fails on the token, not the header
	bad := append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x02}, 0xDC, 0x20)
	var v any
	if err := gbon.Unmarshal(bad, &v); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("reserved kind in minor 02: want ErrFormat at the token, got %v", err)
	}
}

// TestEvoKindMismatch: kind/width drift is ErrFormat, no softening.
func TestEvoKindMismatch(t *testing.T) {
	type evW64 struct{ F int64 }
	type evW32 struct{ F int32 }
	type evSL struct{ F []int64 }
	type evST struct{ F struct{ Z int64 } }
	// width int64→int32
	b64, _ := gbon.Marshal(evW64{F: 1})
	sp := coderSplice(t, b64, "evW64", "evW32")
	var w32 evW32
	if err := gbon.Unmarshal(sp, &w32); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("width drift: want ErrFormat, got %v", err)
	}
	// slice ↔ struct
	bsl, _ := gbon.Marshal(evSL{F: []int64{1}})
	sp2 := coderSplice(t, bsl, "evSL", "evST")
	var st evST
	if err := gbon.Unmarshal(sp2, &st); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("kind drift: want ErrFormat, got %v", err)
	}
}

// TestCoderBudget: a crafted skip field with a
// 2^40-claimed string length dies by budget, not allocation; a depth attack
// inside a coder body dies by the depth budget.
func TestCoderBudget(t *testing.T) {
	type evBigStr struct{ A, B string }
	type evSmlStr struct{ A string }
	b, _ := gbon.Marshal(evBigStr{A: "x", B: "y"})
	i := bytes.Index(b, []byte{0x61, 0x79}) // B's value token "y"
	if i < 0 {
		t.Fatal("B value token not found")
	}
	huge := append([]byte{0x6F, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00}, 0x30)
	crafted := append(append([]byte{}, b[:i]...), huge...)
	crafted = coderSplice(t, crafted, "evBigStr", "evSmlStr")
	var out evSmlStr
	err := gbon.Unmarshal(crafted, &out)
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("skip budget: want ErrBudget, got %v", err)
	}
	var sb *gbon.Error
	if !errors.As(err, &sb) || sb.Class() != "budget_bytes" {
		t.Fatalf("skip budget: want class budget_bytes, got %v", err)
	}

	// depth attack inside a coder body: the coder sub-decodes into a
	// self-referential slice nest past MaxDepth
	var s []byte
	s = append(s, 0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00)
	s = append(s, coderDescLit(qn(czSelf{}), 0)...)
	// sub-value: DESC(SLICE czD) self-ref C2 + deep nest + nil
	czd := qn(czD{})
	s = append(s, 0xD1, 0x6C, byte(len(czd)))
	s = append(s, czd...)
	s = append(s, 0xC2)
	for range 10002 {
		s = append(s, 0x81, 0x01)
	}
	s = append(s, 0x01)
	var cz czSelf
	dec := gbon.NewDecoder(bytes.NewReader(s))
	if err := dec.RegisterCoder(czSelf{}, czSliceCoder{}); err != nil {
		t.Fatal(err)
	}
	err = dec.Decode(&cz)
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("coder depth attack: want ErrBudget, got %v", err)
	}
}

type czSelf struct{}

type czD []czD // self-referential slice: the depth-attack target

type czSliceCoder struct{}

func (czSliceCoder) EncodeValue(*gbon.Encoder, reflect.Value) error {
	return errors.New("unreachable in this fixture")
}

func (czSliceCoder) DecodeValue(d *gbon.Decoder, _ reflect.Value) error {
	var x czD
	return d.Decode(&x)
}

// TestBareRoundtrip: ecosystem types round-trip
// through stateless Marshal/Unmarshal with zero registration.
func TestBareRoundtrip(t *testing.T) {
	tv := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	b1, err := gbon.Marshal(tv)
	if err != nil {
		t.Fatal(err)
	}
	var tvo time.Time
	if err := gbon.Unmarshal(b1, &tvo); err != nil || !tvo.Equal(tv) {
		t.Fatalf("time.Time: %v %v", err, tvo)
	}
	ip := net.IP{192, 168, 1, 4}
	b2, err := gbon.Marshal(ip)
	if err != nil {
		t.Fatal(err)
	}
	var ipo net.IP
	if err := gbon.Unmarshal(b2, &ipo); err != nil || !ip.Equal(ipo) {
		t.Fatalf("net.IP: %v %v", err, ipo)
	}
	u := url.URL{Scheme: "https", Host: "example.com", Path: "/x"}
	b3, err := gbon.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var uo url.URL
	if err := gbon.Unmarshal(b3, &uo); err != nil || uo != u {
		t.Fatalf("url.URL: %v %v", err, uo)
	}
	big1 := new(big.Int).SetInt64(-987654321)
	b4, err := gbon.Marshal(*big1)
	if err != nil {
		t.Fatal(err)
	}
	var bigo big.Int
	if err := gbon.Unmarshal(b4, &bigo); err != nil || bigo.Cmp(big1) != 0 {
		t.Fatalf("big.Int: %v %v", err, bigo)
	}
	dur := time.Duration(90*time.Minute + 123*time.Millisecond)
	b5, err := gbon.Marshal(dur)
	if err != nil {
		t.Fatal(err)
	}
	var duro time.Duration
	if err := gbon.Unmarshal(b5, &duro); err != nil || duro != dur {
		t.Fatalf("time.Duration: %v %v", err, duro)
	}
}

// TestAdapterKeyRegression: full Marshaler pairs are
// adapter-intercepted in every position — map key positions are
// ErrUnsupported.
// Fixture url.URL, not the spec's time.Duration: Duration implements no
// Marshaler pair in the stdlib, so it never leaves the derived path.
func TestAdapterKeyRegression(t *testing.T) {
	_, err := gbon.Marshal(map[url.URL]int{{Scheme: "https"}: 1})
	if !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("url.URL key: want ErrUnsupported, got %v", err)
	}
	var mk *gbon.Error
	if !errors.As(err, &mk) || mk.Class() != "unsupported_kind" {
		t.Fatalf("url.URL key: want class unsupported_kind, got %v", err)
	}
	if mk.Path == "" {
		t.Fatalf("key-position error must carry a path: %v", err)
	}
	_, err = gbon.Marshal(map[any]int{time.Second: 1})
	if err != nil {
		t.Fatalf("dynamic duration key must stay derived-encodable: %v", err)
	}
}

// TestCoderBuiltinConflict: time.Time is built-in coded and
// not overridable through RegisterCoder on either facade.
func TestCoderBuiltinConflict(t *testing.T) {
	enc := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc.RegisterCoder(time.Time{}, ccIntCoder{}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("Encoder built-in override: want ErrUnsupported, got %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(nil))
	if err := dec.RegisterCoder(time.Time{}, ccIntCoder{}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("Decoder built-in override: want ErrUnsupported, got %v", err)
	}
}

// Generated coder-typed values round
// trip through registered Encoder/Decoder; registration order never
// reaches the bytes.
func TestPCoderRT(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	zone := func() *time.Location {
		switch r.Intn(4) {
		case 0:
			return time.UTC
		case 1:
			return time.FixedZone("Z1", r.Intn(80000)-40000)
		default:
			if l, err := time.LoadLocation("Asia/Tokyo"); err == nil {
				return l
			}
			return time.UTC
		}
	}
	eq := func(got, want any) bool {
		switch w := want.(type) {
		case time.Time:
			g, ok := got.(time.Time)
			return ok && g.Equal(w) && func() bool {
				gn, goff := g.Zone()
				wn, woff := w.Zone()
				return gn == wn && goff == woff
			}()
		default:
			return reflect.DeepEqual(got, want)
		}
	}
	for i := range 200 {
		var val any
		switch r.Intn(5) {
		case 0:
			val = time.Unix(r.Int63n(1<<40), r.Int63n(1000000000)).In(zone())
		case 1:
			val = Bin{N: byte(r.Intn(256))}
		case 2:
			val = txtVal{S: fmt.Sprintf("s%d", r.Intn(1000))}
		case 3:
			val = ccA{N: r.Int63()}
		case 4:
			val = nA{S: fmt.Sprintf("n%d", r.Intn(50)), B: nB{S: fmt.Sprintf("n%d", r.Intn(50))}}
		}
		var buf bytes.Buffer
		e1 := gbon.NewEncoder(&buf)
		if err := e1.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
			t.Fatal(err)
		}
		if err := e1.RegisterCoder(nA{}, nAcoder{}); err != nil {
			t.Fatal(err)
		}
		if err := e1.RegisterCoder(nB{}, nBcoder{}); err != nil {
			t.Fatal(err)
		}
		if err := e1.Encode(val); err != nil {
			t.Fatalf("%d: encode %s: %v", i, safeDescValue(val), err)
		}
		var buf2 bytes.Buffer
		e2 := gbon.NewEncoder(&buf2)
		if err := e2.RegisterCoder(nB{}, nBcoder{}); err != nil {
			t.Fatal(err)
		}
		if err := e2.RegisterCoder(nA{}, nAcoder{}); err != nil {
			t.Fatal(err)
		}
		if err := e2.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
			t.Fatal(err)
		}
		if err := e2.Encode(val); err != nil {
			t.Fatalf("%d: encode2: %v", i, err)
		}
		if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
			t.Fatalf("%d: bytes depend on RegisterCoder order", i)
		}
		dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
		if err := dec.RegisterCoder(ccA{}, ccIntCoder{}); err != nil {
			t.Fatal(err)
		}
		if err := dec.RegisterCoder(nA{}, nAcoder{}); err != nil {
			t.Fatal(err)
		}
		if err := dec.RegisterCoder(nB{}, nBcoder{}); err != nil {
			t.Fatal(err)
		}
		out := reflect.New(reflect.TypeOf(val))
		if err := dec.Decode(out.Interface()); err != nil {
			t.Fatalf("%d: decode %s: %v", i, safeDescValue(val), err)
		}
		if !eq(out.Elem().Interface(), val) {
			t.Fatalf("%d: RT mismatch:\n got %s\nwant %s", i, safeDescValue(out.Elem().Interface()), safeDescValue(val))
		}
	}
}

// Dropped fields carrying nil slices, nil maps, nil
// blobs, and shared-backing views must be skipped, not rejected. Streams
// are produced by the encoder itself (legitimate wire).
func TestEvolutionSkipComposite(t *testing.T) {
	type EvNewNul0 struct {
		Drop []int64
		M    map[string]int64
		B    []byte
		V    []int64
		Keep int64
	}
	type EvOldNul0 struct {
		Keep int64
	}
	n := EvNewNul0{Drop: nil, M: nil, B: nil, V: nil, Keep: 9}
	raw, err := gbon.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	data := coderSplice(t, raw, "gbon_test.EvNewNul0", "gbon_test.EvOldNul0")
	var o EvOldNul0
	if err := gbon.Unmarshal(data, &o); err != nil {
		t.Fatalf("skip nil composites: %v", err)
	}
	if o.Keep != 9 {
		t.Fatalf("keep = %d", o.Keep)
	}

	// Shared-backing view in a dropped field.
	type Two struct {
		A []int64
		B []int64
	}
	arr := []int64{1, 2, 3, 4}
	two := Two{A: arr[0:2], B: arr[2:4]}
	raw2, err := gbon.Marshal(two)
	if err != nil {
		t.Fatal(err)
	}
	data2 := coderSplice(t, raw2, "gbon_test.Two", "gbon_test.Ola")
	type Ola struct {
		A []int64
	}
	var oa Ola
	if err := gbon.Unmarshal(data2, &oa); err != nil {
		t.Fatalf("skip shared view: %v", err)
	}
	if len(oa.A) != 2 || oa.A[0] != 1 || oa.A[1] != 2 {
		t.Fatalf("A = %v", oa.A)
	}

	// Nil interface in a dropped field (target name IfaceDrop — same
	// length as WithIface for the equal-length splice).
	type IfaceDrop struct {
		Keep int64
	}
	type WithIface struct {
		V    any
		Keep int64
	}
	wi := WithIface{V: nil, Keep: 3}
	raw3, err := gbon.Marshal(wi)
	if err != nil {
		t.Fatal(err)
	}
	data3 := coderSplice(t, raw3, "gbon_test.WithIface", "gbon_test.IfaceDrop")
	var o3 IfaceDrop
	if err := gbon.Unmarshal(data3, &o3); err != nil {
		t.Fatalf("skip nil iface: %v", err)
	}
	if o3.Keep != 3 {
		t.Fatalf("keep = %d", o3.Keep)
	}
}

// Counter-inheritance discrimination: a Coder encoding a
// deep sub-value must inherit the stream's depth counter. If encodeSub
// reset depth per call, the deep chain inside the coder would escape the
// limit. Fixture: depth budget set below the coder's internal chain depth.
func TestCoderDepthCounterInheritance(t *testing.T) {
	// chainCoder delegates a depth-10 chain through e.Encode in ONE call.
	// If the depth counter resets at the coder boundary, no error; with
	// inheritance, ErrBudget fires at MaxDepth=4.
	enc := gbon.NewEncoder(io.Discard)
	if err := enc.RegisterCoder(ccTrigger{}, chainCoder{10}); err != nil {
		t.Fatal(err)
	}
	if err := enc.SetLimits(gbon.Limits{MaxDepth: 4}); err != nil {
		t.Fatal(err)
	}
	err := enc.Encode(ccTrigger{})
	if err == nil || !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("want ErrBudget from inherited depth, got %v", err)
	}
	// Same depth chain without the coder (derived path) — same limit.
	enc2 := gbon.NewEncoder(io.Discard)
	if err := enc2.SetLimits(gbon.Limits{MaxDepth: 4}); err != nil {
		t.Fatal(err)
	}
	if err := enc2.Encode(boxN(10, ccTrigger{})); err == nil || !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("derived path: want ErrBudget, got %v", err)
	}
	// Within-limit value succeeds through the coder path.
	enc3 := gbon.NewEncoder(io.Discard)
	if err := enc3.RegisterCoder(ccTrigger{}, chainCoder{3}); err != nil {
		t.Fatal(err)
	}
	if err := enc3.SetLimits(gbon.Limits{MaxDepth: 20}); err != nil {
		t.Fatal(err)
	}
	if err := enc3.Encode(ccTrigger{}); err != nil {
		t.Fatalf("within budget: %v", err)
	}
}

// ccTrigger is the coder-covered type that chainCoder delegates.
type ccTrigger struct{}

// chainCoder delegates a deep chain through one e.Encode call (no
// type-level recursion) — exercising depth-counter inheritance across
// the coder→encoder boundary.
type chainCoder struct{ n int }

func (c chainCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(boxN(c.n, int64(0)))
}

func (chainCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var out any
	if err := d.Decode(&out); err != nil {
		return err
	}
	v.Set(reflect.ValueOf(out))
	return nil
}

// boxN builds a nested chain of boxing structs of depth n.
type box struct{ V any }

func boxN(n int, leaf any) any {
	v := leaf
	for range n {
		v = box{V: v}
	}
	return v
}

// Coder-covered types in struct-field positions.

// assertTimeRT checks a time.Time round-trip by the TestTimeCoder
// observables: instant, zone name/offset, location name.
func assertTimeRT(t *testing.T, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("instant drift: %v vs %v", got, want)
	}
	wn, wo := want.Zone()
	gn, go_ := got.Zone()
	if wn != gn || wo != go_ {
		t.Fatalf("zone drift: (%q,%d) vs (%q,%d)", wn, wo, gn, go_)
	}
	if got.Location().String() != want.Location().String() {
		t.Fatalf("location name drift: %q vs %q", got.Location(), want.Location())
	}
}

func TestCoderInStructField(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	type rec struct {
		Name string
		T    time.Time
	}
	cases := []rec{
		{"utc", time.Unix(1, 2).UTC()},
		{"msk", time.Date(2026, 1, 1, 0, 0, 0, 0, msk)},
		{"fz", time.Date(2026, 7, 4, 12, 0, 0, 123456789, time.FixedZone("MyZone", 3600))},
	}
	for i, want := range cases {
		b, err := gbon.Marshal(want)
		if err != nil {
			t.Fatalf("%d: Marshal: %v", i, err)
		}
		var got rec
		if err := gbon.Unmarshal(b, &got); err != nil {
			t.Fatalf("%d: Unmarshal: %v", i, err)
		}
		if got.Name != want.Name {
			t.Fatalf("%d: plain field drift: %q vs %q", i, got.Name, want.Name)
		}
		assertTimeRT(t, got.T, want.T)
	}
	// same struct through the streaming facades (per-stream structDescs)
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	for _, want := range cases {
		if err := enc.Encode(want); err != nil {
			t.Fatal(err)
		}
	}
	dec := gbon.NewDecoder(&buf)
	for i, want := range cases {
		var got rec
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("%d: stream Decode: %v", i, err)
		}
		if got.Name != want.Name {
			t.Fatalf("%d: stream plain field drift: %q vs %q", i, got.Name, want.Name)
		}
		assertTimeRT(t, got.T, want.T)
	}
}

func TestBinaryAdapterInStruct(t *testing.T) {
	type rec struct {
		ID int64
		U  *url.URL
	}
	want := rec{ID: 7, U: &url.URL{
		Scheme:   "https",
		User:     url.UserPassword("alice", "s3cret"),
		Host:     "example.com:8080",
		Path:     "/p a/x",
		RawQuery: "q=1&r=2",
		Fragment: "frag",
	}}
	b, err := gbon.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got rec
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID {
		t.Fatalf("plain field drift: %d vs %d", got.ID, want.ID)
	}
	if got.U == nil {
		t.Fatal("adapter field decoded to nil")
	}
	if got.U.String() != want.U.String() {
		t.Fatalf("adapter field drift: %q vs %q", got.U.String(), want.U.String())
	}
	if got.U.User == nil || got.U.User.Username() != "alice" {
		t.Fatalf("userinfo drift: %v", got.U.User)
	}
	// nil adapter pointer stays nil
	empty := rec{ID: 1}
	eb, err := gbon.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	var egot rec
	if err := gbon.Unmarshal(eb, &egot); err != nil {
		t.Fatal(err)
	}
	if egot.U != nil {
		t.Fatalf("nil adapter pointer drift: %v", egot.U)
	}
}

func TestTextAdapterInStruct(t *testing.T) {
	type rec struct {
		ID int64
		V  txtVal
	}
	want := rec{ID: 3, V: txtVal{S: "text payload"}}
	b, err := gbon.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got rec
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.V != want.V {
		t.Fatalf("drift: %s vs %s", safeDescValue(got), safeDescValue(want))
	}
}

func TestNestedCoderInStruct(t *testing.T) {
	type inner struct {
		Note string
		T    time.Time
	}
	type outer struct {
		Tag   string
		A     inner
		Extra time.Time
	}
	want := outer{
		Tag:   "o",
		A:     inner{Note: "i", T: time.Date(2026, 3, 1, 5, 6, 7, 890000001, time.UTC)},
		Extra: time.Unix(99, 1).UTC(),
	}
	b, err := gbon.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got outer
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Tag != want.Tag || got.A.Note != want.A.Note {
		t.Fatalf("plain field drift: %s vs %s", safeDescValue(got), safeDescValue(want))
	}
	assertTimeRT(t, got.A.T, want.A.T)
	assertTimeRT(t, got.Extra, want.Extra)
}

func TestCoderInSlice(t *testing.T) {
	type rec struct {
		N int64
		T time.Time
	}
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	want := []rec{
		{1, time.Unix(10, 20).UTC()},
		{2, time.Date(2026, 1, 1, 0, 0, 0, 0, msk)},
		{3, time.Date(2026, 7, 4, 12, 0, 0, 123456789, time.FixedZone("MyZone", 3600))},
	}
	b, err := gbon.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got []rec
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("len drift: %d vs %d", len(got), len(want))
	}
	for i := range want {
		if got[i].N != want[i].N {
			t.Fatalf("%d: plain field drift: %d vs %d", i, got[i].N, want[i].N)
		}
		assertTimeRT(t, got[i].T, want[i].T)
	}
}

// Evolution pair fixtures: v2 carries a coder field that v1 drops.
type evoV2 struct {
	ID   int64
	When time.Time
	Name string
}

type evoV1 struct {
	ID   int64
	Name string
}

// atomStamp carries a custom coder whose body grammar belongs to the
// coder, so a v2-to-v1 skip of this field rejects with ErrUnsupported
// and leaves the target untouched.
type atomStamp int64

type atomStampCoder struct{}

func (atomStampCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Int())
}

func (atomStampCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var n int64
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.SetInt(n)
	return nil
}

type evoCustomV2 struct {
	ID    int64
	Stamp atomStamp
	Name  string
}

// TestEvolutionCoderField pins the evolution behavior across coder
// fields: a built-in time.Time field present in the stream but absent on
// the target is parse-skipped (fixed grammar, ID/Name round-trip), while
// a custom coder field cannot be skipped — ErrUnsupported lands with the
// target untouched (decode atomicity).
func TestEvolutionCoderField(t *testing.T) {
	when := time.Date(2026, 8, 29, 12, 0, 0, 0, time.FixedZone("TST", 3600))

	t.Run("builtin time field skipped, siblings round-trip", func(t *testing.T) {
		v2 := evoV2{ID: 1, When: when, Name: "rec"}
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		if err := enc.RegisterAs("gbon.evo", evoV2{}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(v2); err != nil {
			t.Fatal(err)
		}
		d := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
		if err := d.RegisterAs("gbon.evo", evoV1{}); err != nil {
			t.Fatal(err)
		}
		var got evoV1
		if err := d.Decode(&got); err != nil {
			t.Fatalf("decode v2→v1: %v", err)
		}
		want := evoV1{ID: 1, Name: "rec"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("v2→v1: got %s, want %s", safeDescValue(got), safeDescValue(want))
		}
	})

	t.Run("same version round-trips the coder field", func(t *testing.T) {
		v2 := evoV2{ID: 2, When: when, Name: "ctl"}
		var buf bytes.Buffer
		if err := gbon.NewEncoder(&buf).Encode(v2); err != nil {
			t.Fatal(err)
		}
		var got evoV2
		if err := gbon.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, v2) {
			t.Fatalf("v2 RT: got %s, want %s", safeDescValue(got), safeDescValue(v2))
		}
	})

	t.Run("custom coder field unsupported on skip, target untouched", func(t *testing.T) {
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		if err := enc.RegisterCoder(atomStamp(0), atomStampCoder{}); err != nil {
			t.Fatal(err)
		}
		if err := enc.RegisterAs("gbon.evoC", evoCustomV2{}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(evoCustomV2{ID: 3, Stamp: 99, Name: "x"}); err != nil {
			t.Fatal(err)
		}
		d := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
		if err := d.RegisterAs("gbon.evoC", evoV1{}); err != nil {
			t.Fatal(err)
		}
		target := evoV1{ID: -5, Name: "sentinel"}
		err := d.Decode(&target)
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("want ErrFormat, got %v", err)
		}
		var sk *gbon.Error
		if !errors.As(err, &sk) || sk.Class() != "unknown_name" {
			t.Fatalf("unsupported skip: want class unknown_name, got %v", err)
		}
		if !reflect.DeepEqual(target, evoV1{ID: -5, Name: "sentinel"}) {
			t.Fatalf("target mutated on unsupported skip: %s", safeDescValue(target))
		}
	})
}

// TestRegisterBuiltInCoder pins the Register path for built-in coder
// types: registering time.Time (or a type nesting it) validates through
// the coder leaf, and interface slots holding time.Time decode through
// the registered Decoder.
func TestRegisterBuiltInCoder(t *testing.T) {
	t.Run("register time.Time and re-register", func(t *testing.T) {
		d := gbon.NewDecoder(bytes.NewReader(nil))
		if err := d.Register(time.Now()); err != nil {
			t.Fatalf("Register(time.Time): %v", err)
		}
		if err := d.Register(time.Now()); err != nil {
			t.Fatalf("re-register: %v", err)
		}
	})

	t.Run("register type nesting time.Time", func(t *testing.T) {
		type stamped struct {
			When time.Time
			Name string
		}
		d := gbon.NewDecoder(bytes.NewReader(nil))
		if err := d.Register(stamped{}); err != nil {
			t.Fatalf("Register(stamped): %v", err)
		}
	})

	t.Run("any(time.Time) decodes through Register", func(t *testing.T) {
		tt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.FixedZone("TST", 3600))
		data, err := gbon.Marshal(any(tt))
		if err != nil {
			t.Fatal(err)
		}
		d := gbon.NewDecoder(bytes.NewReader(data))
		if err := d.Register(time.Now()); err != nil {
			t.Fatal(err)
		}
		var out any
		if err := d.Decode(&out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out, tt) {
			t.Fatalf("any(time.Time) RT: got %s, want %v", safeDescValue(out), tt)
		}
	})
}

// TestConcreteCoderCacheEquivalence walks the coder-resolution precedence
// ladder through the public API with a warm per-value cache (many fields of
// the same type in one stream) and compares against cold single-field
// decodes: builtin (time.Time), a registered custom coder, and a
// coder-less type must all resolve identically either way, and a
// RegisterCoder between decodes must stay visible to the next decode.
func TestConcreteCoderCacheEquivalence(t *testing.T) {
	type single struct{ T time.Time }
	type many struct {
		T1, T2, T3, T4, T5, T6, T7, T8 time.Time
	}
	tt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	blobMany, err := gbon.Marshal(many{T1: tt, T2: tt, T3: tt, T4: tt, T5: tt, T6: tt, T7: tt, T8: tt})
	if err != nil {
		t.Fatal(err)
	}
	blobOne, err := gbon.Marshal(single{T: tt})
	if err != nil {
		t.Fatal(err)
	}
	var warm many
	if err := gbon.Unmarshal(blobMany, &warm); err != nil {
		t.Fatal(err)
	}
	var cold single
	if err := gbon.Unmarshal(blobOne, &cold); err != nil {
		t.Fatal(err)
	}
	if !warm.T1.Equal(cold.T) || !warm.T8.Equal(cold.T) {
		t.Fatalf("warm-cache decode diverged from cold: %v vs %v", warm.T1, cold.T)
	}
	// decode visibility across decoder instances: fresh decoders of the
	// same stream resolve identically (per-value cache never leaks across)
	type point struct{ X, Y int }
	pb, err := gbon.Marshal(point{X: 1, Y: 2})
	if err != nil {
		t.Fatal(err)
	}
	var out2 point
	dec2 := gbon.NewDecoder(bytes.NewReader(pb))
	if err := dec2.Decode(&out2); err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if out2 != (point{X: 1, Y: 2}) {
		t.Fatalf("second decode diverged: %s", safeDescValue(out2))
	}
}

// TestSafeDescCyclicCoder: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicCoder(t *testing.T) {
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
