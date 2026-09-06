package gbon

// Trusted-input window tests: the boundary table of the capture (typical
// window, front clamp, tail clamp without realignment, zero offset, empty
// degenerate, offset at the buffer base), the stdlib cross-check of the
// unmarked dump form, the machine triple of Snippet, the capture wiring
// over the public API (flag on and off, no reads past the fault), and the
// dump materializer pinning the verbose-render literals.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// snippetPattern is the deterministic buffer content: printable spans
// with a control byte every 13th position so the ASCII gutter of the dump
// carries dots as well as printable characters.
func snippetPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		if i%13 == 5 {
			b[i] = 0x01
			continue
		}
		b[i] = byte(0x20 + i%95)
	}
	return b
}

// dumpRow is one boundary row of the capture table: the error offset and
// the reader buffer (absolute bounds), the expected window bounds, and
// the marked byte with its 1-based dump line.
type dumpRow struct {
	name       string
	base       int
	buf        []byte
	off        int
	start, end int
	marked     int // -1 when the window is empty
	markedLine int // 1-based; 0 when the window is empty
}

func dumpRows() []dumpRow {
	return []dumpRow{
		{name: "typical", base: 0, buf: snippetPattern(100), off: 40, start: 8, end: 56, marked: 39, markedLine: 2},
		{name: "front-clamp", base: 0, buf: snippetPattern(20), off: 5, start: 0, end: 20, marked: 4, markedLine: 1},
		{name: "tail-clamp", base: 100, buf: snippetPattern(64), off: 164, start: 132, end: 164, marked: 163, markedLine: 2},
		{name: "zero-offset", base: 0, buf: snippetPattern(40), off: 0, start: 0, end: 16, marked: 0, markedLine: 1},
		{name: "empty", base: 0, buf: nil, off: 0, start: 0, end: 0, marked: -1, markedLine: 0},
		{name: "offset-at-base", base: 10, buf: snippetPattern(40), off: 10, start: 10, end: 26, marked: 10, markedLine: 1},
	}
}

// injectSnippet builds a decode error carrying the given captured window
// directly (the render path under test — the capture choke point is
// covered by the wiring tests below).
func injectSnippet(row dumpRow) *Error {
	e := errFormat(classTruncated, row.off, "$", nil, nil, errDetail("end of input"))
	if row.marked >= 0 {
		e.snippet = append([]byte(nil), row.buf[row.start-row.base:row.end-row.base]...)
		e.snippetOff = row.start
		e.snippetSet = true
	}
	return e
}

// dumpLines extracts the hex block of a verbose render.
func dumpLines(t *testing.T, verbose string) []string {
	t.Helper()
	const header = "\n    input =\n"
	_, after, ok := strings.Cut(verbose, header)
	if !ok {
		t.Fatalf("verbose render lacks the input block:\n%s", verbose)
	}
	return strings.Split(after, "\n")
}

// TestSnippetDumpBounds: line count, absolute offset columns, the single
// marked line, and the empty window omitting the block entirely.
func TestSnippetDumpBounds(t *testing.T) {
	for _, row := range dumpRows() {
		t.Run(row.name, func(t *testing.T) {
			verbose := fmt.Sprintf("%+v", injectSnippet(row))
			if row.marked < 0 {
				if strings.Contains(verbose, "input =") {
					t.Fatalf("empty window rendered a block:\n%s", verbose)
				}
				return
			}
			lines := dumpLines(t, verbose)
			wantLines := (row.end - row.start + 15) / 16
			if len(lines) != wantLines {
				t.Fatalf("dump has %d lines, want %d:\n%s", len(lines), wantLines, verbose)
			}
			markedSeen := 0
			for i, ln := range lines {
				if strings.HasPrefix(ln, "=> ") {
					ln = ln[3:]
					markedSeen++
					if i+1 != row.markedLine {
						t.Fatalf("marked line is %d, want %d:\n%s", i+1, row.markedLine, verbose)
					}
				}
				off, err := strconv.ParseUint(ln[:8], 16, 64)
				if err != nil {
					t.Fatalf("line %d lacks the 8-digit offset column: %q", i, ln)
				}
				if int(off) != row.start+16*i {
					t.Fatalf("line %d offset %#x, want %#x (absolute, no realignment)", i, off, row.start+16*i)
				}
				if !strings.HasSuffix(ln, "|") || !strings.Contains(ln, "  |") {
					t.Fatalf("line %d lacks the ASCII gutter: %q", i, ln)
				}
			}
			if markedSeen != 1 {
				t.Fatalf("marked lines = %d, want 1:\n%s", markedSeen, verbose)
			}
		})
	}
}

// TestSnippetDumpStdlibCrossCheck: for windows starting at offset 0 the
// unmarked dump form (marker prefixes stripped) is byte-for-byte the
// stdlib hex.Dump of the window — an independent oracle for the renderer.
func TestSnippetDumpStdlibCrossCheck(t *testing.T) {
	for _, row := range dumpRows() {
		if row.start != 0 || row.marked < 0 {
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			lines := dumpLines(t, fmt.Sprintf("%+v", injectSnippet(row)))
			for i := range lines {
				lines[i] = strings.TrimPrefix(lines[i], "=> ")
			}
			window := row.buf[row.start-row.base : row.end-row.base]
			want := strings.TrimSuffix(hex.Dump(window), "\n")
			if got := strings.Join(lines, "\n"); got != want {
				t.Fatalf("unmarked dump diverges from hex.Dump:\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// snippetDumpBaseline holds the materialized verbose renders of the
// boundary rows; regenerated by TestSnippetDumpMaterialize when empty,
// compared strictly afterwards.

// dump-literal-begin
var snippetDumpBaseline = map[string]string{
	"empty":          "gbon: truncated at $ (offset 0): end of input\n    class = truncated\n    offset = 0\n    path = $\n    cause = end of input",
	"front-clamp":    "gbon: truncated at $ (offset 5): end of input\n    class = truncated\n    offset = 5\n    path = $\n    cause = end of input\n    input =\n=> 00000000  20 21 22 23 24 01 26 27  28 29 2a 2b 2c 2d 2e 2f  | !\"#$.&'()*+,-./|\n00000010  30 31 01 33                                       |01.3|",
	"offset-at-base": "gbon: truncated at $ (offset 10): end of input\n    class = truncated\n    offset = 10\n    path = $\n    cause = end of input\n    input =\n=> 0000000a  20 21 22 23 24 01 26 27  28 29 2a 2b 2c 2d 2e 2f  | !\"#$.&'()*+,-./|",
	"tail-clamp":     "gbon: truncated at $ (offset 164): end of input\n    class = truncated\n    offset = 164\n    path = $\n    cause = end of input\n    input =\n00000084  40 41 42 43 44 45 46 47  48 49 4a 4b 01 4d 4e 4f  |@ABCDEFGHIJK.MNO|\n=> 00000094  50 51 52 53 54 55 56 57  58 01 5a 5b 5c 5d 5e 5f  |PQRSTUVWX.Z[\\]^_|",
	"typical":        "gbon: truncated at $ (offset 40): end of input\n    class = truncated\n    offset = 40\n    path = $\n    cause = end of input\n    input =\n00000008  28 29 2a 2b 2c 2d 2e 2f  30 31 01 33 34 35 36 37  |()*+,-./01.34567|\n=> 00000018  38 39 3a 3b 3c 3d 3e 01  40 41 42 43 44 45 46 47  |89:;<=>.@ABCDEFG|\n00000028  48 49 4a 4b 01 4d 4e 4f  50 51 52 53 54 55 56 57  |HIJK.MNOPQRSTUVW|",
	"zero-offset":    "gbon: truncated at $ (offset 0): end of input\n    class = truncated\n    offset = 0\n    path = $\n    cause = end of input\n    input =\n=> 00000000  20 21 22 23 24 01 26 27  28 29 2a 2b 2c 2d 2e 2f  | !\"#$.&'()*+,-./|",
}

// dump-literal-end

// TestSnippetDumpMaterialize: pins the verbose-render literal of every
// boundary row. First run (empty baseline) rewrites the literal block;
// subsequent runs compare strictly.
func TestSnippetDumpMaterialize(t *testing.T) {
	got := map[string]string{}
	for _, row := range dumpRows() {
		got[row.name] = fmt.Sprintf("%+v", injectSnippet(row))
	}
	if len(snippetDumpBaseline) != len(dumpRows()) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b bytes.Buffer
		b.WriteString("// dump-literal-begin\nvar snippetDumpBaseline = map[string]string{\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "\t%q: %q,\n", k, got[k])
		}
		b.WriteString("}\n// dump-literal-end\n")
		self := string(mustReadSelf(t))
		const anchor = "// dump-literal-begin\nvar snippetDumpBaseline = map[string]string{}\n// dump-literal-end\n"
		if !strings.Contains(self, anchor) {
			t.Fatalf("materialize: literal block anchor not found")
		}
		src, err := format.Source([]byte(strings.ReplaceAll(self, anchor, b.String())))
		if err != nil {
			t.Fatalf("materialize: format: %v", err)
		}
		if err := os.WriteFile("errors_snippet_test.go", src, 0o644); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		t.Fatalf("dump baseline was empty: materialized %d rows, re-run to verify", len(got))
	}
	for k, want := range snippetDumpBaseline {
		g, ok := got[k]
		if !ok {
			t.Errorf("baseline row %s not rendered by the table", k)
			continue
		}
		if g != want {
			t.Errorf("row %s drifted:\ngot:\n%s\nwant:\n%s", k, g, want)
		}
	}
}

func mustReadSelf(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("errors_snippet_test.go")
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	return raw
}

// TestSnippetMachineFields: the Snippet triple is consistent with the
// captured window and independently decodable.
func TestSnippetMachineFields(t *testing.T) {
	for _, row := range dumpRows() {
		if row.marked < 0 {
			continue
		}
		e := injectSnippet(row)
		sn, ok := e.Snippet()
		if !ok {
			t.Fatalf("%s: Snippet not ok", row.name)
		}
		if sn.ByteOffset != row.start || sn.ByteLength != row.end-row.start {
			t.Fatalf("%s: triple bounds (%d,%d), want (%d,%d)", row.name, sn.ByteOffset, sn.ByteLength, row.start, row.end-row.start)
		}
		raw, err := base64.StdEncoding.DecodeString(sn.Base64)
		if err != nil {
			t.Fatalf("%s: base64 not decodable: %v", row.name, err)
		}
		want := row.buf[row.start-row.base : row.end-row.base]
		if !bytes.Equal(raw, want) {
			t.Fatalf("%s: decoded window diverges (%d vs %d bytes)", row.name, len(raw), len(want))
		}
	}
}

// TestSnippetEmptyWindowSemantics: a captured-empty window answers the
// triple with ByteLength 0 and ok true — emptiness lives in the triple,
// not in the bool.
func TestSnippetEmptyWindowSemantics(t *testing.T) {
	e := errFormat(classIORead, 3, "", nil, nil, errors.New("fault"))
	e.snippetOff = 3
	e.snippetSet = true
	sn, ok := e.Snippet()
	if !ok || sn.ByteLength != 0 || sn.Base64 != "" || sn.ByteOffset != 3 {
		t.Fatalf("empty window = (%+v, %v), want ({3 0 \"\"}, true)", sn, ok)
	}
	if verbose := fmt.Sprintf("%+v", e); strings.Contains(verbose, "input =") {
		t.Fatalf("empty window rendered a block:\n%s", verbose)
	}
}

// TestSnippetAbsentSemantics: without a capture (no trust flag, or an
// error without an offset) Snippet answers false.
func TestSnippetAbsentSemantics(t *testing.T) {
	e := errFormat(classTruncated, 4, "$", nil, nil, errDetail("x"))
	if sn, ok := e.Snippet(); ok {
		t.Fatalf("uncaptured error answers (%+v, true)", sn)
	}
	d := &codecDecoder{fac: &Decoder{trusted: true}}
	// Offset < 0: the choke point passes through without capturing.
	kept := d.fail(errUnsupported(classContractMismatch, "$", nil, nil, errDetail("x")))
	if sn, ok := kept.(*Error).Snippet(); ok {
		t.Fatalf("offset-less error answers (%+v, true)", sn)
	}
}

// TestSnippetMarkedByteClamp: the marked byte is the last consumed byte,
// clamped into the window at both ends.
func TestSnippetMarkedByteClamp(t *testing.T) {
	cases := []struct {
		off, start, end, want int
	}{
		{40, 8, 56, 39},      // typical: off-1 inside
		{0, 0, 16, 0},        // front clamp: off-1 before the window
		{10, 10, 26, 10},     // offset at the window start
		{200, 132, 164, 163}, // terminal clamp: off-1 past the window end
	}
	for _, tc := range cases {
		e := &Error{Offset: tc.off, snippet: make([]byte, tc.end-tc.start), snippetOff: tc.start}
		if got := e.markedByte(); got != tc.want {
			t.Fatalf("markedByte(off=%d, [%d,%d)) = %d, want %d", tc.off, tc.start, tc.end, got, tc.want)
		}
	}
}

// snippetClaimStream crafts a 100-byte stream whose string-body claim
// starts reading at absolute offset 40: a padded wire name positions the
// descriptor, the literal length claims 61 bytes with 60 remaining.
func snippetClaimStream(t *testing.T) []byte {
	t.Helper()
	name := strings.Repeat("p", 28)
	out := []byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00}
	out = append(out, 0xDC, 0x0C) // descriptor literal, STRING kind (u8 form)
	out = append(out, 0x6C, byte(len(name)))
	out = append(out, name...)
	out = append(out, 0x6C, 61) // string literal, u8 length claiming 61 bytes
	out = append(out, snippetPattern(60)...)
	if len(out) != 100 {
		t.Fatalf("crafted stream is %d bytes, want 100", len(out))
	}
	return out
}

type snippetStr string

// TestSnippetCaptureWiring: a trusted Decoder captures the window around
// the failure offset end to end; the default Decoder on the same input
// renders identically minus the block and answers Snippet false.
func TestSnippetCaptureWiring(t *testing.T) {
	stream := snippetClaimStream(t)

	td := NewDecoder(bytes.NewReader(append([]byte(nil), stream...)))
	td.SetTrustedInput(true)
	if err := td.RegisterAs(strings.Repeat("p", 28), snippetStr("")); err != nil {
		t.Fatalf("RegisterAs: %v", err)
	}
	var s snippetStr
	err := td.Decode(&s)
	var ae *Error
	if !errors.As(err, &ae) || ae.Class() != classTruncated {
		t.Fatalf("want truncated, got %v", err)
	}
	if ae.Offset != 40 {
		t.Fatalf("offset = %d, want 40", ae.Offset)
	}
	sn, ok := ae.Snippet()
	if !ok || sn.ByteOffset != 8 || sn.ByteLength != 48 {
		t.Fatalf("wired window = (%d,%d,%t), want (8,48,true)", sn.ByteOffset, sn.ByteLength, ok)
	}
	verbose := fmt.Sprintf("%+v", ae)
	lines := dumpLines(t, verbose)
	if len(lines) != 3 {
		t.Fatalf("wired dump lines = %d, want 3:\n%s", len(lines), verbose)
	}
	for i, want := range []string{"00000008", "00000018", "00000028"} {
		if got := strings.TrimPrefix(lines[i], "=> "); got[:8] != want {
			t.Fatalf("line %d offset %q, want %q", i, got[:8], want)
		}
	}
	if !strings.HasPrefix(lines[1], "=> ") {
		t.Fatalf("marked line is not line 2:\n%s", verbose)
	}

	dd := NewDecoder(bytes.NewReader(append([]byte(nil), stream...)))
	if err := dd.RegisterAs(strings.Repeat("p", 28), snippetStr("")); err != nil {
		t.Fatalf("RegisterAs: %v", err)
	}
	var s2 snippetStr
	derr := dd.Decode(&s2)
	var dae *Error
	if !errors.As(derr, &dae) || dae.Class() != classTruncated || dae.Offset != 40 {
		t.Fatalf("default decode diverged: %v", derr)
	}
	if sn, ok := dae.Snippet(); ok {
		t.Fatalf("default decode captured a window (%+v)", sn)
	}
	if dverbose := fmt.Sprintf("%+v", dae); strings.Contains(dverbose, "input =") {
		t.Fatalf("default verbose render carries the block:\n%s", dverbose)
	}
}

// TestSnippetHeaderTruncation: a short header truncates at offset 0 — the
// front clamp of the marked byte on a live stream.
func TestSnippetHeaderTruncation(t *testing.T) {
	d := NewDecoder(bytes.NewReader([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00}))
	d.SetTrustedInput(true)
	var n int64
	err := d.Decode(&n)
	var ae *Error
	if !errors.As(err, &ae) || ae.Class() != classTruncated || ae.Offset != 0 {
		t.Fatalf("want truncated at 0, got %v", err)
	}
	sn, ok := ae.Snippet()
	if !ok || sn.ByteOffset != 0 || sn.ByteLength != 5 {
		t.Fatalf("window = (%d,%d,%t), want (0,5,true)", sn.ByteOffset, sn.ByteLength, ok)
	}
	lines := dumpLines(t, fmt.Sprintf("%+v", ae))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "=> ") || lines[0][3:11] != "00000000" {
		t.Fatalf("want a single marked line at 0:\n%v", lines)
	}
}

// countingFaultReader serves data, then fails; reads after the first
// fault are counted — the capture must never trigger one.
type countingFaultReader struct {
	data       []byte
	err        error
	reads      int
	afterFault int
	faulted    bool
}

func (r *countingFaultReader) Read(p []byte) (int, error) {
	r.reads++
	if r.faulted {
		r.afterFault++
	}
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	r.faulted = true
	return 0, r.err
}

// TestSnippetIOReadEmptyWindow: a reader faulting on the very first pull
// yields an empty captured window — the empty triple with ok true.
func TestSnippetIOReadEmptyWindow(t *testing.T) {
	marker := errors.New("SNIPPETIOFAULT")
	fr := &countingFaultReader{err: marker}
	d := NewDecoder(fr)
	d.SetTrustedInput(true)
	var n int64
	err := d.Decode(&n)
	var ae *Error
	if !errors.As(err, &ae) || ae.Class() != classIORead {
		t.Fatalf("want io_read, got %v", err)
	}
	if ae.Offset != 0 {
		t.Fatalf("offset = %d, want 0", ae.Offset)
	}
	sn, ok := ae.Snippet()
	if !ok || sn.ByteOffset != 0 || sn.ByteLength != 0 || sn.Base64 != "" {
		t.Fatalf("empty window = (%+v,%t), want ({0 0 \"\"},true)", sn, ok)
	}
	if verbose := fmt.Sprintf("%+v", ae); strings.Contains(verbose, "input =") {
		t.Fatalf("empty window rendered a block:\n%s", verbose)
	}
}

// TestSnippetNoExtraReads: the capture reads nothing beyond what the
// plain error path already pulled — the trusted and default decodes
// issue the same number of reads, and none happens after the fault.
func TestSnippetNoExtraReads(t *testing.T) {
	mk := func() []byte {
		b, err := Marshal(int64(7))
		if err != nil {
			t.Fatal(err)
		}
		return b[:len(b)-2] // truncate mid-body: read error on the pull
	}
	trusted := &countingFaultReader{data: mk(), err: errors.New("FAULTT")}
	td := NewDecoder(trusted)
	td.SetTrustedInput(true)
	var a int64
	terr := td.Decode(&a)
	if terr == nil {
		t.Fatal("trusted decode unexpectedly succeeded")
	}
	def := &countingFaultReader{data: mk(), err: errors.New("FAULTT")}
	dd := NewDecoder(def)
	var b int64
	derr := dd.Decode(&b)
	if derr == nil {
		t.Fatal("default decode unexpectedly succeeded")
	}
	if trusted.afterFault != 0 {
		t.Fatalf("capture pulled the source after the fault: %d reads", trusted.afterFault)
	}
	if trusted.reads != def.reads {
		t.Fatalf("trusted decode made %d reads, default made %d", trusted.reads, def.reads)
	}
}

type snippetSubBox struct{ N int64 }

type snippetSubCoder struct{}

func (snippetSubCoder) EncodeValue(e *Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(snippetSubBox).N)
}

func (snippetSubCoder) DecodeValue(d *Decoder, v reflect.Value) error {
	var n int64
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.SetInt(n)
	return nil
}

// TestSnippetCoderSubDecode: an error surfacing from a coder-body
// sub-decode carries exactly one coherent window — the choke point does
// not re-capture on the way out through the coder seam.
func TestSnippetCoderSubDecode(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.RegisterCoder(snippetSubBox{}, snippetSubCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(snippetSubBox{N: 7}); err != nil {
		t.Fatal(err)
	}
	stream := buf.Bytes()[:len(buf.Bytes())-6]

	tr := NewDecoder(bytes.NewReader(stream))
	tr.SetTrustedInput(true)
	if err := tr.RegisterCoder(snippetSubBox{}, snippetSubCoder{}); err != nil {
		t.Fatal(err)
	}
	var out snippetSubBox
	err := tr.Decode(&out)
	var ae *Error
	if !errors.As(err, &ae) || ae.Class() != classTruncated {
		t.Fatalf("want truncated through the coder seam, got %v", err)
	}
	sn, ok := ae.Snippet()
	if !ok {
		t.Fatalf("sub-decode error lacks a window: %+v", ae)
	}
	if sn.ByteLength == 0 || sn.ByteLength > 48 {
		t.Fatalf("sub-decode window length %d outside (0,48]", sn.ByteLength)
	}
	if sn.ByteOffset != max(0, ae.Offset-32) {
		t.Fatalf("window start %d, want the clamped offset-32 = %d (off %d)", sn.ByteOffset, max(0, ae.Offset-32), ae.Offset)
	}
}
