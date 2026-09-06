package gbon_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Budget-contract tests: decode charges derived backing
// allocations at their production point and encode caps the derived
// output resources — depth/nodes/bytes.

// g51 allocbomb (spec golden, hand-computed): []int64 ARRAY L=10^8 E=0 —
// 31 bytes claiming an 8×10^8-byte backing.
const g51Hex = "67626F6E0000" + // header: magic "gbon", 0.0
	"D1" + "67" + "5B5D696E743634" + // DESC-SLICE id0, name "[]int64" id1 (inline 7)
	"D8" + "65" + "696E743634" + "08" + // DESC-INT id2, name "int64" id3 (inline 5), width 8
	"8E" + "05F5E100" + "00" + // ARRAY id4: L=10^8 (u32), E=0 (inline)
	"90" + "04" // VIEW form0 → id4

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// appendStrLit appends a string literal: inline selector 0x6<<4|len for
// len ≤ 11, else 0x6C + u8 length.
func appendStrLit(s []byte, str string) []byte {
	if len(str) <= 11 {
		return append(append(s, 0x60|byte(len(str))), str...)
	}
	return append(append(s, 0x6C, byte(len(str))), str...)
}

// craftedSliceStream builds a single-slice stream through the crafted
// generator: header, DESC-SLICE with the element DESC
// inline, ARRAY L E=0, VIEW form0 — byte-identical to the
// hand-computed form (pinned by TestCraftedLegacyEquivalence on g51).
func craftedSliceStream(elem *cDesc, L uint64) []byte {
	c := newCraft()
	c.descPos(dSlice(elem))
	r := c.arrayRec(L, 0)
	c.view0(r)
	return c.buf
}

// Element DESC fixtures (generator-owned cDesc trees).
var (
	descInt64      = dInt64
	descComplex128 = dC128
	descInt8       = dInt8
)

// Crafted counterexample — ErrBudget without the 8×10^8 allocation.
func TestBudgetAllocBombG51(t *testing.T) {
	data := mustHex(t, g51Hex)
	if len(data) != 31 {
		t.Fatalf("g51 length = %d, want 31", len(data))
	}
	var v []int64
	if err := gbon.Unmarshal(data, &v); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Unmarshal(g51, &[]int64{}): err = %v, want ErrBudget", err)
	}
}

// Blob boundary — a legit []byte L=200 E=0 under MaxBytes=100 fails
// before MakeSlice; the [E,L) zero tail is charged (L, not E).
func TestBudgetBlobChargeLNotE(t *testing.T) {
	blob := make([]byte, 0, 200) // backing L=200, dense prefix E=0
	data, err := gbon.Marshal(blob)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	d := gbon.NewDecoder(bytes.NewReader(data))
	d.SetLimits(gbon.Limits{MaxBytes: 100})
	var out []byte
	if err := d.Decode(&out); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Decode(blob L=200 E=0, MaxBytes=100): err = %v, want ErrBudget", err)
	}
}

// deepBox is the deep-value fixture for encode-depth: one recursive type
// (no per-level type names — a *N pointer chain grows quadratic descriptor
// names), nesting carried through the any field.
type deepBox struct{ V any }

// deepChain builds a deepBox value graph of depth n (two recursion
// frames per level) — the moderate-depth fixture.
func deepChain(n int) any {
	v := any(0)
	for range n {
		v = deepBox{V: v}
	}
	return v
}

// es-axes — []complex128 L=10^8 charges 16×10^8; []int8 L=10^8
// charges 10^8 (boundary at L == MaxSliceLen default: the wire guard
// passes, the charge plus consumed descriptor bytes must not).
func TestBudgetChargeElemsizeAxes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elemDesc *cDesc
		example  any
	}{
		{"complex128 es=16", descComplex128, []complex128(nil)},
		{"int8 es=1", descInt8, []int8(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := craftedSliceStream(tc.elemDesc, 100000000)
			d := gbon.NewDecoder(bytes.NewReader(data))
			if err := d.Register(tc.example); err != nil {
				t.Fatalf("Register: %v", err)
			}
			out := reflect.New(reflect.TypeOf(tc.example)).Interface()
			if err := d.Decode(out); !errors.Is(err, gbon.ErrBudget) {
				t.Fatalf("Decode: err = %v, want ErrBudget", err)
			}
		})
	}
}

// ckBinary BLOB — the 5th charge point (codec_desc.go make([]byte,L)):
// a crafted Binary-adapter body with L=10^8 E=0 charges 10^8 before the make.
type sb9Bin struct{ data []byte }

func (b sb9Bin) MarshalBinary() ([]byte, error) {
	return b.data, nil
}

func (b *sb9Bin) UnmarshalBinary(d []byte) error {
	b.data = append([]byte{}, d...)
	return nil
}

func TestBudgetCkBinaryCharge(t *testing.T) {
	// header + DESC-CODER (kind 14 rides the u8 ARG form: DC 0E) +
	// name qn(sb9Bin{}) + tag 0 + BLOB L=10^8 (u32) E=0.
	// Id order: id0 desc, id1 name, id2 blob.
	s := append([]byte{}, hdr...)
	s = append(s, 0xDC, 0x0E) // DESC + u8-kind ARG = 14 (KindCoder)
	s = appendStrLit(s, qn(sb9Bin{}))
	s = append(s, 0x00)                   // coder tag 0 inline
	s = append(s, 0x7E)                   // BLOB token, u32 L
	s = append(s, 0x05, 0xF5, 0xE1, 0x00) // L = 10^8 (0x05F5E100)
	s = append(s, 0x00)                   // E = 0
	var v sb9Bin
	if err := gbon.Unmarshal(s, &v); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Unmarshal(ckBinary L=10^8 E=0): err = %v, want ErrBudget", err)
	}
}

// Encode depth — pointer chain 10^6 → ErrBudget (not a fatal
// stack); 10^5 → OK at the encode defaults.
func TestEncodeDepthBudget(t *testing.T) {
	if _, err := gbon.Marshal(deepChain(100000)); err != nil {
		t.Fatalf("Marshal(depth 10^5): %v", err)
	}
	// At the default depth budget a 10^6-deep graph is ErrBudget, not a
	// fatal stack. Race builds pay ~4x per frame and overflow the 1GB
	// goroutine stack before the counter reaches the guard, so the
	// default-threshold leg runs only in regular builds; race builds
	// verify the same guard at a configured threshold.
	if raceEnabled {
		e := gbon.NewEncoder(&bytes.Buffer{})
		if err := e.SetLimits(gbon.Limits{MaxDepth: 1000}); err != nil {
			t.Fatal(err)
		}
		if err := e.Encode(deepChain(501)); !errors.Is(err, gbon.ErrBudget) {
			t.Fatalf("Encode(depth %d, MaxDepth=1000): err = %v, want ErrBudget", 2*501+1, err)
		}
	} else if _, err := gbon.Marshal(deepChain(1000000)); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Marshal(depth 10^6): err = %v, want ErrBudget", err)
	}
}

// Encode bytes + sticky — MaxBytes=64, a 100-byte string →
// ErrBudget; every subsequent Encode returns the same error; the partial
// buffer never flushes.
func TestEncodeBytesSticky(t *testing.T) {
	var sink bytes.Buffer
	e := gbon.NewEncoder(&sink)
	if err := e.SetLimits(gbon.Limits{MaxBytes: 64}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	first := e.Encode(strings.Repeat("x", 100))
	if !errors.Is(first, gbon.ErrBudget) {
		t.Fatalf("Encode(100B string, MaxBytes=64): err = %v, want ErrBudget", first)
	}
	if second := e.Encode("ok"); !errors.Is(second, first) {
		t.Fatalf("Encode after budget breach: err = %v, want the same sticky error", second)
	}
	if sink.Len() != 0 {
		t.Fatalf("partial buffer flushed: %d bytes reached the sink", sink.Len())
	}
}

// Marshal 10^7×int64 — real data volume within the encode default
// budgets is not choked. Element count is 10^7−2: the root encodeBody
// call also counts as an output node, and the resolved MaxNodes
// default is exactly 10^7 (a 10^7 element count would sit past the
// resolved default by one node).
func TestEncodeMarshalLargeOK(t *testing.T) {
	vals := make([]int64, 10000000-2)
	for i := range vals {
		vals[i] = int64(i)
	}
	if _, err := gbon.Marshal(vals); err != nil {
		t.Fatalf("Marshal(10^7 int64): %v", err)
	}
}

// Negative validation — negative depth/nodes fields are
// ErrUnsupported before anything is stored; a negative MaxBytes is the
// encode-side no-cap switch (accepted; its behavior is pinned by
// TestEncoderMaxBytesDisable); SetLimits after a failed Encoder is a
// sticky no-op.
func TestEncoderSetLimitsValidation(t *testing.T) {
	e := gbon.NewEncoder(&bytes.Buffer{})
	if err := e.SetLimits(gbon.Limits{MaxDepth: -3}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("SetLimits(MaxDepth=-3): err = %v, want ErrUnsupported", err)
	}
	if err := e.SetLimits(gbon.Limits{MaxNodes: -1}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("SetLimits(MaxNodes=-1): err = %v, want ErrUnsupported", err)
	}
	// not stored: the encoder still works under its previous limits
	if err := e.Encode(7); err != nil {
		t.Fatalf("Encode after rejected SetLimits: %v", err)
	}
	if err := e.SetLimits(gbon.Limits{MaxBytes: 4}); err != nil {
		t.Fatalf("pre-breach SetLimits: %v", err)
	}
	if err := e.Encode("break the 4-byte budget"); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Encode over budget: err = %v, want ErrBudget", err)
	}
	if err := e.SetLimits(gbon.Limits{MaxBytes: 1000000}); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("SetLimits after breach: err = %v, want sticky ErrBudget", err)
	}
}

// Encoder MaxBytes disable (negative MaxBytes = no byte cap, an explicit
// trust decision): a value past the byte cap encodes with the cap
// removed, while the depth and node budgets stay live. The cap crossing
// runs at the 10^9 default in regular builds; race builds run the
// configured-threshold equivalent (one 16 MB value, rejected by an
// explicit 8 MB cap, encoded with the cap off) — the 1.1 GB fixture
// plus its stream buffers OOM-kills the race runtime mid-suite.
func TestEncoderMaxBytesDisable(t *testing.T) {
	if err := gbon.NewEncoder(&bytes.Buffer{}).SetLimits(gbon.Limits{MaxBytes: -1}); err != nil {
		t.Fatalf("SetLimits(MaxBytes=-1): err = %v, want accepted as the no-cap switch", err)
	}

	if raceEnabled {
		big := strings.Repeat("x", 16<<20)
		capped := gbon.NewEncoder(&bytes.Buffer{})
		if err := capped.SetLimits(gbon.Limits{MaxBytes: 8 << 20}); err != nil {
			t.Fatal(err)
		}
		if err := capped.Encode(big); !errors.Is(err, gbon.ErrBudget) {
			t.Fatalf("Encode(16 MB, MaxBytes=8 MB): err = %v, want ErrBudget", err)
		}
		e := gbon.NewEncoder(io.Discard)
		if err := e.SetLimits(gbon.Limits{MaxBytes: -1}); err != nil {
			t.Fatal(err)
		}
		if err := e.Encode(big); err != nil {
			t.Fatalf("Encode(16 MB string, bytes disabled): %v", err)
		}
	} else {
		big := strings.Repeat("x", 1_100_000_000) // > 10^9 output bytes, one node
		e := gbon.NewEncoder(io.Discard)
		if err := e.SetLimits(gbon.Limits{MaxBytes: -1}); err != nil {
			t.Fatalf("SetLimits(MaxBytes=-1): %v", err)
		}
		if err := e.Encode(big); err != nil {
			t.Fatalf("Encode(1.1 GB string, bytes disabled): %v", err)
		}
		// the default cap still rejects the same value — the disable is
		// the only door past it
		if _, err := gbon.Marshal(big); !errors.Is(err, gbon.ErrBudget) {
			t.Fatalf("Marshal(1.1 GB string, default limits): err = %v, want ErrBudget", err)
		}
	}

	// depth stays live under the disabled byte cap
	ed := gbon.NewEncoder(&bytes.Buffer{})
	if err := ed.SetLimits(gbon.Limits{MaxBytes: -1, MaxDepth: 4}); err != nil {
		t.Fatalf("SetLimits(disable, MaxDepth=4): %v", err)
	}
	var deep any
	for range 8 {
		deep = &struct{ Next any }{deep} // fresh node each level — a real chain
	}
	if err := ed.Encode(deep); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Encode(deep chain, MaxDepth=4 under disabled bytes): err = %v, want ErrBudget", err)
	}

	// nodes stay live under the disabled byte cap
	en := gbon.NewEncoder(&bytes.Buffer{})
	if err := en.SetLimits(gbon.Limits{MaxBytes: -1, MaxNodes: 3}); err != nil {
		t.Fatalf("SetLimits(disable, MaxNodes=3): %v", err)
	}
	if err := en.Encode([]int64{1, 2, 3, 4}); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Encode(4 elements, MaxNodes=3 under disabled bytes): err = %v, want ErrBudget", err)
	}
}

// Map tie-break sub-marshal errors are not swallowed: a pair value that
// cannot marshal surfaces wrapped with its pair path, before the map
// header is written. The sort itself stays deterministic (a failed
// sub-marshal falls back to nil bytes inside the comparator).
func TestEncodeMapTieBreakErrorPath(t *testing.T) {
	type tbPt struct{ X int64 }
	p1, p2 := &tbPt{7}, &tbPt{7} // structurally equal pointer keys
	m := map[*tbPt]any{p1: int64(1), p2: func() {}}
	_, err := gbon.Marshal(m)
	if !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("Marshal(map with an unserializable pair value): err = %v, want ErrUnsupported", err)
	}
	var tb *gbon.Error
	if !errors.As(err, &tb) || tb.Class() != "unsupported_kind" {
		t.Fatalf("err = %v, want the pair-value marshal fault surfaced", err)
	}
}

// The tie-break sub-marshal mirrors the parent's coder scope: values
// behind a registered custom coder marshal inside the tie-break instead
// of failing it. tbOpaque is only marshalable through its coder (the
// derived walk rejects the unexported field), so a scoped failure here
// would surface as a tie-break error and fail the encode.
type tbOpaque struct{ n int64 }

type tbOpaqueCoder struct{}

func (tbOpaqueCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(tbOpaque).n)
}

func (tbOpaqueCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var n int64
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.FieldByName("n").SetInt(n)
	return nil
}

func TestEncodeMapTieBreakCoderScope(t *testing.T) {
	type tbPt struct{ X int64 }
	p1, p2 := &tbPt{7}, &tbPt{7}
	m := map[*tbPt]any{p1: tbOpaque{n: 1}, p2: tbOpaque{n: 2}}
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	if err := e.RegisterCoder(tbOpaque{}, tbOpaqueCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	if err := e.Encode(m); err != nil {
		t.Fatalf("Encode(map with custom-coder pair values): %v", err)
	}
}

// Encode-side mirror: MaxMapPairs/MaxSliceLen do not apply on encode —
// a live value is not a consumable input resource.
func TestEncodeNonApplicableFields(t *testing.T) {
	e := gbon.NewEncoder(&bytes.Buffer{})
	if err := e.SetLimits(gbon.Limits{MaxSliceLen: 4, MaxMapPairs: 2}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	if err := e.Encode([]int{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatalf("Encode(8 ints, MaxSliceLen=4): %v", err)
	}
	if err := e.Encode(map[string]int{"a": 1, "b": 2, "c": 3}); err != nil {
		t.Fatalf("Encode(3 pairs, MaxMapPairs=2): %v", err)
	}
}

// es-discrimination: the charge window between es=1 and
// es=16 must be exercised — a mid-size L where int8 fits the default
// MaxBytes but complex128 does not. A broken es multiplier (always 1)
// makes both legs succeed and must fail this test.
func TestBudgetChargeElemsizeDiscrimination(t *testing.T) {
	const L = 40_000_000 // int8: 4×10^7 ≤ 10^8 OK; complex128: 6.4×10^8 > 10^8
	cases := []struct {
		name     string
		elemDesc *cDesc
		example  any
		wantErr  bool
	}{
		{"int8 es=1 fits", descInt8, []int8(nil), false},
		{"complex128 es=16 exceeds", descComplex128, []complex128(nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := craftedSliceStream(tc.elemDesc, L)
			d := gbon.NewDecoder(bytes.NewReader(data))
			if err := d.Register(tc.example); err != nil {
				t.Fatalf("Register: %v", err)
			}
			out := reflect.New(reflect.TypeOf(tc.example)).Interface()
			err := d.Decode(out)
			if tc.wantErr && !errors.Is(err, gbon.ErrBudget) {
				t.Fatalf("want ErrBudget, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want OK, got %v", err)
			}
		})
	}
}

// NAMED-hops budget fixtures: legal wrapper chains over ARRAY
// elements. bhN* chain over int64 (the prim batch path,
// decodePrimElems/elemBudgetHops), bhS* over a one-field struct (the
// framed path, decodeElemFrame).
type bhN1 int64

type bhN2 bhN1
type bhN3 bhN2
type bhN4 bhN3
type bhN5 bhN4
type bhN6 bhN5

type bhBox struct{ F int64 }

type bhS1 bhBox
type bhS2 bhS1
type bhS3 bhS2
type bhS4 bhS3
type bhS5 bhS4
type bhS6 bhS5

// bhPrimStream builds SLICE(NAMED^depth → INT64) with one dense
// element; bhStructStream the SLICE(NAMED^depth → struct{F}) twin with
// E dense elements.
func bhPrimStream(depth int) []byte {
	names := []string{qn(bhN1(0)), qn(bhN2(0)), qn(bhN3(0)), qn(bhN4(0)), qn(bhN5(0)), qn(bhN6(0))}
	d := dInt64
	for i := range depth {
		d = dNamed(names[i], d)
	}
	c := newCraft()
	c.descPos(dSlice(d))
	r := c.arrayRec(1, 1)
	c.intTok(42)
	c.view0(r)
	return c.buf
}

func bhStructStream(depth, E int) []byte {
	names := []string{qn(bhS1{}), qn(bhS2{}), qn(bhS3{}), qn(bhS4{}), qn(bhS5{}), qn(bhS6{})}
	d := dStructT(qn(bhBox{}), cField{"F", dInt64})
	for i := range depth {
		d = dNamed(names[i], d)
	}
	c := newCraft()
	c.descPos(dSlice(d))
	r := c.arrayRec(uint64(E), uint64(E))
	for i := range E {
		c.structTok()
		c.intTok(int64(i + 1))
	}
	c.view0(r)
	return c.buf
}

// TestBudgetNamedHopsThresholds pins the exact MaxNodes/MaxDepth
// thresholds of a legal NAMED^6 wrapper chain over one ARRAY element on
// both element paths. The cycle guard (derefNamed) adds no charges: its
// walk pass is the pre-fix hop loop verbatim, so the thresholds are the
// pre-fix values by construction. Crossing a threshold by one lands on
// ErrBudget; the ok side decodes the element with the wrapper value
// intact (legal chains keep decoding as before the guard).
func TestBudgetNamedHopsThresholds(t *testing.T) {
	const N = 6
	big := gbon.Limits{MaxDepth: 1 << 30, MaxNodes: 1 << 30,
		MaxBytes: 1 << 62, MaxMapPairs: 1 << 30, MaxSliceLen: 1 << 62}
	prim := func(maxDepth, maxNodes int) ([]bhN6, error) {
		dec := gbon.NewDecoder(bytes.NewReader(bhPrimStream(N)))
		lim := big
		lim.MaxDepth, lim.MaxNodes = maxDepth, maxNodes
		dec.SetLimits(lim)
		var out []bhN6
		err := dec.Decode(&out)
		return out, err
	}
	struc := func(maxDepth, maxNodes int) ([]bhS6, error) {
		dec := gbon.NewDecoder(bytes.NewReader(bhStructStream(N, 1)))
		lim := big
		lim.MaxDepth, lim.MaxNodes = maxDepth, maxNodes
		dec.SetLimits(lim)
		var out []bhS6
		err := dec.Decode(&out)
		return out, err
	}
	strucE3 := func(maxDepth, maxNodes int) ([]bhS6, error) {
		dec := gbon.NewDecoder(bytes.NewReader(bhStructStream(N, 3)))
		lim := big
		lim.MaxDepth, lim.MaxNodes = maxDepth, maxNodes
		dec.SetLimits(lim)
		var out []bhS6
		err := dec.Decode(&out)
		return out, err
	}
	// prim path: one slice frame + hops+1 element nodes; depth window
	// d.depth+1+hops — both cross at N+2 = 8.
	if out, err := prim(1<<30, N+2); err != nil || len(out) != 1 || out[0] != 42 {
		t.Fatalf("prim ok-side (MaxNodes=%d): out=%v err=%v", N+2, out, err)
	}
	if out, err := prim(N+2, 1<<30); err != nil || len(out) != 1 || out[0] != 42 {
		t.Fatalf("prim ok-side (MaxDepth=%d): out=%v err=%v", N+2, out, err)
	}
	if _, err := prim(1<<30, N+1); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("prim MaxNodes=%d: err = %v, want ErrBudget", N+1, err)
	}
	if _, err := prim(N+1, 1<<30); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("prim MaxDepth=%d: err = %v, want ErrBudget", N+1, err)
	}
	// framed path at E=1: both thresholds sit in the wire descriptor
	// walk — nodes 10, depth 9. The value side stays below on both
	// axes (9 nodes, depth ≤3): decodeElemFrame charges nodes only and
	// has no depth window of its own.
	if out, err := struc(1<<30, 10); err != nil || len(out) != 1 || out[0].F != 1 {
		t.Fatalf("struct ok-side (MaxNodes=10): out=%v err=%v", out, err)
	}
	if out, err := struc(9, 1<<30); err != nil || len(out) != 1 || out[0].F != 1 {
		t.Fatalf("struct ok-side (MaxDepth=9): out=%v err=%v", out, err)
	}
	if _, err := struc(1<<30, 9); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("struct MaxNodes=9: err = %v, want ErrBudget", err)
	}
	if _, err := struc(8, 1<<30); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("struct MaxDepth=8: err = %v, want ErrBudget", err)
	}
	// framed path at E=3 — layer separation: the node threshold moves
	// to the value side (slice + 3·(hops + struct + field) = 25 nodes),
	// the depth threshold stays on the descriptor side (9, independent
	// of E — the elements share one depth window).
	if out, err := strucE3(1<<30, 25); err != nil || len(out) != 3 || out[2].F != 3 {
		t.Fatalf("struct E=3 ok-side (MaxNodes=25): out=%v err=%v", out, err)
	}
	if out, err := strucE3(9, 1<<30); err != nil || len(out) != 3 || out[2].F != 3 {
		t.Fatalf("struct E=3 ok-side (MaxDepth=9): out=%v err=%v", out, err)
	}
	if _, err := strucE3(1<<30, 24); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("struct E=3 MaxNodes=24: err = %v, want ErrBudget", err)
	}
	if _, err := strucE3(8, 1<<30); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("struct E=3 MaxDepth=8: err = %v, want ErrBudget", err)
	}
}
