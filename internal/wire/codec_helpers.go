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
// identity, KO-8).
func (r *Reader) ByteRange(start, end int) []byte { return r.buf[start:end] }

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

// PeekNilKind returns the selector of a NIL-class token at the current
// position without consuming it; ok=false on any other lookahead
// outcome, including unknown selectors.
func (r *Reader) PeekNilKind() (NilKind, bool) {
	if class, err := r.peekFirst(); err != nil || class != classNil {
		return 0, false
	}
	k := NilKind(r.buf[r.pos] & 0x0F)
	if k > NilInterface {
		return 0, false
	}
	return k, true
}
