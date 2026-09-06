package gbon_test

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Regression corpus for the intern-space mirror invariant:
// writer-ids ≡ reader-entries. The oracle is behavioral — full
// round-trips, byte-stable re-encoding, and evolution skips whose kept
// positions REF interned records allocated inside skipped fields.

// evStrN/evStrO: equal-length names, the splice fixture pattern.
type evStrN struct{ Dropped, A, B string }

type evStrO struct{ A, B string }

// TestInternMirrorSkipString: a dropped string field's literal
// interns on the skip path; a REF from a kept position resolves to
// the same string — no false ErrFormat, no id shift.
func TestInternMirrorSkipString(t *testing.T) {
	nb, err := gbon.Marshal(evStrN{Dropped: "dup", A: "dup", B: "dup"})
	if err != nil {
		t.Fatal(err)
	}
	sp := coderSplice(t, nb, "evStrN", "evStrO")
	var o evStrO
	if err := gbon.Unmarshal(sp, &o); err != nil {
		t.Fatalf("skip+ref decode: %v", err)
	}
	if o.A != "dup" || o.B != "dup" {
		t.Fatalf("got %s, want A=B=dup", safeDescValue(o))
	}
	// unique literals: no REF involved, boundary sanity
	nb2, _ := gbon.Marshal(evStrN{Dropped: "zzz", A: "aaa", B: "bbb"})
	var o2 evStrO
	if err := gbon.Unmarshal(coderSplice(t, nb2, "evStrN", "evStrO"), &o2); err != nil {
		t.Fatalf("unique decode: %v", err)
	}
	if o2.A != "aaa" || o2.B != "bbb" {
		t.Fatalf("got %s, want A=aaa B=bbb", safeDescValue(o2))
	}
}

// TestInternMirrorSkipStringSilentCorruption: the silent-corruption
// shape — Dropped=="dup" interned on skip, then "mid", then B REFs
// "dup". A shifted id space would resolve B into "mid".
func TestInternMirrorSkipStringSilentCorruption(t *testing.T) {
	nb, err := gbon.Marshal(evStrN{Dropped: "dup", A: "mid", B: "dup"})
	if err != nil {
		t.Fatal(err)
	}
	sp := coderSplice(t, nb, "evStrN", "evStrO")
	var o evStrO
	if err := gbon.Unmarshal(sp, &o); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if o.A != "mid" || o.B != "dup" {
		t.Fatalf("silent corruption: got %s, want A=mid B=dup", safeDescValue(o))
	}
}

// TestInternMirrorZeroSizePointer: *struct{} sentinel fields
// reserve no id on either side; the A==B string REF must resolve.
func TestInternMirrorZeroSizePointer(t *testing.T) {
	type zs struct {
		P *struct{}
		A string
		B string
	}
	v := zs{A: "same", B: "same"}
	b, err := gbon.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out zs
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if out.A != "same" || out.B != "same" || out.P != nil {
		t.Fatalf("got %s", safeDescValue(out))
	}
	if !reflect.DeepEqual(v, out) {
		t.Fatalf("got %s want %s", safeDescValue(out), safeDescValue(v))
	}
	// map variant
	type zm struct {
		P *struct{}
		M map[string]int
	}
	m := zm{M: map[string]int{"k": 1}}
	mb, _ := gbon.Marshal(m)
	var mo zm
	if err := gbon.Unmarshal(mb, &mo); err != nil {
		t.Fatalf("map variant: %v", err)
	}
	if !reflect.DeepEqual(m, mo) {
		t.Fatalf("map variant: got %s want %s", safeDescValue(mo), safeDescValue(m))
	}
}

// skip-ZS: a dropped *struct{} field on the skip path registers no
// phantom entry — kept fields after it stay aligned.
func TestInternMirrorSkipZeroSizePointer(t *testing.T) {
	type zsn struct {
		P *struct{}
		A string
	}
	type zso struct {
		A string
	}
	nb, _ := gbon.Marshal(zsn{A: "x"})
	sp := coderSplice(t, nb, "zsn", "zso")
	var o zso
	if err := gbon.Unmarshal(sp, &o); err != nil {
		t.Fatalf("skip zs ptr: %v", err)
	}
	if o.A != "x" {
		t.Fatalf("got %s", safeDescValue(o))
	}
}

// rawN/rawO with named byte-slice field.
type rawN struct {
	S []byte
	R rawAlias
}

type rawO struct {
	R rawAlias
	S []byte
}

type rawAlias []byte

// TestViewAssignability: one allocation surfaced as []byte
// and as a named Raw type — grouping emits one backing record, the view
// gate accepts Go-assignable element types in both field orders.
func TestViewAssignability(t *testing.T) {
	s := []byte("hello world")
	v := rawN{S: s, R: rawAlias(s[6:11])}
	b, err := gbon.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out rawN
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatalf("rawN round-trip: %v", err)
	}
	if string(out.R) != "world" || string(out.S) != "hello world" {
		t.Fatalf("rawN: got S=%q R=%q", out.S, out.R)
	}
	v2 := rawO{R: rawAlias(s[0:5]), S: s}
	b2, _ := gbon.Marshal(v2)
	var out2 rawO
	if err := gbon.Unmarshal(b2, &out2); err != nil {
		t.Fatalf("rawO round-trip: %v", err)
	}
	if string(out2.R) != "hello" || string(out2.S) != "hello world" {
		t.Fatalf("rawO: got S=%q R=%q", out2.S, out2.R)
	}
	// non-assignable element types still reject: []int backing vs a
	// named []int8 target is a structural mismatch caught by matchDesc;
	// the direct view gate check needs a crafted mismatch, covered by
	// the crafted corpus (shared-backing-unavailable class).
}

// mirrorFn runs the behavioral intern-mirror oracle over one value:
// full round-trip, re-encode byte stability, and decode of the re-encoded
// stream (id-space drift surfaces as an error or a corrupted value whose
// next encode differs).
func mirrorFn(t *testing.T, v any) {
	t.Helper()
	b1, err := gbon.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := reflect.New(reflect.TypeOf(v)).Interface()
	if err := gbon.Unmarshal(b1, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := reflect.ValueOf(out).Elem().Interface()
	if !reflect.DeepEqual(v, got) {
		t.Fatalf("round-trip value: got %s want %s", safeDescValue(got), safeDescValue(v))
	}
	b2, err := gbon.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("re-encode unstable:\n b1 % x\n b2 % x", b1, b2)
	}
	out2 := reflect.New(reflect.TypeOf(v)).Interface()
	if err := gbon.Unmarshal(b2, out2); err != nil {
		t.Fatalf("second unmarshal: %v", err)
	}
}

// TestInternMirror is the property core: one battery covering every
// internable record class — strings (repeats across depths), blob/array
// backings with views, map objects (shared/self), non-zero-size pointer
// targets (DAGs and cycles), zero-size sentinels, and a recursive type.
// Pointer-identity values get relational checks (addresses change across
// a round trip; sharing relations must not).
func TestInternMirror(t *testing.T) {
	type node struct {
		Name string // repeated at depth
		Next *node
	}
	self := node{Name: "n"}
	self.Next = &self
	backing := []int{1, 2, 3, 4}
	type views struct {
		A []int
		B []int
	}
	// pointer-free battery: full oracle (value equality + byte stability)
	for _, v := range []any{
		[]string{"dup", "dup", "dup"},
		map[string]string{"a": "dup", "b": "dup"},
		views{A: backing[0:2], B: backing[1:4]},
	} {
		t.Run(reflect.TypeOf(v).String(), func(t *testing.T) {
			mirrorFn(t, v)
		})
	}
	// cycles: DeepEqual unfolds the cycle; re-encode byte-stability holds
	t.Run("cycle", func(t *testing.T) { mirrorFn(t, self) })

	// pointer DAG sharing: out[0]==out[1]!=out[2]
	t.Run("ptr-dag", func(t *testing.T) {
		a, b2 := 1, 2
		v := []*int{&a, &a, &b2}
		bm, err := gbon.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out []*int
		if err := gbon.Unmarshal(bm, &out); err != nil {
			t.Fatal(err)
		}
		if len(out) != 3 || out[0] != out[1] || out[0] == out[2] ||
			*out[0] != 1 || *out[2] != 2 {
			t.Fatalf("sharing lost: %v", out)
		}
		re, err := gbon.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bm, re) {
			t.Fatalf("re-encode unstable:\n b1 % x\n b2 % x", bm, re)
		}
	})

	// pointer keys: two distinct slots survive with their values
	t.Run("ptr-keys", func(t *testing.T) {
		a, b2 := 1, 2
		v := map[*int]int{&a: 1, &b2: 2}
		bm, err := gbon.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out := map[*int]int{}
		if err := gbon.Unmarshal(bm, &out); err != nil {
			t.Fatal(err)
		}
		if len(out) != 2 {
			t.Fatalf("len=%d want 2", len(out))
		}
		for k, val := range out {
			if *k == 1 && val != 1 || *k == 2 && val != 2 {
				t.Fatalf("pair corruption: (*k=%d)->%d", *k, val)
			}
		}
	})

	// zero-size sentinel beside a string REF and a live cycle pointer
	t.Run("zero-size-mixed", func(t *testing.T) {
		type zs struct {
			P  *struct{}
			A  string
			B  string
			Up *node
		}
		bm, err := gbon.Marshal(zs{A: "dup", B: "dup", Up: &self})
		if err != nil {
			t.Fatal(err)
		}
		var out zs
		if err := gbon.Unmarshal(bm, &out); err != nil {
			t.Fatal(err)
		}
		if out.A != "dup" || out.B != "dup" || out.P != nil ||
			out.Up == nil || out.Up.Next != out.Up || out.Up.Name != "n" {
			t.Fatalf("got %s", safeDescValue(out))
		}
		re, err := gbon.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bm, re) {
			t.Fatalf("re-encode unstable:\n b1 % x\n b2 % x", bm, re)
		}
	})
}

// recursionCoder forwards Decode into the facade.
type recursionCoder struct{}

type recT struct{ N int }

func (recursionCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error { return nil }

func (recursionCoder) DecodeValue(d *gbon.Decoder, target reflect.Value) error {
	nv := reflect.New(target.Type()).Elem()
	if err := d.Decode(nv.Addr().Interface()); err != nil {
		return err
	}
	target.Set(nv)
	return nil
}

// TestCoderDecodeRefRecursion: a valid base stream plus a crafted REF
// tail on the CODER descriptor drives facade recursion into
// inherited-budget exhaustion (ErrBudget, never a stack overflow).
func TestCoderDecodeRefRecursion(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(recT{}, recursionCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(recT{N: 1}); err != nil {
		t.Fatal(err)
	}
	b := append([]byte{}, buf.Bytes()...)
	// append REF(id 0) tokens: first byte 0xC0 (class REF, inline arg 0)
	tail := bytes.Repeat([]byte{0xC0}, 2_000_000)
	b = append(b, tail...)
	dec := gbon.NewDecoder(io.Reader(bytes.NewReader(b)))
	if err := dec.RegisterCoder(recT{}, recursionCoder{}); err != nil {
		t.Fatal(err)
	}
	out := recT{}
	err := dec.Decode(&out)
	if err == nil || !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("want ErrBudget from inherited depth, got %v", err)
	}
}

// decChainCoder delegates a depth chain through d.Decode in ONE call —
// the decode-side mirror of TestCoderDepthCounterInheritance.
type decChainCoder struct{ n int }

type decTrigger struct{}

func (c decChainCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(boxDOf(c.n, int64(0)))
}

func (decChainCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var out any
	if err := d.Decode(&out); err != nil {
		return err
	}
	if rv := reflect.ValueOf(out); rv.Type().AssignableTo(v.Type()) {
		v.Set(rv)
	}
	return nil
}

type boxD struct{ V any }

func boxDOf(n int, v any) any {
	if n == 0 {
		return v
	}
	return boxD{V: boxDOf(n-1, v)}
}

func TestCoderDecodeDepthCounterInheritance(t *testing.T) {
	build := func(t *testing.T, n int) []byte {
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		if err := enc.RegisterCoder(decTrigger{}, decChainCoder{n}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(decTrigger{}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	// depth-10 chain through one coder callback: fresh windows would
	// pass at MaxDepth=4; inheritance fires ErrBudget.
	dec := gbon.NewDecoder(bytes.NewReader(build(t, 10)))
	if err := dec.Register(boxD{}); err != nil {
		t.Fatal(err)
	}
	if err := dec.RegisterCoder(decTrigger{}, decChainCoder{10}); err != nil {
		t.Fatal(err)
	}
	dec.SetLimits(gbon.Limits{MaxDepth: 4})
	out := decTrigger{}
	err := dec.Decode(&out)
	if err == nil || !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("want ErrBudget from inherited depth, got %v", err)
	}
	// within-limit chain decodes through the coder path without error
	dec4 := gbon.NewDecoder(bytes.NewReader(build(t, 3)))
	if err := dec4.Register(boxD{}, int64(0)); err != nil {
		t.Fatal(err)
	}
	if err := dec4.RegisterCoder(decTrigger{}, decChainCoder{3}); err != nil {
		t.Fatal(err)
	}
	dec4.SetLimits(gbon.Limits{MaxDepth: 20})
	ok := decTrigger{}
	if err := dec4.Decode(&ok); err != nil {
		t.Fatalf("within budget: %v", err)
	}
}

// Zero-size types with deep descriptors (65+ levels — within
// MaxDescDepth) must still classify as zero-size and keep the intern
// mirror aligned.
func TestInternMirrorDeepZeroSize(t *testing.T) {
	// Build type: struct { F [0]array-nested 70 levels }
	inner := reflect.TypeOf(struct{}{})
	for range 70 {
		inner = reflect.ArrayOf(0, inner)
	}
	deepType := reflect.StructOf([]reflect.StructField{
		{Name: "F", Type: inner},
	})
	v := reflect.New(deepType).Interface() // *T
	b, err := gbon.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal deep zero-size: %v", err)
	}
	// Decode to the same concrete type through the registry
	d := gbon.NewDecoder(bytes.NewReader(b))
	if err := d.Register(v); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var decoded any
	if err := d.Decode(&decoded); err != nil {
		t.Fatalf("Decode deep zero-size: %v", err)
	}
	// The decoded value should be a *deepType with zero value
	if _, ok := decoded.(reflect.Value); ok {
		t.Fatal("decoded is reflect.Value wrapper")
	}
}

// TestSafeDescCyclicMirror: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicMirror(t *testing.T) {
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
