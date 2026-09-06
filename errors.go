package gbon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode"
)

// Sentinel errors of the codec error contract. Every error returned by the
// public API carries exactly one of them through errors.Is, derived from
// its class by the class-sentinel table below. The fourth sentinel, ErrIO,
// attributes a failed read on the underlying input stream (an environment
// fault, distinct from malformed data).
var (
	// ErrUnsupported reports a value of a category that has no serialized
	// form: func, chan, unsafe.Pointer, or a struct with unexported fields
	// (unless allowed explicitly), a coder failing, or a registry conflict.
	ErrUnsupported = errors.New("gbon: value type not serializable")

	// ErrBudget reports that a decoding or encoding budget (depth, nodes,
	// bytes — including charged backing allocations on decode — map pairs,
	// or slice length) was exhausted.
	ErrBudget = errors.New("gbon: decoding budget exceeded")

	// ErrFormat reports malformed wire input.
	ErrFormat = errors.New("gbon: malformed input")

	// ErrIO reports that the underlying reader failed while decode was
	// pulling input; the original fault is reachable through Unwrap.
	ErrIO = errors.New("gbon: underlying input read failed")
)

// Error class IDs — snake_case strings carried by Error.Class. They are a
// public contract: additive only; IDs are never renamed or reused. Five
// attribution families: data/format, budget, code, contract, env.
const (
	classBadMagic         = "bad_magic"
	classTruncated        = "truncated"
	classMalformedOp      = "malformed_op"
	classMalformedArg     = "malformed_arg"
	classOverflowValue    = "overflow_value"
	classDuplicateKey     = "duplicate_key"
	classBadRef           = "bad_ref"
	classBadView          = "bad_view"
	classTypeMismatch     = "type_mismatch"
	classUnknownName      = "unknown_name"
	classBudgetDepth      = "budget_depth"
	classBudgetNodes      = "budget_nodes"
	classBudgetBytes      = "budget_bytes"
	classBudgetAlloc      = "budget_alloc"
	classUnsupportedKind  = "unsupported_kind"
	classRegisterConflict = "register_conflict"
	classCoderError       = "coder_error"
	classCoderRecursion   = "coder_recursion"
	classContractMismatch = "contract_mismatch"
	classIORead           = "io_read"
)

// classSentinels is the deterministic class-to-sentinel table: data/format
// classes map to ErrFormat, budget to ErrBudget, code and contract to
// ErrUnsupported, env (io_read) to ErrIO. Error.Is answers through this
// table alone.
var classSentinels = map[string]error{
	classBadMagic:         ErrFormat,
	classTruncated:        ErrFormat,
	classMalformedOp:      ErrFormat,
	classMalformedArg:     ErrFormat,
	classOverflowValue:    ErrFormat,
	classDuplicateKey:     ErrFormat,
	classBadRef:           ErrFormat,
	classBadView:          ErrFormat,
	classTypeMismatch:     ErrFormat,
	classUnknownName:      ErrFormat,
	classBudgetDepth:      ErrBudget,
	classBudgetNodes:      ErrBudget,
	classBudgetBytes:      ErrBudget,
	classBudgetAlloc:      ErrBudget,
	classUnsupportedKind:  ErrUnsupported,
	classRegisterConflict: ErrUnsupported,
	classCoderError:       ErrUnsupported,
	classCoderRecursion:   ErrUnsupported,
	classContractMismatch: ErrUnsupported,
	classIORead:           ErrIO,
}

// Error is the structured error type returned by the codec: class (the
// stable identifier), the input offset and value path when the class
// carries input context, got/want detail values, and the cause. Texts are
// rendering, not contract: the diagnostic message may change between
// releases; the stable surface is the class ID, the sentinel family, and
// these fields.
type Error struct {
	class  string
	Offset int    // byte offset into the input; -1 when the class has no input context
	Path   string // value path; empty when the class has no path context
	Got    any    // offending value or kind, rendered boundedly
	Want   any    // expected value, kind, or limit, rendered boundedly
	err    error  // cause or detail phrase; reachable through Unwrap
	// Trusted-input diagnostics (opt-in): the input window
	// captured around Offset when the error was raised, with the flag on.
	// snippetSet distinguishes a captured-empty window from no capture;
	// the fields are unread on the default path.
	snippet    []byte
	snippetOff int
	snippetSet bool
}

// ErrorSnippet is the machine-readable form of a captured input window:
// the absolute byte offset of the window start, its length, and the window
// bytes as standard base64 (SARIF-compatible triple). The raw bytes are
// not exported; Base64 is derived at call time.
type ErrorSnippet struct {
	ByteOffset int
	ByteLength int
	Base64     string
}

// Snippet returns the input window captured around the failure offset in
// trusted-input mode. ok is true exactly when a capture ran — the window
// may legitimately be empty (the emptiness is expressed by the triple with
// ByteLength 0, not by ok). ok is false without the trust flag, or for an
// error that never carried an input offset.
func (e *Error) Snippet() (ErrorSnippet, bool) {
	if !e.snippetSet {
		return ErrorSnippet{}, false
	}
	return ErrorSnippet{
		ByteOffset: e.snippetOff,
		ByteLength: len(e.snippet),
		Base64:     base64.StdEncoding.EncodeToString(e.snippet),
	}, true
}

// LogValue renders the error as a structured slog group: class always,
// then path and offset under the same presence rules as the one-line
// text. The captured-window fields stay out of the log; the machine form
// is Snippet.
func (e *Error) LogValue() slog.Value {
	attrs := make([]slog.Attr, 1, 3)
	attrs[0] = slog.String("class", e.class)
	if e.Path != "" {
		attrs = append(attrs, slog.String("path", e.Path))
	}
	if e.Offset >= 0 {
		attrs = append(attrs, slog.Int("offset", e.Offset))
	}
	return slog.GroupValue(attrs...)
}

// Class returns the error class ID (snake_case, public contract).
func (e *Error) Class() string { return e.class }

// Unwrap returns the cause (the underlying fault, or the detail phrase for
// caller-context classes).
func (e *Error) Unwrap() error { return e.err }

// Is answers sentinel comparisons through the class-sentinel table.
func (e *Error) Is(target error) bool {
	return classSentinels[e.class] == target
}

// Error renders the one-line diagnostic: "gbon: <class>", then the path
// and offset segments when present, then the cause phrase when present.
// No input values are interpolated by the core; callers of the
// constructors keep untrusted data out of the phrase.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("gbon: ")
	b.WriteString(e.class)
	if e.Path != "" {
		b.WriteString(" at ")
		b.WriteString(e.Path)
	}
	if e.Offset >= 0 {
		b.WriteString(" (offset ")
		b.WriteString(strconv.Itoa(e.Offset))
		b.WriteString(")")
	}
	if e.err != nil {
		b.WriteString(": ")
		b.WriteString(e.err.Error())
	}
	return b.String()
}

// Format renders %v as Error does and %+v verbosely: the one-line form,
// then the structured fields (only those present) and the cause.
func (e *Error) Format(f fmt.State, verb rune) {
	if verb != 'v' || !f.Flag('+') {
		_, _ = fmt.Fprint(f, e.Error())
		return
	}
	var b strings.Builder
	b.WriteString(e.Error())
	b.WriteString("\n    class = ")
	b.WriteString(e.class)
	if e.Offset >= 0 {
		b.WriteString("\n    offset = ")
		b.WriteString(strconv.Itoa(e.Offset))
	}
	if e.Path != "" {
		b.WriteString("\n    path = ")
		b.WriteString(e.Path)
	}
	if e.Got != nil {
		b.WriteString("\n    got = ")
		b.WriteString(renderValue(e.Got))
	}
	if e.Want != nil {
		b.WriteString("\n    want = ")
		b.WriteString(renderValue(e.Want))
	}
	if e.err != nil {
		b.WriteString("\n    cause = ")
		b.WriteString(e.err.Error())
	}
	if len(e.snippet) > 0 {
		b.WriteString("\n    input =\n")
		e.dumpSnippet(&b)
	}
	_, _ = f.Write([]byte(b.String()))
}

// snippetBefore and snippetAfter bound the captured input window around
// the failure offset: 32 bytes before, 16 after, clamped to the record
// buffer at capture time.
const (
	snippetBefore = 32
	snippetAfter  = 16
)

// markedByte is the byte the dump marker points at: the last consumed
// byte (input offsets are post-cursor), clamped into the window on both
// ends so it always names a byte of the captured window.
func (e *Error) markedByte() int {
	m := max(e.Offset-1, e.snippetOff)
	if last := e.snippetOff + len(e.snippet) - 1; m > last {
		m = last
	}
	return m
}

// dumpSnippet appends the captured window as a hexdump -C listing with
// absolute offsets; the line holding the marked byte carries the GDB-style
// "=> " prefix. An empty window produces no lines at all (the caller
// omits the block header).
func (e *Error) dumpSnippet(b *strings.Builder) {
	marked := e.markedByte()
	for i := 0; i < len(e.snippet); i += 16 {
		lineOff := e.snippetOff + i
		if lineOff <= marked && marked < lineOff+16 {
			b.WriteString("=> ")
		}
		_, _ = fmt.Fprintf(b, "%08x  ", lineOff)
		for j := range 16 {
			if i+j < len(e.snippet) {
				_, _ = fmt.Fprintf(b, "%02x ", e.snippet[i+j])
			} else {
				b.WriteString("   ")
			}
			if j == 7 {
				b.WriteString(" ")
			}
		}
		b.WriteString(" |")
		for j := 0; j < 16 && i+j < len(e.snippet); j++ {
			if c := e.snippet[i+j]; unicode.IsPrint(rune(c)) {
				b.WriteByte(c)
			} else {
				b.WriteString(".")
			}
		}
		b.WriteString("|")
		if i+16 < len(e.snippet) {
			b.WriteString("\n")
		}
	}
}

// maxKeyRunes is the printable-key bound of the bounded path-key form.
const maxKeyRunes = 16

// boundedKey renders a map key as a path segment: printable keys of up to
// 16 runes in Go %q quoting; longer or non-printable keys in the bounded
// form [<key N B>] with N the rune length and B the bound marker. The
// address segment leaks at most the key's shape, never its full value.
func boundedKey(k string) string {
	rs := []rune(k)
	if len(rs) <= maxKeyRunes {
		printable := true
		for _, r := range rs {
			if !unicode.IsPrint(r) {
				printable = false
				break
			}
		}
		if printable {
			return "[" + strconv.Quote(k) + "]"
		}
	}
	var b strings.Builder
	b.WriteString("[<key ")
	b.WriteString(strconv.Itoa(len(rs)))
	b.WriteString(" B>]")
	return b.String()
}

// maxDetailRunes caps the rendering of got/want values in %+v.
const maxDetailRunes = 48

// renderValue renders a got/want value boundedly: strings and byte slices
// through the bounded key forms, scalars plainly, anything else as its %v
// rendering truncated at maxDetailRunes runes.
func renderValue(v any) string {
	switch x := v.(type) {
	case string:
		return boundedKey(x)
	case []byte:
		var b strings.Builder
		b.WriteString("[<bytes ")
		b.WriteString(strconv.Itoa(len(x)))
		b.WriteString(" B>]")
		return b.String()
	}
	s := fmt.Sprintf("%v", v)
	rs := []rune(s)
	if len(rs) > maxDetailRunes {
		rs = rs[:maxDetailRunes]
		return string(rs) + "…"
	}
	return s
}

// errDetail is a plain detail phrase carried in the cause slot.
type errDetail string

func (d errDetail) Error() string { return string(d) }

// Constructors of the core. Sites build errors only through these; the
// class, the sentinel mapping, and the rendering live here.

// errFormat builds a data/format error with input context.
func errFormat(class string, off int, path string, got, want any, cause error) *Error {
	return &Error{class: class, Offset: off, Path: path, Got: got, Want: want, err: cause}
}

// errBudget builds a budget error naming the exhausted resource.
func errBudget(class string, off int, path string, got, want any, cause error) *Error {
	return &Error{class: class, Offset: off, Path: path, Got: got, Want: want, err: cause}
}

// errUnsupported builds a code/contract error without input context
// (register conflicts, unsupported kinds, coder faults carry no offset).
func errUnsupported(class string, path string, got, want any, cause error) *Error {
	return &Error{class: class, Offset: -1, Path: path, Got: got, Want: want, err: cause}
}

// errIO wraps a failed underlying read: class io_read, offset included,
// the original fault as the cause.
func errIO(off int, cause error) *Error {
	return &Error{class: classIORead, Offset: off, err: cause}
}

// errRegister builds a registry-conflict error from the caller-facing
// detail phrase (wire names and types are caller arguments, trusted).
func errRegister(detail string) *Error {
	return &Error{class: classRegisterConflict, Offset: -1, err: errDetail(detail)}
}

// errCoder builds a coder fault (coder_error, coder_recursion) with the
// seam-assigned location; the coder's own error is the cause.
func errCoder(class string, off int, path string, cause error) *Error {
	return &Error{class: class, Offset: off, Path: path, err: cause}
}
