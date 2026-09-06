package gbon_test

// The crafted corpus behind the identity baseline and the structured-log
// probes: one public-API probe per error class, all twenty classes covered.
// The seventeen probes of the oracle corpus are reused as-is; the three
// the rest of the classes get dedicated probes here (duplicate key stream, a
// failing custom coder, a panicking custom coder recovered by the decode
// panic guard).

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
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
	"overflow_value", "duplicate_key", "bad_ref", "bad_view",
	"type_mismatch", "unknown_name",
	"budget_depth", "budget_nodes", "budget_bytes", "budget_alloc",
	"unsupported_kind", "register_conflict", "coder_error", "coder_recursion",
	"contract_mismatch", "io_read",
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

// snippetPanicCoder panics inside the decode body; the decode panic guard
// turns the panic into the allocation class.
type snippetPanicBox struct{ N int64 }

type snippetPanicCoder struct{}

func (snippetPanicCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(snippetPanicBox).N)
}

func (snippetPanicCoder) DecodeValue(*gbon.Decoder, reflect.Value) error {
	panic("SNIPPETALLOCBOOM")
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

// snippetCorpus builds the twenty-class probe set. Every probe goes through
// the public API on a crafted input; the returned cases carry distinct
// classes, asserted against the inventory above.
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

	var pb snippetPanicBox
	pdec := gbon.NewDecoder(bytes.NewReader(snippetCoderStream(t, snippetPanicBox{N: 7}, snippetPanicCoder{})))
	if err := pdec.RegisterCoder(snippetPanicBox{}, snippetPanicCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	out = append(out, snippetCase{name: "budget_alloc", class: "budget_alloc",
		err: pdec.Decode(&pb)})

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
