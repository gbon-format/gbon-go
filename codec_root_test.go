package gbon_test

// Root-path suite: R1 root unwrap (one deref at the
// top entry, stateless and streaming) and R2 in-place materialization of
// pointer-target record roots with snapshot-restore atomicity.

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

type rtS1 struct{ A *int }

type rtFlat struct {
	A [16]byte
	B int64
	C string
	D float64
}

type rtComment struct {
	ID      int
	Body    string
	Parent  *rtComment
	Replies []*rtComment
}

type rtRing struct {
	Val        int
	Next, Prev *rtRing
}

type rtNamedInt int

type rtZero struct{}

type rtZS struct {
	P *rtZero
	N int
}

type rtPanicS struct{ S []int64 }

func rtIntPtr(t *testing.T, n int) *int {
	t.Helper()
	return &n
}

// wireNameOf mirrors the codec's type naming: import path plus the
// type-literal string (codec_desc.go nameOf form for named types).
func wireNameOf(t reflect.Type) string {
	return t.PkgPath() + "." + t.String()
}

// Stateless deref — Marshal(&x T) -> Unmarshal(&out T) round-trips.
func TestRootUnwrapStateless(t *testing.T) {
	cases := []any{
		rtS1{A: rtIntPtr(t, 42)},
		rtFlat{A: [16]byte{1, 2, 3}, B: -99, C: "hello", D: 2.5},
		rtNamedInt(7),
		rtZS{P: &rtZero{}, N: 5},
		rtComment{ID: 1, Body: "solo"},
	}
	for _, x := range cases {
		typ := reflect.TypeOf(x)
		ptr := reflect.New(typ)
		ptr.Elem().Set(reflect.ValueOf(x))
		data, err := gbon.Marshal(ptr.Interface())
		if err != nil {
			t.Fatalf("%s: Marshal: %v", typ, err)
		}
		out := reflect.New(typ)
		if err := gbon.Unmarshal(data, out.Interface()); err != nil {
			t.Fatalf("%s: Unmarshal: %v", typ, err)
		}
		if got := out.Elem().Interface(); !reflect.DeepEqual(got, x) {
			t.Fatalf("%s: RT mismatch: %s != %s", typ, safeDescValue(got), safeDescValue(x))
		}
	}
}

// Streaming deref — the first stream value carries a `*T` root
// descriptor and decodes into a T target.
func TestRootUnwrapStreaming(t *testing.T) {
	x := rtFlat{A: [16]byte{9}, B: 1 << 40, C: "stream", D: -0.5}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(&x); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	var out rtFlat
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out != x {
		t.Fatalf("RT mismatch: %s != %s", safeDescValue(out), safeDescValue(x))
	}
}

// In-place identity (comment form): replies reference the caller's cell as Parent.
func TestRootInPlaceIdentityComment(t *testing.T) {
	root := &rtComment{ID: 1, Body: "root"}
	r2 := &rtComment{ID: 2, Body: "first", Parent: root}
	r3 := &rtComment{ID: 3, Body: "second", Parent: root}
	root.Replies = []*rtComment{r2, r3}
	data, err := gbon.Marshal(root)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out rtComment
	if err := gbon.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.ID != 1 || len(out.Replies) != 2 {
		t.Fatalf("root shape lost: %s", safeDescValue(out))
	}
	for i, want := range []string{"first", "second"} {
		if r := out.Replies[i]; r.Body != want || r.Parent != &out {
			t.Fatalf("reply[%d]: Body=%q Parent==&out: %v", i, r.Body, r.Parent == &out)
		}
	}
}

// In-place identity (ring form): n.Next.Prev == n && n.Prev.Next == n against the cell.
func TestRootInPlaceIdentityRing(t *testing.T) {
	a := &rtRing{Val: 1}
	b := &rtRing{Val: 2}
	c := &rtRing{Val: 3}
	a.Next, b.Next, c.Next = b, c, a
	a.Prev, b.Prev, c.Prev = c, a, b
	data, err := gbon.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out rtRing
	if err := gbon.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Next.Prev != &out || out.Prev.Next != &out {
		t.Fatalf("ring identity lost: Next.Prev==cell %v, Prev.Next==cell %v",
			out.Next.Prev == &out, out.Prev.Next == &out)
	}
	n := &out
	for i := range 9 {
		if n.Val != i%3+1 {
			t.Fatalf("ring walk broken at %d: Val=%d", i, n.Val)
		}
		n = n.Next
	}
}

// Snapshot-restore atomicity — a truncated or crafted stream leaves the
// pre-call value of the cell byte-identical (deep-equal double check).
func TestRootAtomicitySnapshotRestore(t *testing.T) {
	pre := rtFlat{A: [16]byte{7, 7, 7}, B: -12345, C: "pre-call", D: 3.25}
	build := func(v rtFlat) []byte {
		data, err := gbon.Marshal(&v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return data
	}
	big := rtFlat{A: [16]byte{5}, B: 1 << 50, C: "0123456789ABCDEFG", D: -1.75}
	trunc := build(big)
	trunc = trunc[:len(trunc)-1] // cut mid-value: fields already wrote
	inputs := map[string][]byte{
		"truncated-tail": trunc,
		"garbage-tail":   append(append([]byte{}, trunc[:len(trunc)-1]...), 0xFF),
	}
	for name, data := range inputs {
		out := pre
		err := gbon.Unmarshal(data, &out)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if out != pre || !reflect.DeepEqual(out, pre) {
			t.Fatalf("%s: cell not restored: %s != %s", name, safeDescValue(out), safeDescValue(pre))
		}
	}
	// streaming variant: same guarantee through Decoder.Decode
	out2 := pre
	dec := gbon.NewDecoder(bytes.NewReader(trunc))
	if err := dec.Decode(&out2); err == nil {
		t.Fatal("streaming truncated: no error")
	}
	if out2 != pre {
		t.Fatalf("streaming cell not restored: %s != %s", safeDescValue(out2), safeDescValue(pre))
	}
}

// Recover-path atomicity — a crafted allocation bomb behind a
// user-raised limit panics into the recovered ErrBudget path; the cell
// keeps its pre-call value (assertion-guard: error class + restoration).
func TestRootAtomicityPanicBudget(t *testing.T) {
	pre := rtPanicS{S: []int64{1, 2, 3}}
	c := newCraft()
	sd := dSlice(dInt64)
	c.descPos(dPtr(dStructT(wireNameOf(reflect.TypeFor[rtPanicS]()), cField{"S", sd})))
	c.ptrRec() // root pointer record: registers before children
	c.structTok()
	arr := c.arrayRec(1<<59, 0) // admitted length, unsatisfiable allocation
	c.view0(arr)
	lim := gbon.Limits{
		MaxDepth: math.MaxInt32, MaxNodes: math.MaxInt32,
		MaxBytes: math.MaxInt64, MaxMapPairs: math.MaxInt32,
		MaxSliceLen: math.MaxInt64,
	}
	out := pre
	err := func() (err error) {
		dec := gbon.NewDecoder(bytes.NewReader(c.buf))
		dec.SetLimits(lim)
		return dec.Decode(&out)
	}()
	if !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Decode: err = %v, want ErrBudget", err)
	}
	if len(out.S) != len(pre.S) || out.S[0] != pre.S[0] || out.S[2] != pre.S[2] {
		t.Fatalf("cell not restored after panic-budget: %s != %s", safeDescValue(out), safeDescValue(pre))
	}
}

// Nil root pointer reject in a T target is a loud format error; a
// derivable chain root decodes additively as the typed nil of the chain,
// named roots stay rejected with the registry hint.
func TestRootNilPointerReject(t *testing.T) {
	nb, err := gbon.Marshal((*rtS1)(nil))
	if err != nil {
		t.Fatalf("Marshal(nil *rtS1): %v", err)
	}
	var out rtS1
	if err := gbon.Unmarshal(nb, &out); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("Unmarshal(nil root): err = %v, want ErrFormat", err)
	}
	var ap *any
	ab, err := gbon.Marshal(ap)
	if err != nil {
		t.Fatalf("Marshal(nil *any): %v", err)
	}
	var av any = 7
	if err := gbon.Unmarshal(ab, &av); err != nil {
		t.Fatalf("Unmarshal(nil *any root): %v", err)
	}
	if av == nil {
		t.Fatalf("nil *any root: want typed nil, got nil interface")
	}
	got, ok := av.(*any)
	if !ok || got != nil {
		t.Fatalf("nil *any root: want (*any)(nil), got %#v", av)
	}
	if !bytes.Equal(mustMarshal(t, av), ab) {
		t.Fatalf("nil *any root: re-encode drift")
	}
	type localAnyPtr *any
	var lp localAnyPtr
	lb, err := gbon.Marshal(lp)
	if err != nil {
		t.Fatalf("Marshal(nil named chain): %v", err)
	}
	var av2 any = 7
	err = gbon.Unmarshal(lb, &av2)
	if !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("named nil root: err = %v, want ErrFormat", err)
	}
	var ge *gbon.Error
	if !errors.As(err, &ge) || ge.Class() != "unknown_name" {
		t.Fatalf("named nil root: want unknown_name, got %v", err)
	}
	if !strings.Contains(err.Error(), "interface concrete type not registered: use Decoder.Register") {
		t.Fatalf("named nil root: verbatim hint expected, got %v", err)
	}
	if av2 != any(any(7)) {
		t.Fatalf("target modified on reject: %s", safeDescValue(av2))
	}
}

// decodeSub no-deref contract keeps its no-deref contract — a coder callback
// Decode into a T value rejects a `*T` sub-stream (in-process seam).
type rtSubWrap struct {
	Inner rtS1
}

type rtSubCoder struct{}

func (rtSubCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	x := v.Interface().(rtSubWrap)
	return e.Encode(&x.Inner)
}

func (rtSubCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var n rtS1
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.FieldByName("Inner").Set(reflect.ValueOf(n))
	return nil
}

func TestDecodeSubNoUnwrap(t *testing.T) {
	type subBox struct {
		Tag string
		W   rtSubWrap
	}
	box := subBox{Tag: "t", W: rtSubWrap{Inner: rtS1{A: rtIntPtr(t, 3)}}}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(rtSubWrap{}, rtSubCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	if err := enc.Encode(box); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err := dec.RegisterCoder(rtSubWrap{}, rtSubCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	var out subBox
	if err := dec.Decode(&out); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("Decode: err = %v, want ErrFormat (decodeSub must not deref)", err)
	}
}

// Cross-value root ref to the root — the R2 cell registered in the
// stream-reader's intern space is a legal value target.
func TestCrossValueRootRef(t *testing.T) {
	type cvW struct{ F *rtS1 }
	a := rtS1{A: rtIntPtr(t, 11)}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(&a); err != nil {
		t.Fatalf("Encode#1: %v", err)
	}
	if err := enc.Encode(cvW{F: &a}); err != nil {
		t.Fatalf("Encode#2: %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	var out rtS1
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("Decode#1: %v", err)
	}
	var w cvW
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("Decode#2: %v", err)
	}
	if w.F != &out {
		t.Fatalf("cross-value root ref resolved to %p, want the caller cell %p", w.F, &out)
	}
	if w.F == nil || w.F.A == nil || *w.F.A != 11 {
		t.Fatalf("cross-value ref content lost: %s", safeDescValue(w.F))
	}
}

// Edges: NAMED-wrapped root descriptor, `**T` double-deref reject,
// streaming sticky-error after a failed in-place decode.
func TestRootEdges(t *testing.T) {
	t.Run("named-root-unwrap", func(t *testing.T) {
		c := newCraft()
		inner := dStructT(wireNameOf(reflect.TypeFor[rtS1]()), cField{"A", dPtr(dIntT)})
		c.descPos(dNamed("gbon_test.rtNamedRoot", dPtr(inner)))
		c.ptrRec()
		c.structTok()
		c.ptrRec()
		c.intTok(7)
		var out rtS1
		if err := gbon.Unmarshal(c.buf, &out); err != nil {
			t.Fatalf("Unmarshal(Named(*rtS1)): %v", err)
		}
		if out.A == nil || *out.A != 7 {
			t.Fatalf("NAMED root RT: %s", safeDescValue(out))
		}
	})
	t.Run("double-deref-reject", func(t *testing.T) {
		x := rtS1{A: rtIntPtr(t, 5)}
		pp := &x
		data, err := gbon.Marshal(&pp)
		if err != nil {
			t.Fatalf("Marshal(&pp): %v", err)
		}
		var out rtS1
		if err := gbon.Unmarshal(data, &out); !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("Unmarshal(**T -> T): err = %v, want ErrFormat", err)
		}
	})
	t.Run("sticky-after-error", func(t *testing.T) {
		pre := rtFlat{B: 8, C: "pre"}
		big := rtFlat{A: [16]byte{3}, B: 4, C: "0123456789ABCDEF", D: 1}
		data, err := gbon.Marshal(&big)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		dec := gbon.NewDecoder(bytes.NewReader(data[:len(data)-1]))
		out := pre
		if err := dec.Decode(&out); err == nil {
			t.Fatal("first Decode: no error")
		}
		if out != pre {
			t.Fatalf("cell not restored: %s", safeDescValue(out))
		}
		var again rtFlat
		if err := dec.Decode(&again); err == nil {
			t.Fatal("second Decode after error: no error (sticky contract)")
		}
	})
}

// The crafted NAMED self-loop corpus vector: a descriptor that interns
// before its body, a REF onto id 0 closes the loop, plus a REF-series
// tail — terminates in a format error on every entry point and under any
// Limits (never a hang). Hang protection is the suite timeout.
func TestRootNamedCycleCorpusVector(t *testing.T) {
	data := mustHex(t, "67626f6e0000"+ // header
		"d46c33"+ // DESC kind 4 (NAMED), name u8 len 51
		"6769746875622e636f6d2f67626f6e2d666f726d61742f67626f6e2d676f5f746573742e67626f6e5f746573742e6d79447572"+ // …myDur
		"c0c0c0c0c0"+ // REF-series tail onto id 0
		"d865696e743634082c54") // DESC INT "int64" w8 + value tokens
	t.Run("stateless-zero-limits", func(t *testing.T) {
		var out any
		if err := gbon.Unmarshal(data, &out); !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("Unmarshal(b214f): err = %v, want ErrFormat", err)
		}
	})
	t.Run("streaming", func(t *testing.T) {
		dec := gbon.NewDecoder(bytes.NewReader(data))
		var out any
		if err := dec.Decode(&out); !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("Decode(b214f): err = %v, want ErrFormat", err)
		}
	})
	t.Run("max-limits", func(t *testing.T) {
		dec := gbon.NewDecoder(bytes.NewReader(data))
		dec.SetLimits(gbon.Limits{MaxDepth: math.MaxInt32, MaxNodes: math.MaxInt32,
			MaxBytes: math.MaxInt64, MaxMapPairs: math.MaxInt32, MaxSliceLen: math.MaxInt64})
		var out any
		if err := dec.Decode(&out); !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("Decode(b214f, max limits): err = %v, want ErrFormat", err)
		}
	})
}

// PBT (light, existing topo generator): a random shared graph behind a
// pointer root round-trips with identity to the caller's cell or fails
// with a distinct error — never a mixed state (identity-vs-error property).
type rtPropBox struct {
	G    any
	Self *rtPropBox
}

func TestRootPropIdentityOrError(t *testing.T) {
	for i := range 300 {
		g := &topoGen{r: rand.New(rand.NewSource(int64(i))), maxDepth: 4, maxFan: 3,
			reusePct: 30, cyclePct: 20}
		w := &rtPropBox{G: g.value(0)}
		w.Self = w
		data, err := gbon.Marshal(w)
		if err != nil {
			continue // generator shapes may exceed encode budgets; property is decode-side
		}
		pre := rtPropBox{G: "pre", Self: nil}
		out := pre
		if err := gbon.Unmarshal(data, &out); err != nil {
			if !reflect.DeepEqual(out, pre) {
				t.Fatalf("iter %d: error %v left mixed state", i, err)
			}
			continue
		}
		if out.Self != &out {
			t.Fatalf("iter %d: root backref resolved to %p, want cell %p", i, out.Self, &out)
		}
		_, dBefore := topoPaths(w.G)
		_, dAfter := topoPaths(out.G)
		if len(dBefore) != len(dAfter) {
			t.Fatalf("iter %d: sharing lost: %d dups before, %d after", i, len(dBefore), len(dAfter))
		}
	}
}

// TestSafeDescCyclicRoot: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicRoot(t *testing.T) {
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
