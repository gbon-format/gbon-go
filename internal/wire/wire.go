// Package wire implements the gbon wire-format primitives: stream header,
// integer arguments (ARG), zigzag, raw float bits, value tokens, the intern
// space (strings, type descriptors, backing arrays, blobs), slice views, and
// the pre-reflection validator.
//
// The package is internal: the public API (package gbon) wires it into
// Marshal/Unmarshal/Encoder/Decoder. Errors ErrFormat and
// ErrBudget mirror the semantics of gbon.ErrFormat and gbon.ErrBudget.
package wire

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Sentinel errors of the wire layer. The codec layer maps them onto the
// package-level gbon sentinels (errors.Is) when the API is connected.
var (
	// ErrFormat reports malformed wire input: bad magic or major version,
	// non-minimal argument forms, unknown opcodes or selector forms,
	// out-of-range views, unregistered references, truncated input.
	ErrFormat = errors.New("gbon/wire: malformed input")

	// ErrBudget reports that a decoding budget (slice backing length)
	// was exhausted.
	ErrBudget = errors.New("gbon/wire: decoding budget exceeded")
)

// Class IDs carried by wire.Error — the same snake_case vocabulary the
// root error core uses (this package cannot import the root: the root
// imports wire; mapErr in the codec lifts these onto the core by kind).
const (
	kindTruncated        = "truncated"
	kindMalformedOp      = "malformed_op"
	kindMalformedArg     = "malformed_arg"
	kindBadMagic         = "bad_magic"
	kindBadRef           = "bad_ref"
	kindBadView          = "bad_view"
	kindBudgetDepth      = "budget_depth"
	kindBudgetNodes      = "budget_nodes"
	kindBudgetBytes      = "budget_bytes"
	kindRegisterConflict = "register_conflict"
)

// Error is the wire layer's structured error: Kind carries the class ID,
// Off the input offset when the site knows it, Got/Want bounded detail
// values (untrusted input never renders into Msg), Msg the diagnostic
// phrase. Is answers the package sentinels by family so errors.Is
// assertions on ErrFormat/ErrBudget keep holding inside the package.
type Error struct {
	Kind string
	Off  int
	Got  any
	Want any
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Is(target error) bool {
	if strings.HasPrefix(e.Kind, "budget_") {
		return target == ErrBudget
	}
	return target == ErrFormat
}

// werr builds a wire error without an input offset.
func werr(kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Off: -1, Msg: fmt.Sprintf(format, args...)}
}

// werrAt builds a wire error carrying its input offset.
func werrAt(off int, kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Off: off, Msg: fmt.Sprintf(format, args...)}
}

// Stream header.
const (
	// Magic is the 4-byte stream magic: hex 67 62 6F 6E, ASCII "gbon".
	Magic = "gbon"
	// Major is the format major version (breaking changes).
	// Major 0 is the draft era of the format.
	Major uint8 = 0
	// Minor is the format minor version (additive changes).
	Minor uint8 = 0
)

// First-byte classes: the high nibble of the first token byte.
const (
	classNil     byte = 0x0
	classBool    byte = 0x1
	classInt     byte = 0x2
	classUint    byte = 0x3
	classFloat   byte = 0x4
	classComplex byte = 0x5
	classString  byte = 0x6
	classBlob    byte = 0x7
	classArray   byte = 0x8
	classView    byte = 0x9
	classMap     byte = 0xA
	classStruct  byte = 0xB
	classRef     byte = 0xC
	classDesc    byte = 0xD
	classEscExp  byte = 0xE
	classEscPriv byte = 0xF
)

// NilKind is the selector value of a NIL-class token: the low
// nibble of the first byte.
type NilKind uint8

// Nil token selectors.
const (
	NilPointer   NilKind = 0
	NilSlice     NilKind = 1
	NilMap       NilKind = 2
	NilInterface NilKind = 3
)

// entryKind distinguishes intern-space record sorts.
type entryKind uint8

const (
	entryString entryKind = iota
	entryDesc
	entryArray
	entryBlob
)

// Writer encodes token-level records into a byte stream. Interning (strings,
// descriptors) is per Writer; identity keys for arrays, blobs, and pointer
// targets are a codec-layer concern.
type Writer struct {
	buf    []byte
	strs   map[string]uint64
	descs  map[string]descClaim
	nextID uint64
}

// NewWriter returns a Writer with empty intern tables.
func NewWriter() *Writer {
	return &Writer{
		strs:  make(map[string]uint64),
		descs: make(map[string]descClaim),
	}
}

// Bytes returns the bytes encoded so far.
func (w *Writer) Bytes() []byte { return w.buf }

// maxPooledCap bounds the buffer capacity a Reset retains: larger buffers
// are dropped so reuse cannot pin outsized memory.
const maxPooledCap = 4 << 20

// Reset returns the Writer to its empty state — intern tables cleared, id
// counter zeroed, buffer retained up to maxPooledCap — for reuse by a
// subsequent self-contained stream. Token output is unaffected.
func (w *Writer) Reset() {
	clear(w.strs)
	clear(w.descs)
	w.nextID = 0
	if cap(w.buf) > maxPooledCap {
		w.buf = nil
	} else {
		w.buf = w.buf[:0]
	}
}

// Grow reserves capacity for n more bytes, amortizing the append-growth
// copies of large record writes. The reservation is a capacity hint only:
// no bytes are appended and token output is unchanged.
func (w *Writer) Grow(n int) {
	if cap(w.buf)-len(w.buf) < n {
		buf := make([]byte, len(w.buf), len(w.buf)+n)
		copy(buf, w.buf)
		w.buf = buf
	}
}

// allocID reserves the next intern-space id in first-encounter order.
func (w *Writer) allocID() uint64 {
	id := w.nextID
	w.nextID++
	return id
}

// Reader decodes token-level records from a byte stream, maintaining the
// intern-space table needed to resolve REF tokens and validate views.
//
// A Reader works in two modes. In buffer mode (NewReader) the whole input
// is one caller-owned slice. In stream mode (NewStreamReader) the Reader
// pulls bytes from an io.Reader on demand into a sliding window, so a
// stream of records decodes without materializing the whole input: the
// window holds at most one in-flight record (plus unread lookahead), and
// consuming a record boundary releases its bytes. Large reads bypass the
// window entirely.
type Reader struct {
	buf []byte
	pos int
	// Intern space indexed by record id: kinds is
	// authoritative for the id count and lens is lockstep with it; the
	// sort-specific arrays (strs, descs, vals) grow lazily up to the last
	// record of their sort, so a stream that never uses a sort never
	// allocates its array. vals holds reflect.Value directly, without
	// interface boxing.
	kinds []entryKind
	strs  []string
	descs []*Desc
	vals  []reflect.Value
	lens  []uint64
	// src non-nil selects stream mode: fill pulls from it into buf.
	src io.Reader
	// eof reports that src is exhausted (stream mode).
	eof bool
	// faultErr caches a non-EOF source fault seen while filling (stream
	// mode); pulls after the fault resurface it instead of reading the
	// fault position as end-of-stream.
	faultErr error
	// base is the absolute stream offset of buf[0] (stream mode): consumed
	// bytes are dropped from the window front by compact.
	base int
	// MaxSliceLen bounds backing length L; 0 disables the budget
	// at token level; the codec layer applies conservative defaults.
	MaxSliceLen uint64
	// MaxDescDepth bounds descriptor-literal parsing recursion;
	// 0 disables the guard at token level. Set by the codec layer from its
	// own non-configurable const, mirroring MaxSliceLen.
	MaxDescDepth int
	// MaxDescNodes bounds the materialized descriptor graph — one node per
	// descriptor literal plus one per struct field; 0 disables the guard at
	// token level. Set by the codec layer from MaxNodes.
	MaxDescNodes int
	descDepth    int
	descNodes    int
}

// NewReader returns a Reader over b; token-level budgets default to
// disabled (MaxSliceLen, MaxDescDepth).
func NewReader(b []byte) *Reader {
	return &Reader{buf: b}
}

// largeRead is the threshold above which stream-mode reads bypass the
// sliding window: a fresh buffer serves the read directly, so a record
// larger than the window never inflates steady-state memory.
const largeRead = 1 << 20

// fillChunk is the streaming pull granularity: small enough to keep the
// window tight around the current record, large enough to amortize the
// per-Read cost of slow sources.
const fillChunk = 1 << 16

// Direct-read growth bounds: the serving buffer of a large stream-mode
// read starts at readDirectInit spare capacity and grows amortized in
// steps capped at readDirectMaxGrow; the declared length of the record
// never sizes the allocation.
const (
	readDirectInit    = 1 << 16
	readDirectMaxGrow = 1 << 20
)

// NewStreamReader returns a Reader pulling tokens from src on demand.
// src is consumed strictly sequentially; the intern space (entries)
// persists across the whole stream.
func NewStreamReader(src io.Reader) *Reader {
	return &Reader{src: src}
}

// fill pulls from src until at least n bytes are available past pos or the
// source is exhausted. A non-EOF source fault is cached in faultErr so
// subsequent pulls resurface it deterministically (io.Reader makes no promise
// that a fault repeats).
func (r *Reader) fill(n int) error {
	if r.faultErr != nil {
		return r.faultErr
	}
	for len(r.buf)-r.pos < n && !r.eof {
		if r.pos >= largeRead {
			r.compact()
		}
		need := max(n-(len(r.buf)-r.pos), fillChunk)
		if cap(r.buf)-len(r.buf) < need {
			grown := make([]byte, len(r.buf), len(r.buf)+need)
			copy(grown, r.buf)
			r.buf = grown
		}
		nr, err := r.src.Read(r.buf[len(r.buf):cap(r.buf)])
		r.buf = r.buf[:len(r.buf)+nr]
		if err != nil {
			if err == io.EOF {
				r.eof = true
				break
			}
			r.faultErr = err
			return err
		}
	}
	return nil
}

// compact drops the consumed prefix of the window.
func (r *Reader) compact() {
	r.base += r.pos
	r.buf = r.buf[r.pos:]
	if len(r.buf) == 0 {
		r.buf = nil
	}
	r.pos = 0
}

// Discard releases the consumed prefix of the window. Calling it at record
// boundaries keeps stream-mode memory proportional to one record.
func (r *Reader) Discard() {
	if r.src != nil && r.pos > 0 {
		r.compact()
	}
}

// AtEOF reports whether the stream is fully consumed at the current
// position without consuming anything: a record-boundary end-of-stream
// probe. In buffer mode it is the end of the slice. A cached source
// fault is not end-of-stream: the probe answers false so the next pull
// surfaces the fault through the error path.
func (r *Reader) AtEOF() bool {
	if r.pos < len(r.buf) {
		return false
	}
	if r.src == nil || r.eof {
		return true
	}
	if err := r.fill(1); err != nil {
		return false
	}
	return r.pos >= len(r.buf)
}

// Pos returns the current read offset into the input.
func (r *Reader) Pos() int { return r.pos }

// Window returns the read-only window of the current sliding buffer
// around the absolute offset off: [max(base, off-before), min(base+len,
// off+after)), clamped to the already-buffered bytes, together with the
// window's absolute start. It never reads from the source and never
// mutates reader state; a clamped-empty window returns a nil slice with
// the clamped start.
func (r *Reader) Window(off, before, after int) ([]byte, int) {
	lo := r.base
	hi := r.base + len(r.buf)
	s := max(off-before, lo)
	e := min(off+after, hi)
	if e <= s {
		return nil, s
	}
	return r.buf[s-r.base : e-r.base], s
}

// internInitCap is the initial capacity of the intern-space arrays: the
// first record of a stream allocates once at this size instead of
// climbing the 1→2→4 realloc chain, while larger streams keep the usual
// amortized append growth.
const internInitCap = 8

// allocEntry registers a new intern-space record and returns its id.
// kinds and lens append in lockstep (one cell per record); each
// sort-specific array grows only on a record of its own sort, so a flat
// stream without a sort skips its allocation entirely.
func (r *Reader) allocEntry(kind entryKind, str string, desc *Desc, val reflect.Value, L uint64) uint64 {
	if r.kinds == nil {
		r.kinds = make([]entryKind, 0, internInitCap)
		r.lens = make([]uint64, 0, internInitCap)
	}
	id := len(r.kinds)
	r.kinds = append(r.kinds, kind)
	r.lens = append(r.lens, L)
	switch kind {
	case entryString:
		r.strs = extendTo(r.strs, id, str)
	case entryDesc:
		r.descs = extendTo(r.descs, id, desc)
	case entryValue, entryMap:
		r.vals = extendTo(r.vals, id, val)
	}
	return uint64(id)
}

// extendTo grows s to hold index n and sets s[n] = x, allocating the
// array on the first record of its sort. Sparse id gaps grow in one step
// (exact power-of-two capacity) instead of a realloc chain; indices
// owned by other sorts read back as the zero value. Readers reach a
// sort's array only through a kindAt guard, which guarantees len(s) > n
// for registered ids.
func extendTo[T any](s []T, n int, x T) []T {
	if len(s) > n {
		s[n] = x
		return s
	}
	if cap(s) <= n {
		c := internInitCap
		for c < n+1 {
			c *= 2
		}
		grown := make([]T, n+1, c)
		copy(grown, s)
		s = grown
	} else {
		var zero T
		old := len(s)
		s = s[:n+1]
		for i := old; i < n; i++ {
			s[i] = zero
		}
	}
	s[n] = x
	return s
}

// kindAt returns the sort of the intern-space record for id, or ErrFormat
// for an unregistered id.
func (r *Reader) kindAt(id uint64) (entryKind, error) {
	if id >= uint64(len(r.kinds)) {
		return 0, werr(kindBadRef, "wire: ref to unregistered id %d", id)
	}
	return r.kinds[id], nil
}

// byteAt consumes one byte; running past the end is malformed input
// (self-delimiting grammar. In stream mode the pull
// happens before the end-of-input verdict.
func (r *Reader) byteAt() (byte, error) {
	if r.pos >= len(r.buf) && r.src != nil {
		if err := r.fill(1); err != nil {
			return 0, err
		}
	}
	if r.pos >= len(r.buf) {
		return 0, werrAt(int(r.base+r.pos), kindTruncated, "wire: truncated input")
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

// readN consumes n raw body bytes. The length is validated against the
// available input before any allocation; in stream mode
// availability means the source too, and reads above the window threshold
// are served by a dedicated buffer so one large record never lodges in the
// sliding window.
func (r *Reader) readN(n uint64) ([]byte, error) {
	if n > uint64(len(r.buf)-r.pos) {
		if r.src == nil {
			return nil, werrAt(int(r.base+r.pos), kindTruncated, "wire: truncated body (need %d bytes)", n)
		}
		if n > largeRead {
			return r.readDirect(n)
		}
		if err := r.fill(int(n)); err != nil {
			return nil, err
		}
		if n > uint64(len(r.buf)-r.pos) {
			return nil, werrAt(int(r.base+r.pos), kindTruncated, "wire: truncated body (need %d bytes)", n)
		}
	}
	b := r.buf[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return b, nil
}

// readDirect serves a large stream-mode read outside the window: pending
// unconsumed bytes move to the head of a fresh buffer and the rest comes
// straight from the source. The buffer grows amortized as bytes actually
// arrive; the declared length never sizes an allocation, so a truncated
// source costs a transient proportional to the bytes it really delivered.
func (r *Reader) readDirect(n uint64) ([]byte, error) {
	pending := len(r.buf) - r.pos
	out := make([]byte, pending, pending+readDirectInit)
	copy(out, r.buf[r.pos:])
	r.base += r.pos + pending
	r.buf = nil
	r.pos = 0
	for uint64(len(out)) < n {
		if len(out) == cap(out) {
			grow := min(cap(out), readDirectMaxGrow)
			if uint64(grow) > n-uint64(len(out)) {
				grow = int(n - uint64(len(out)))
			}
			grown := make([]byte, len(out), cap(out)+grow)
			copy(grown, out)
			out = grown
		}
		nr, err := r.src.Read(out[len(out):cap(out)])
		out = out[:len(out)+nr]
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, werrAt(int(r.base), kindTruncated, "wire: truncated body (need %d bytes)", n)
			}
			return nil, err
		}
	}
	return out, nil
}

// WriteHeader writes the stream header: magic, major, minor.
func (w *Writer) WriteHeader() error {
	w.buf = append(w.buf, Magic...)
	w.buf = append(w.buf, Major, Minor)
	return nil
}

// ReadHeader reads and validates the stream header. Mismatched magic or an
// unknown major version yield ErrFormat; an unknown minor is accepted
// (additive-only versioning).
func (r *Reader) ReadHeader() (major, minor uint8, err error) {
	hdr, err := r.readN(6)
	if err != nil {
		return 0, 0, err
	}
	if string(hdr[:4]) != Magic {
		return 0, 0, &Error{Kind: kindBadMagic, Off: -1,
			Got: append([]byte(nil), hdr[:4]...),
			Msg: "wire: bad magic"}
	}
	major, minor = hdr[4], hdr[5]
	if major != Major {
		return 0, 0, werr(kindMalformedOp, "wire: unknown major version %d", major)
	}
	return major, minor, nil
}
