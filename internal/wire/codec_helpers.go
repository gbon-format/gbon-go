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

// DescAt returns the descriptor registered for id; a REF to any other
// record sort is a format error.
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
