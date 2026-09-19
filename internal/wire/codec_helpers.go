package wire

import (
	"io"
	"reflect"
)

// Codec-facing helpers: intern-space and buffer access. These extend the
// wire layer without redefining token grammar, canonical rules, or
// opcode assignment.

// Exported first-byte class aliases for codec dispatch.
const (
	ClassNil   byte = classNil
	ClassRef   byte = classRef
	ClassView  byte = classView
	ClassArray byte = classArray
	ClassBlob  byte = classBlob
	ClassDesc  byte = classDesc
)

// entryValue marks a decoder-side object record: a reconstructed pointer
// target referenced by REF tokens (two-phase decode:
// REFs may resolve before the target is fully reconstructed).
const entryValue entryKind = 4

// entryMap marks a decoder-side map record: a reconstructed map object
// referenced by REF tokens in map-typed positions (record-then-fill,
// intern semantics).
const entryMap entryKind = 5

// ReserveID reserves the next intern-space id without emitting bytes:
// pointer-target registration before children.
func (w *Writer) ReserveID() uint64 { return w.allocID() }

// NextID returns the id that the next allocation will take (peek).
func (w *Writer) NextID() uint64 { return w.nextID }

// WriteRawBytes appends raw record-body bytes (BLOB payload positions).
func (w *Writer) WriteRawBytes(b []byte) error {
	w.buf = append(w.buf, b...)
	return nil
}

// FlushTo drains the buffered stream into iw, preserving intern state for
// subsequent records (Encoder facade streaming).
func (w *Writer) FlushTo(iw io.Writer) error {
	if len(w.buf) == 0 {
		return nil
	}
	if _, err := iw.Write(w.buf); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

// ReadRawBytes consumes n raw record-body bytes (BLOB payload positions).
func (r *Reader) ReadRawBytes(n uint64) ([]byte, error) { return r.readN(n) }

// ByteRange returns the read-only input bytes in [start, end); the bounds
// come from Pos() snapshots taken around token consumption (skip-path key
// identity, KO-8) and are absolute input offsets, translated here into the
// sliding window's coordinates.
func (r *Reader) ByteRange(start, end int) []byte { return r.buf[start-r.base : end-r.base] }

// PeekClass returns the class of the next token without consuming it.
func (r *Reader) PeekClass() (byte, error) { return r.peekFirst() }

// AtEnd reports whether the input is fully consumed (facade EOF check
// for coder sub-decode positions).
func (r *Reader) AtEnd() bool { return r.pos >= len(r.buf) }

// IsNextNil reports whether the next token is a NIL-class token.
func (r *Reader) IsNextNil() bool {
	class, err := r.peekFirst()
	return err == nil && class == classNil
}

// RegisterValue registers a reconstructed object (pointer target) in the
// intern space and returns its id; must be called before decoding children.
func (r *Reader) RegisterValue(v reflect.Value) uint64 {
	return r.allocEntry(entryValue, "", nil, v, 0)
}

// ValueAt returns the object registered for id.
func (r *Reader) ValueAt(id uint64) (reflect.Value, error) {
	kind, err := r.kindAt(id)
	if err != nil {
		return reflect.Value{}, err
	}
	if kind != entryValue {
		return reflect.Value{}, werr(kindBadRef, "wire: ref %d is not an object record", id)
	}
	return r.vals[id], nil
}

// MapAt returns the map object registered for record id. The id must
// resolve to a materialized map record; a REF to any other record sort —
// or to a skipped, never-materialized map record — is a format error.
func (r *Reader) MapAt(id uint64) (reflect.Value, error) {
	kind, err := r.kindAt(id)
	if err != nil {
		return reflect.Value{}, err
	}
	if kind != entryMap {
		return reflect.Value{}, werr(kindBadRef, "wire: ref %d is not a map record", id)
	}
	if mv := r.vals[id]; !mv.IsValid() {
		return reflect.Value{}, werr(kindBadRef, "wire: map record %d is not materialized", id)
	} else {
		return mv, nil
	}
}

// SetMapVal materializes the map object of record id (MakeMap before pairs).
func (r *Reader) SetMapVal(id uint64, m reflect.Value) {
	r.vals[id] = m
}

// RecordKind is the sort of an intern-space record, for codec-side REF
// disambiguation before token consumption.
type RecordKind uint8

// Intern-record sorts visible to the codec layer.
const (
	RecordOther  RecordKind = 0
	RecordString RecordKind = 1
	RecordDesc   RecordKind = 2
	RecordArray  RecordKind = 3
	RecordBlob   RecordKind = 4
	RecordValue  RecordKind = 5
	RecordMap    RecordKind = 6
)

// RecordAt reports the sort and stored value of record id. An
// unregistered id reads as RecordOther; the consuming read surfaces the
// precise error.
func (r *Reader) RecordAt(id uint64) (RecordKind, reflect.Value) {
	kind, err := r.kindAt(id)
	if err != nil {
		return RecordOther, reflect.Value{}
	}
	switch kind {
	case entryString:
		return RecordString, reflect.Value{}
	case entryDesc:
		return RecordDesc, reflect.Value{}
	case entryArray:
		return RecordArray, reflect.Value{}
	case entryBlob:
		return RecordBlob, reflect.Value{}
	case entryValue:
		return RecordValue, r.vals[id]
	case entryMap:
		return RecordMap, r.vals[id]
	}
	return RecordOther, reflect.Value{}
}

// PeekRef returns the id of a REF token at the current position without
// consuming anything; ok=false covers every non-REF lookahead outcome
// (the caller's regular read surfaces the error).
func (r *Reader) PeekRef() (uint64, bool) {
	if class, err := r.peekFirst(); err != nil || class != classRef {
		return 0, false
	}
	if r.pendLen == 0 {
		if r.src != nil {
			// The lookahead fills exactly the token extent — the header
			// byte plus the ARG payload width named by its form nibble —
			// so it never waits for bytes past the token itself (a live
			// non-EOF source would block forever on an over-read).
			if err := r.fill(1 + argWidth(r.buf[r.pos]&0x0F)); err != nil {
				return 0, false
			}
		}
		start := r.pos
		id, err := r.readTokenArg(classRef)
		end := r.pos
		r.pos = start
		if err != nil {
			return 0, false
		}
		r.pendStart, r.pendLen, r.peekID = start, end-start, id
	}
	return r.peekID, true
}

// peekNameCap bounds the name lookahead: descriptor names are type
// identifiers, far below this magnitude; a longer declared length is
// treated as a non-DESC lookahead outcome rather than buffered.
const peekNameCap = 1 << 16

// PeekDesc returns the kind and literal name of a DESC-literal header
// without consuming or interning; ok=false covers non-DESC lookahead
// outcomes and malformed or oversized extents; the fill is exact.
func (r *Reader) PeekDesc() (Kind, string, bool) {
	if class, err := r.peekFirst(); err != nil || class != classDesc {
		return 0, "", false
	}
	hdr := 1
	form := r.buf[r.pos] & 0x0F
	var kind Kind
	switch {
	case form <= 11:
		kind = Kind(form)
	case form == argU8:
		if r.src != nil {
			if err := r.fill(2); err != nil {
				return 0, "", false
			}
		}
		if r.pos+1 >= len(r.buf) {
			return 0, "", false
		}
		k := Kind(r.buf[r.pos+1])
		if k < 12 || k >= maxKind {
			return 0, "", false
		}
		kind, hdr = k, 2
	default:
		return 0, "", false
	}
	nx := r.pos + hdr
	if r.src != nil {
		if err := r.fill(hdr + 1); err != nil {
			return 0, "", false
		}
	}
	if nx >= len(r.buf) || r.buf[nx]>>4 != classString {
		return 0, "", false
	}
	sform := r.buf[nx] & 0x0F
	aw := argWidth(sform)
	if r.src != nil && aw > 0 {
		if err := r.fill(hdr + 1 + aw); err != nil {
			return 0, "", false
		}
	}
	var n uint64
	if sform <= byte(argInlineMax) {
		n = uint64(sform)
	} else if sform >= argU8 && sform <= argU64 {
		if nx+1+aw > len(r.buf) {
			return 0, "", false
		}
		for _, c := range r.buf[nx+1 : nx+1+aw] {
			n = n<<8 | uint64(c)
		}
	} else {
		return 0, "", false
	}
	if n > peekNameCap {
		return 0, "", false
	}
	if r.src != nil {
		if err := r.fill(hdr + 1 + aw + int(n)); err != nil {
			return 0, "", false
		}
	}
	end := nx + 1 + aw + int(n)
	if end > len(r.buf) {
		return 0, "", false
	}
	return kind, string(r.buf[nx+1+aw : end]), true
}

// PeekNilKind returns the selector of a NIL-class token at the current
// position without consuming it; ok=false on any other lookahead
// outcome, including unknown selectors.
func (r *Reader) PeekNilKind() (NilKind, bool) {
	if class, err := r.peekFirst(); err != nil || class != classNil {
		return 0, false
	}
	k := NilKind(r.buf[r.pos] & 0x0F)
	if k > NilZeroSize {
		return 0, false
	}
	return k, true
}

// NilZeroSize is the zero-size marker selector: a non-nil pointer to a
// zero-size pointee encodes as this NIL-class token (reserved selector
// 4, graduated in minor 1).
const NilZeroSize NilKind = 4

// ReadNilSelector reads a NIL token allowing the graduated selectors
// (0..3 plus the zero-size marker); other selectors stay malformed.
func (r *Reader) ReadNilSelector() (NilKind, error) {
	form, err := r.readFirst(classNil)
	if err != nil {
		return 0, err
	}
	if form > byte(NilZeroSize) {
		return 0, werr(kindMalformedOp, "wire: unknown nil selector %d", form)
	}
	return NilKind(form), nil
}

// DescAt returns the descriptor of record id.
func (r *Reader) DescAt(id uint64) (*Desc, error) {
	kind, err := r.kindAt(id)
	if err != nil {
		return nil, err
	}
	if kind != entryDesc {
		return nil, werr(kindBadRef, "wire: ref %d is not a descriptor", id)
	}
	return r.descs[id], nil
}
