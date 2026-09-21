package gbon_test

// The crafted corpus behind the identity baseline and the structured-log
// probes: one public-API probe per error class, all twenty-four classes
// covered. The eighteen probes of the oracle corpus are reused as-is; the
// remaining six classes get dedicated probes here (duplicate key
// stream, a failing custom coder, a crafted allocation bomb under
// raised limits, a panicking custom coder attributed by the decode
// tripwire, and the two stable-mode guard rejects).

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// snippetClasses is the full class inventory the corpus must cover.
var snippetClasses = []string{
	"bad_magic", "truncated", "malformed_op", "malformed_arg",
	"overflow_value", "duplicate_key", "bad_ref", "bad_path", "bad_view",
	"type_mismatch", "evolution_ref_unmaterialized", "unknown_name",
	"budget_depth", "budget_nodes", "budget_bytes", "budget_alloc",
	"unsupported_kind", "register_conflict", "coder_error", "coder_recursion",
	"contract_mismatch", "io_read", "io_write",
	"internal_panic",
	"unstable_tie_break", "unstable_zero_float_key",
}

// snippetPathS/W carry the middle-field interior alias the zero-step
// probe corrupts.
type snippetPathS struct{ A, B, C int64 }

type snippetPathW struct {
	V *snippetPathS
	P *int64
}

// snippetNarrowMapW/B form the narrowing pair: the wide stream's field A
// drops out of the narrow target, its map record stays unmaterialized
// under the skip, and the kept field B's REF names it.
type snippetNarrowMapW struct {
	A map[bool]int64
	B map[bool]int64
}

type snippetNarrowB struct {
	B map[bool]int64
}

// snippetTieMap builds the guard's tie reject: two distinct pointers to
// congruent pointees under equal pair values.
func snippetTieMap() map[*int]int {
	return map[*int]int{new(int): 1, new(int): 1}
}

type snippetCase struct {
	name  string
	class string
	err   error
}

// snippetFailCoder fails every decode with a fixed marker error.
type snippetFailBox struct{ N int64 }

type snippetFailCoder struct{}

func (snippetFailCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(snippetFailBox).N)
}

func (snippetFailCoder) DecodeValue(*gbon.Decoder, reflect.Value) error {
	return errors.New("SNIPPETCODERFAIL")
}

// snippetPanicCoder panics inside the decode body; the decode tripwire
// attributes the coder's panic to the internal family.
type snippetPanicBox struct{ N int64 }

type snippetPanicCoder struct{}

func (snippetPanicCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(snippetPanicBox).N)
}

func (snippetPanicCoder) DecodeValue(*gbon.Decoder, reflect.Value) error {
	panic("SNIPPETALLOCBOOM")
}

// craftedAllocBomb builds a []int8 stream claiming a 2^60-element
// backing: under raised limits the charge passes and the allocator
// must refuse the allocation — the craftable budget_alloc probe.
func craftedAllocBomb() []byte {
	c := newCraft()
	c.descPos(dSlice(dInt8))
	r := c.arrayRec(1<<60, 0)
	c.view0(r)
	return c.buf
}

func snippetCoderStream(t *testing.T, box any, coder gbon.Coder) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(box, coder); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	if err := enc.Encode(box); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf.Bytes()
}

// stableErr adapts MarshalStable's two-value form to the corpus's err
// field.
func stableErr(v any) error {
	_, err := gbon.MarshalStable(v)
	return err
}

// snippetCorpus builds the twenty-one-class probe set. Every probe goes
// through the public API on a crafted input; the returned cases carry
// distinct classes, asserted against the inventory above.
func snippetCorpus(t *testing.T) []snippetCase {
	t.Helper()
	var out []snippetCase
	for _, oc := range oracleCorpus(t) {
		out = append(out, snippetCase{name: oc.name, class: oc.class, err: oc.err})
	}

	var dm map[string]int64
	out = append(out, snippetCase{name: "duplicate_key", class: "duplicate_key",
		err: gbon.Unmarshal(oracleDupStream("dupkey"), &dm)})

	var fb snippetFailBox
	fdec := gbon.NewDecoder(bytes.NewReader(snippetCoderStream(t, snippetFailBox{N: 7}, snippetFailCoder{})))
	if err := fdec.RegisterCoder(snippetFailBox{}, snippetFailCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	out = append(out, snippetCase{name: "coder_error", class: "coder_error",
		err: fdec.Decode(&fb)})

	bomb := gbon.NewDecoder(bytes.NewReader(craftedAllocBomb()))
	bomb.SetLimits(gbon.Limits{MaxBytes: math.MaxInt64, MaxSliceLen: math.MaxInt64})
	var ab []int8
	out = append(out, snippetCase{name: "budget_alloc", class: "budget_alloc",
		err: bomb.Decode(&ab)})

	var pb snippetPanicBox
	pdec := gbon.NewDecoder(bytes.NewReader(snippetCoderStream(t, snippetPanicBox{N: 7}, snippetPanicCoder{})))
	if err := pdec.RegisterCoder(snippetPanicBox{}, snippetPanicCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	out = append(out, snippetCase{name: "internal_panic", class: "internal_panic",
		err: pdec.Decode(&pb)})

	// bad_path: a zero-step path — the marker directly before the
	// terminator — patched into a middle-field interior-alias stream
	ps := snippetPathS{A: 1, B: 2, C: 3}
	pstream, err := gbon.Marshal(&snippetPathW{V: &ps, P: &ps.B})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ph := hex.EncodeToString(pstream)
	pi := strings.LastIndex(ph, "05")
	zb, err := hex.DecodeString(ph[:pi] + "0506")
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	var pw snippetPathW
	out = append(out, snippetCase{name: "bad_path", class: "bad_path",
		err: gbon.Unmarshal(zb, &pw)})

	// evolution_ref_unmaterialized: a kept map-typed REF over the map
	// record the narrowing skip left unmaterialized
	nm := map[bool]int64{true: 1}
	nb, err := gbon.Marshal(&snippetNarrowMapW{A: nm, B: nm})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ndec := gbon.NewDecoder(bytes.NewReader(nb))
	if err := ndec.RegisterAs("github.com/gbon-format/gbon-go_test.gbon_test.snippetNarrowMapW", snippetNarrowB{}); err != nil {
		t.Fatalf("RegisterAs: %v", err)
	}
	var nn snippetNarrowB
	out = append(out, snippetCase{name: "evolution_ref_unmaterialized", class: "evolution_ref_unmaterialized",
		err: ndec.Decode(&nn)})

	out = append(out, snippetCase{name: "unstable_tie_break", class: "unstable_tie_break",
		err: stableErr(snippetTieMap())})
	out = append(out, snippetCase{name: "unstable_zero_float_key", class: "unstable_zero_float_key",
		err: stableErr(map[float64]int{0: 1})})

	seen := map[string]string{}
	for _, tc := range out {
		if tc.err == nil {
			t.Fatalf("probe %s produced no error", tc.name)
		}
		if prev, dup := seen[tc.class]; dup {
			t.Fatalf("class %s probed twice (%s, %s)", tc.class, prev, tc.name)
		}
		seen[tc.class] = tc.name
	}
	for _, class := range snippetClasses {
		if _, ok := seen[class]; !ok {
			t.Fatalf("corpus missing class %s", class)
		}
	}
	if len(seen) != len(snippetClasses) {
		t.Fatalf("corpus has %d classes, want %d", len(seen), len(snippetClasses))
	}
	return out
}

// Baseline line budgets: a whole map entry (key plus both quoted values
// plus syntax) staying within lineBudget renders on one physical line;
// longer values are chunked into %q pieces whose quoted length stays within
// pieceBudget. Piece concatenation is byte-identical to the value (chunks
// split on raw bytes; %q escapes the remainder).
const (
	lineBudget  = 180
	pieceBudget = 150
)

// snippetPieces splits s greedily so every %q-rendered piece stays within
// pieceBudget and joins the pieces with line continuations.
func snippetPieces(s string) string {
	var parts []string
	for len(s) > 0 {
		n := 1
		for n < len(s) && len(fmt.Sprintf("%q", s[:n+1])) <= pieceBudget {
			n++
		}
		parts = append(parts, fmt.Sprintf("%q", s[:n]))
		s = s[n:]
	}
	return strings.Join(parts, " +\n\t")
}

// snippetEntry renders one baseline map entry; every emitted physical line
// stays within lineBudget.
func snippetEntry(key string, v [2]string) string {
	kq := fmt.Sprintf("%q", key)
	q0 := fmt.Sprintf("%q", v[0])
	q1 := fmt.Sprintf("%q", v[1])
	if len(kq)+len(q0)+len(q1)+len("\t: {}, \n") <= lineBudget {
		return fmt.Sprintf("\t%s: {%s, %s},\n", kq, q0, q1)
	}
	return fmt.Sprintf("\t%s: {%s,\n\t%s},\n", kq, snippetPieces(v[0]), snippetPieces(v[1]))
}

// snippetWriteBaseline regenerates the baseline file from got.
func snippetWriteBaseline(got map[string][2]string) error {
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("package gbon_test\n\n")
	b.WriteString("// Identity baseline of the error renders (materialization fixture): %v and\n")
	b.WriteString("// %+v of one crafted probe per error class, pinned as literals. The\n")
	b.WriteString("// materializer below regenerates this file when the baseline is empty;\n")
	b.WriteString("// once materialized, every run compares strictly — a render change\n")
	b.WriteString("// of the default (no trust flag) path fails here. Do not edit by hand.\n\n")
	b.WriteString("var snippetIdentityBaseline = map[string][2]string{\n")
	for _, k := range keys {
		b.WriteString(snippetEntry(k, got[k]))
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		return fmt.Errorf("format generated baseline: %v", err)
	}
	return os.WriteFile("errors_snippet_baseline_test.go", src, 0o644)
}

// TestSnippetIdentityMaterialize pins the default renders of the full class
// corpus. First run (empty baseline): materializes the literals file. Subsequent
// runs: strict comparison in both directions — the default rendering of
// errors must stay byte-identical to the pinned baseline.
func TestSnippetIdentityMaterialize(t *testing.T) {
	got := map[string][2]string{}
	for _, tc := range snippetCorpus(t) {
		var ae *gbon.Error
		if !errors.As(tc.err, &ae) {
			t.Fatalf("probe %s: not As-recoverable: %v", tc.name, tc.err)
		}
		if ae.Class() != tc.class {
			t.Fatalf("probe %s: class = %q, want %q", tc.name, ae.Class(), tc.class)
		}
		got[tc.class] = [2]string{ae.Error(), fmt.Sprintf("%+v", ae)}
	}
	if len(snippetIdentityBaseline) != len(snippetClasses) {
		if err := snippetWriteBaseline(got); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		t.Fatalf("baseline was empty: materialized %d classes, re-run to verify", len(got))
	}
	for class, want := range snippetIdentityBaseline {
		g, ok := got[class]
		if !ok {
			t.Errorf("baseline class %s not probed by the corpus", class)
			continue
		}
		if g != want {
			t.Errorf("class %s: default %%v/%%+v drifted\n got %%v: %q\nwant %%v: %q\n got %%+v: %q\nwant %%+v: %q",
				class, g[0], want[0], g[1], want[1])
		}
	}
	for class := range got {
		if _, ok := snippetIdentityBaseline[class]; !ok {
			t.Errorf("corpus class %s absent from the baseline", class)
		}
	}
}

// PB6 — absolute offset and snippet site in a multi-record stream: the
// budget breach of record 2's second blob reports Error.Offset at the
// known absolute charge site (a window-relative site would be the
// defect) and
// the trusted-input snippet window is captured around that absolute
// offset, clamped to the window base (the B1 body's readDirect reset
// leaves the window starting at the B1 view token).
func TestSnippetPB6AbsoluteOffsetGolden(t *testing.T) {
	data, filler, _, afterB1, afterB2hdr := pb6Stream()
	dec := gbon.NewDecoder(bytes.NewReader(data))
	dec.SetLimits(gbon.Limits{MaxBytes: 8 << 20})
	dec.SetTrustedInput(true)
	var rec1 []byte
	if err := dec.Decode(&rec1); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if !bytes.Equal(rec1, filler) {
		t.Fatalf("record 1 = %d bytes, want the %d-byte filler", len(rec1), len(filler))
	}
	var box pb6Box
	err := dec.Decode(&box)
	var ae *gbon.Error
	if !errors.As(err, &ae) {
		t.Fatalf("record 2: err = %v, want *gbon.Error", err)
	}
	if ae.Class() != "budget_bytes" || !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("class = %q, err = %v, want budget_bytes/ErrBudget", ae.Class(), err)
	}
	if ae.Path != "$.B2" {
		t.Fatalf("path = %q, want $.B2", ae.Path)
	}
	if ae.Offset != afterB2hdr {
		t.Fatalf("Offset = %d, want absolute charge site %d", ae.Offset, afterB2hdr)
	}
	if ae.Offset < largeReadScale {
		t.Fatalf("Offset = %d not >largeRead-scale", ae.Offset)
	}
	sn, ok := ae.Snippet()
	if !ok {
		t.Fatalf("no snippet captured in trusted mode")
	}
	winBase := afterB1 - 2 // the B1 view token: the window base after the body's reset
	if sn.ByteOffset != winBase {
		t.Fatalf("snippet ByteOffset = %d, want window base %d", sn.ByteOffset, winBase)
	}
	wantLen := afterB2hdr + snippetAfterLen - winBase
	if sn.ByteLength != wantLen {
		t.Fatalf("snippet ByteLength = %d, want %d", sn.ByteLength, wantLen)
	}
	want := data[winBase : winBase+wantLen]
	got, derr := base64.StdEncoding.DecodeString(sn.Base64)
	if derr != nil {
		t.Fatalf("snippet base64: %v", derr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snippet bytes diverge from the stream window")
	}
}

// PB6 scale pins: the wire large-read threshold (stream reads above it
// bypass the sliding window) and the snippet's after-window extent.
const (
	largeReadScale  = 1 << 20
	snippetAfterLen = 16
)
