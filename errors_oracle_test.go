package gbon_test

// A1 oracle and render/path-grammar oracle over the crafted corpus
// (spec SC-B1a, SC-C4a, SC-C7a; properties P3/P4). The corpus drives one
// concrete public-API probe per error class; the oracle asserts the
// three-part A1 boundary per categoria: (i) payload substrings of the
// untrusted input never leak into %v, (ii) caller arguments (limits,
// registry names) may appear, (iii) address path segments render in the
// bounded grammar.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// oracleCase is one corpus probe: the error obtained from the public API,
// its expected class and sentinel, whether it came from a decode context
// (byte offset required), and the marker payload substrings of the input
// that must never surface in %v.
type oracleCase struct {
	name       string
	class      string
	sentinel   error
	err        error
	decodeCtx  bool
	hasPath    bool
	payload    []string
	callerArgs []string // trusted caller arguments allowed (and expected) in %v
}

// oracleFaultReader serves data, then fails with err (io injection seam).
type oracleFaultReader struct {
	data []byte
	err  error
}

func (r *oracleFaultReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func oracleDupStream(key string) []byte {
	c := newCraft()
	c.descPos(dMap(dString, dInt64))
	c.mapRec(2)
	c.strPos(key)
	c.intTok(1)
	c.strPos(key)
	c.intTok(2)
	return c.buf
}

// oracleCorpus builds one probe per class reachable through the public
// API. budget_alloc (decode panic recovery) has no craftable public
// probe: its charges fire before any allocation that could panic.
func oracleCorpus(t *testing.T) []oracleCase {
	t.Helper()
	mk := func(name, class string, sentinel error, err error, decodeCtx, hasPath bool, payload ...string) oracleCase {
		return oracleCase{name: name, class: class, sentinel: sentinel, err: err,
			decodeCtx: decodeCtx, hasPath: hasPath, payload: payload}
	}

	var out []oracleCase

	// data/format
	var v any
	out = append(out,
		mk("bad_magic", "bad_magic", gbon.ErrFormat,
			gbon.Unmarshal([]byte("ZZYVALmarker\x01\x00"+string([]byte{0xD8, 0x63})), &v), true, false, "ZZYVALmarker"),
	)
	tb, _ := gbon.Marshal("MARKERSTRVALUE")
	var sv string
	out = append(out,
		mk("truncated", "truncated", gbon.ErrFormat, gbon.Unmarshal(tb[:4], &sv), true, false, "MARKERSTRVALUE"),
	)
	c := newCraft()
	c.descPos(dString)
	c.strWide(0x0C, 11, "PROBEARGBODY")
	out = append(out,
		mk("malformed_arg", "malformed_arg", gbon.ErrFormat, gbon.Unmarshal(c.buf, &sv), true, false, "PROBEARGBODY"),
	)
	c = newCraft()
	c.descPos(dSlice(dInt64))
	c.arrayRec(2, 2)
	c.intTok(7)
	c.intTok(0)
	var s64 []int64
	out = append(out,
		mk("malformed_op", "malformed_op", gbon.ErrFormat, gbon.Unmarshal(c.buf, &s64), true, false),
	)
	c = newCraft()
	c.descPos(dMap(dString, dInt64))
	c.refUnregistered(100)
	var msi map[string]int64
	out = append(out,
		mk("bad_ref", "bad_ref", gbon.ErrFormat, gbon.Unmarshal(c.buf, &msi), true, false),
	)
	c = newCraft()
	c.descPos(dSlice(dInt64))
	r := c.arrayRec(2, 0)
	c.view2(r, 0, 2, 6)
	out = append(out,
		mk("bad_view", "bad_view", gbon.ErrFormat, gbon.Unmarshal(c.buf, &s64), true, false),
	)
	fb, _ := gbon.Marshal(1.0)
	var iv int
	out = append(out,
		mk("type_mismatch", "type_mismatch", gbon.ErrFormat, gbon.Unmarshal(fb, &iv), true, false),
	)
	ub, _ := gbon.Marshal(struct{ V any }{struct{ X int64 }{1}})
	var uv struct{ V any }
	out = append(out,
		mk("unknown_name", "unknown_name", gbon.ErrFormat, gbon.Unmarshal(ub, &uv), true, true, "struct { X int64 }"),
	)
	// the overflowing number is untrusted stream payload: it lands in
	// Got (%+v only) and must never surface in %v; a width-8 int token
	// behind a width-1 descriptor reaches the range check
	const ovfMarker = int64(0xDEADBEEFCAFE)
	c = newCraft()
	c.descPos(dInt8)
	c.intTok(ovfMarker)
	var i8 int8
	out = append(out,
		mk("overflow_value", "overflow_value", gbon.ErrFormat, gbon.Unmarshal(c.buf, &i8), true, true, fmt.Sprintf("%d", ovfMarker)),
	)

	// budget
	name := qn(selfRef{})
	db := []byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00, 0xD1, 0x6C, byte(len(name))}
	db = append(db, name...)
	db = append(db, 0xC0)
	for range 10002 {
		db = append(db, 0x81, 0x01)
	}
	db = append(db, 0x20)
	var sr selfRef
	out = append(out,
		mk("budget_depth", "budget_depth", gbon.ErrBudget, gbon.Unmarshal(db, &sr), true, true),
	)
	c = newCraft()
	fields := make([]cField, 200)
	for i := range fields {
		fields[i] = cField{fmt.Sprintf("F%d", i), dInt64}
	}
	c.descPos(dStructT("oracle.wideDescStruct", fields...))
	dn := gbon.NewDecoder(bytes.NewReader(c.buf))
	dn.SetLimits(gbon.Limits{MaxNodes: 100})
	var wm map[string]int64
	out = append(out,
		mk("budget_nodes", "budget_nodes", gbon.ErrBudget, dn.Decode(&wm), true, false),
	)
	c = newCraft()
	c.descPos(dBlob)
	c.blobRec(100, 0, nil)
	c.view0(c.alloc())
	dbd := gbon.NewDecoder(bytes.NewReader(c.buf))
	dbd.SetLimits(gbon.Limits{MaxBytes: 32})
	var bb []byte
	out = append(out,
		mk("budget_bytes", "budget_bytes", gbon.ErrBudget, dbd.Decode(&bb), true, true),
	)

	// code
	out = append(out, mk("unsupported_kind", "unsupported_kind", gbon.ErrUnsupported,
		func() error { _, err := gbon.Marshal(struct{ F func() }{}); return err }(), false, true),
	)
	out = append(out, mk("coder_recursion", "coder_recursion", gbon.ErrUnsupported,
		func() error {
			var buf bytes.Buffer
			enc := gbon.NewEncoder(&buf)
			if err := enc.RegisterCoder(oracleRec{}, oracleRecCoder{}); err != nil {
				return err
			}
			return enc.Encode(oracleRec{N: 1})
		}(), false, true),
	)
	// registry conflict: out-of-context form — no path, no offset; the
	// wire name is a trusted caller argument and stays in the text
	rcc := mk("register_conflict", "register_conflict", gbon.ErrUnsupported,
		func() error {
			var buf bytes.Buffer
			enc := gbon.NewEncoder(&buf)
			if err := enc.RegisterAs("oracle.conflictName", oracleRec{}); err != nil {
				return err
			}
			return enc.RegisterAs("oracle.conflictName", struct{ X int64 }{})
		}(), false, false)
	rcc.callerArgs = []string{"oracle.conflictName"}
	out = append(out, rcc)

	// contract (caller arguments: limits are trusted and stay in the text)
	dec := gbon.NewDecoder(bytes.NewReader(tb))
	dec.SetLimits(gbon.Limits{MaxDepth: -1})
	var n int
	oc := mk("contract_mismatch", "contract_mismatch", gbon.ErrUnsupported, dec.Decode(&n), false, false)
	oc.callerArgs = []string{"Limits.MaxDepth"}
	out = append(out, oc)

	// env (fault injection; the cause is the caller-side reader fault,
	// not input payload, so it legitimately appears in %v)
	marker := errors.New("ORACLEIOFAIL")
	vb, _ := gbon.Marshal(int64(7))
	fd := gbon.NewDecoder(&oracleFaultReader{data: vb[:3], err: marker})
	var seven int64
	out = append(out, mk("io_read", "io_read", gbon.ErrIO, fd.Decode(&seven), true, false))

	// env (write side): same attribution shape, the caller-side writer
	// fault as the cause
	out = append(out, mk("io_write", "io_write", gbon.ErrIO,
		func() error {
			enc := gbon.NewEncoder(&oracleFaultWriter{err: errors.New("ORACLEWRITEFAIL")})
			return enc.Encode(int64(7))
		}(), false, false))

	return out
}

// TestOracleClassSentinelMap (SC-C1a): every corpus error Is its class's
// sentinel and no other; a successful decode carries none of them.
func TestOracleClassSentinelMap(t *testing.T) {
	for _, tc := range oracleCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatalf("probe produced no error")
			}
			sentinels := []error{gbon.ErrFormat, gbon.ErrBudget, gbon.ErrUnsupported, gbon.ErrIO}
			for _, s := range sentinels {
				want := s == tc.sentinel
				if got := errors.Is(tc.err, s); got != want {
					t.Fatalf("Is(%v) = %v, want %v (err %v)", s, got, want, tc.err)
				}
			}
		})
	}
	// success side: a clean decode satisfies none of the four sentinels
	good, err := gbon.Marshal(int64(7))
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := gbon.Unmarshal(good, &n); err != nil {
		t.Fatal(err)
	}
}

// TestOracleAsRecover (SC-C2a): class and location are recoverable from
// every corpus error — *Error with a non-empty class, a byte offset for
// decode-context errors, a path where the class carries one.
func TestOracleAsRecover(t *testing.T) {
	for _, tc := range oracleCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			var ae *gbon.Error
			if !errors.As(tc.err, &ae) {
				t.Fatalf("not As-recoverable: %v", tc.err)
			}
			if ae.Class() != tc.class {
				t.Fatalf("class = %q, want %q", ae.Class(), tc.class)
			}
			if ae.Class() == "" {
				t.Fatalf("empty class: %+v", ae)
			}
			if tc.decodeCtx && ae.Offset < 0 {
				t.Fatalf("decode-context error lacks byte offset: %+v", ae)
			}
			if tc.hasPath && ae.Path == "" {
				t.Fatalf("class with location lacks path: %+v", ae)
			}
		})
	}
}

// oraclePathGrammar matches the full bounded path grammar: root marker,
// field segments, element indexes, %q-quoted printable keys up to 16
// runes, and the [<key N B>] bounded form.
var oraclePathGrammar = regexp.MustCompile(`^\$(\.[A-Za-z0-9_]+|\[\d+\]|\["[^"\n]*"\]|\[<key \d+ B>\])*$`)

// TestOraclePathGrammar (SC-C7a): exact path strings for the grammar
// segments — field, index, root-only — and the key bound edges: 16 and
// 17 printable runes, non-printable, quotes/backslash, Unicode.
func TestOraclePathGrammar(t *testing.T) {
	k16 := strings.Repeat("k", 16)
	k17 := strings.Repeat("k", 17)
	keys := []struct {
		key  string
		want string
	}{
		{k16, `["` + k16 + `"]`},
		{k17, "[<key 17 B>]"},
		{"a\x00b", "[<key 3 B>]"},
		{`a"b\c`, `["a\"b\\c"]`},
		{"ключик", `["ключик"]`},
	}
	for _, tc := range keys {
		var m map[string]int64
		err := gbon.Unmarshal(oracleDupStream(tc.key), &m)
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "duplicate_key" {
			t.Fatalf("key %q: want duplicate_key, got %v", tc.key, err)
		}
		if !strings.HasSuffix(ae.Path, tc.want) {
			t.Fatalf("key %q: path = %q, want suffix %q", tc.key, ae.Path, tc.want)
		}
	}

	// field segment ($.a), index segment ($[0]), root-only ($)
	_, err := gbon.Marshal(struct{ a int }{1})
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Path != "$.a" {
		t.Fatalf("field path: %v", err)
	}
	_, err = gbon.Marshal(map[float64]int{nan64(): 1})
	if !errors.As(err, &ae) || ae.Path != "$[0]" {
		t.Fatalf("index path: %v", err)
	}
	data, _ := hexDecode("67626F6E0000" + "D864696E743801" + "2D012C")
	var i8 int8
	err = gbon.Unmarshal(data, &i8)
	if !errors.As(err, &ae) || ae.Path != "$" {
		t.Fatalf("root-only path: %v", err)
	}
}

func nan64() float64 {
	var f float64
	return f / f
}

// oracleStripPath removes the " at <path>" segment from a %v rendering so
// payload probing can allow the bounded address segments (categoria iii).
func oracleStripPath(s string, ae *gbon.Error) string {
	if ae.Path == "" {
		return s
	}
	return strings.ReplaceAll(s, " at "+ae.Path, "")
}

// TestOracleA1NoLeak (SC-B1a, P3): categoria (i) — payload substrings of
// the untrusted input never appear in %v once the bounded path segments
// (categoria iii) are stripped; categoria (ii) — caller arguments do
// appear where the class carries them.
func TestOracleA1NoLeak(t *testing.T) {
	for _, tc := range oracleCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			var ae *gbon.Error
			if !errors.As(tc.err, &ae) {
				t.Fatalf("not As-recoverable: %v", tc.err)
			}
			// (iii) the path, when present, is entirely bounded grammar
			if ae.Path != "" && !oraclePathGrammar.MatchString(ae.Path) {
				t.Fatalf("path outside bounded grammar: %q", ae.Path)
			}
			// (i) payload substrings absent from %v (path segments stripped)
			stripped := oracleStripPath(tc.err.Error(), ae)
			for _, p := range tc.payload {
				if strings.Contains(stripped, p) {
					t.Fatalf("%%v leaks input payload %q: %q", p, tc.err.Error())
				}
			}
			// (ii) caller arguments stay
			for _, c := range tc.callerArgs {
				if !strings.Contains(tc.err.Error(), c) {
					t.Fatalf("%%v lost caller argument %q: %q", c, tc.err.Error())
				}
			}
		})
	}
}

// TestOracleRenderOneLine (SC-C4a, P4): %v is one line in the uniform
// shape "gbon: <class>[ at <path>][ (offset N)][: cause]"; %+v carries
// the structured fields.
func TestOracleRenderOneLine(t *testing.T) {
	for _, tc := range oracleCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			var ae *gbon.Error
			if !errors.As(tc.err, &ae) {
				t.Fatalf("not As-recoverable: %v", tc.err)
			}
			s := tc.err.Error()
			if strings.Contains(s, "\n") {
				t.Fatalf("%%v is not one line: %q", s)
			}
			prefix := "gbon: " + tc.class
			if !strings.HasPrefix(s, prefix) {
				t.Fatalf("%%v lacks uniform class prefix: %q", s)
			}
			rest := s[len(prefix):]
			if ae.Path != "" {
				if !strings.HasPrefix(rest, " at "+ae.Path) {
					t.Fatalf("%%v lacks the at-path segment: %q", s)
				}
				rest = rest[len(" at "+ae.Path):]
			}
			if ae.Offset >= 0 {
				if !strings.HasPrefix(rest, fmt.Sprintf(" (offset %d)", ae.Offset)) {
					t.Fatalf("%%v lacks the offset segment: %q", s)
				}
				rest = rest[len(fmt.Sprintf(" (offset %d)", ae.Offset)):]
			}
			if rest != "" && !strings.HasPrefix(rest, ": ") {
				t.Fatalf("%%v has non-cause trailing text: %q", s)
			}
			verbose := fmt.Sprintf("%+v", ae)
			for _, part := range []string{"\n    class = " + tc.class} {
				if !strings.Contains(verbose, part) {
					t.Fatalf("%%+v missing %q: %q", part, verbose)
				}
			}
			if ae.Path != "" && !strings.Contains(verbose, "\n    path = "+ae.Path) {
				t.Fatalf("%%+v missing path field: %q", verbose)
			}
			if ae.Offset >= 0 && !strings.Contains(verbose, fmt.Sprintf("\n    offset = %d", ae.Offset)) {
				t.Fatalf("%%+v missing offset field: %q", verbose)
			}
		})
	}
}

type oracleRec struct{ N int }

type oracleRecCoder struct{}

func (oracleRecCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(oracleRec))
}

func (oracleRecCoder) DecodeValue(*gbon.Decoder, reflect.Value) error {
	return errors.New("unreachable in this fixture")
}
