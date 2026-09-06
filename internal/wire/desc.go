package wire

import (
	"math"
	"reflect"
)

// Kind is a type-descriptor kind; 16 kinds, 16..255 reserved.
type Kind uint8

// Descriptor kinds.
const (
	KindStruct    Kind = 0
	KindSlice     Kind = 1
	KindArray     Kind = 2
	KindMap       Kind = 3
	KindNamed     Kind = 4
	KindPointer   Kind = 5
	KindInterface Kind = 6
	KindBool      Kind = 7
	KindInt       Kind = 8
	KindUint      Kind = 9
	KindFloat     Kind = 10
	KindComplex   Kind = 11
	KindString    Kind = 12
	KindBlob      Kind = 13
	KindCoder     Kind = 14
	KindBigint    Kind = 15
)

// maxKind is the first reserved kind value.
const maxKind = 16

// Field is a struct-descriptor field: interned name plus a type-ref
// position. Idx is the Go struct field index for
// encode-side Field(idx) access — derived state, never serialized.
type Field struct {
	Name string
	Type *Desc
	Idx  int
}

// Desc is a type descriptor. Bodies by kind: Struct → Fields; Slice,
// Pointer, Named → Refs[0]; Array → Len + Refs[0]; Map → Refs[0]=key,
// Refs[1]=elem; Int/Uint → Width ∈ {1,2,4,8}; Float → Width ∈ {4,8,16}
// (16 ↔ form 2, decimal128); Complex → Width ∈ {4,8}; Coder → Tag (per-stream
// coder tag); Bool, Interface, String, Blob, Bigint →
// empty.
type Desc struct {
	Kind   Kind
	Name   string
	Width  uint64
	Len    uint64
	Tag    uint64
	Fields []Field
	Refs   []*Desc
}

// descClaim is one interned descriptor: its id plus the descriptor
// itself, so a subsequent name-hit can be verified against the claimed
// structure (same name + different structure = ambiguous type names,
// a loud encode reject).
type descClaim struct {
	id uint64
	d  *Desc
}

// WriteDesc writes a type-ref position: REF on repeated encounter, DESC
// literal on first. The descriptor is registered in the
// intern space before its children, so recursive types close through REF
// tokens (descriptor cycles).
func (w *Writer) WriteDesc(d *Desc) error {
	if claim, ok := w.descs[d.Name]; ok {
		if !descEqual(claim.d, d) {
			return &Error{Kind: kindRegisterConflict, Off: -1,
				Got: d.Name,
				Msg: "gbon/wire: descriptor name rebound to a structurally different type: distinct types sharing one name are ambiguous (qualify the module path)"}
		}
		return w.WriteRef(claim.id)
	}
	if d.Kind >= maxKind {
		return werr(kindMalformedOp, "gbon/wire: descriptor kind %d is reserved", d.Kind)
	}
	w.descs[d.Name] = descClaim{id: w.allocID(), d: d}
	// Kind ARG: kinds 0..11 ride inline in the first byte; 12..15 use the
	// u8 form (minimal-length).
	if d.Kind <= 11 {
		w.buf = append(w.buf, classDesc<<4|byte(d.Kind))
	} else {
		w.buf = append(w.buf, classDesc<<4|argU8, byte(d.Kind))
	}
	if err := w.WriteString(d.Name); err != nil {
		return err
	}
	switch d.Kind {
	case KindStruct:
		w.writeArg(uint64(len(d.Fields)))
		for _, f := range d.Fields {
			if err := w.WriteString(f.Name); err != nil {
				return err
			}
			if err := w.WriteDesc(f.Type); err != nil {
				return err
			}
		}
	case KindSlice, KindPointer, KindNamed:
		if len(d.Refs) != 1 {
			return werr(kindMalformedOp, "gbon/wire: kind %d needs exactly 1 type-ref", d.Kind)
		}
		return w.WriteDesc(d.Refs[0])
	case KindArray:
		if len(d.Refs) != 1 {
			return werr(kindMalformedOp, "gbon/wire: kind %d needs exactly 1 type-ref", d.Kind)
		}
		w.writeArg(d.Len)
		return w.WriteDesc(d.Refs[0])
	case KindMap:
		if len(d.Refs) != 2 {
			return werr(kindMalformedOp, "gbon/wire: kind %d needs exactly 2 type-refs", d.Kind)
		}
		if err := w.WriteDesc(d.Refs[0]); err != nil {
			return err
		}
		return w.WriteDesc(d.Refs[1])
	case KindInt, KindUint:
		if err := w.checkWidth(d.Width, 1); err != nil {
			return err
		}
		w.writeArg(d.Width)
	case KindFloat:
		if err := w.checkFloatWidth(d.Width); err != nil {
			return err
		}
		w.writeArg(d.Width)
	case KindComplex:
		if err := w.checkWidth(d.Width, 4); err != nil {
			return err
		}
		w.writeArg(d.Width)
	case KindCoder:
		w.writeArg(d.Tag)
	case KindBool, KindInterface, KindString, KindBlob, KindBigint:
		// empty body
	default:
		return werr(kindMalformedOp, "gbon/wire: descriptor kind %d is reserved", d.Kind)
	}
	return nil
}

// descEqual is the structural identity of two descriptors: name, kind,
// width, length, coder tag, field names and order, and nested type-refs,
// compared co-inductively so recursive type graphs terminate (a back-edge
// pair is assumed equal — both sides describe real Go types, and a
// mismatch on any forward path rejects first).
func descEqual(a, b *Desc) bool {
	return descEq(a, b, make(map[[2]*Desc]bool))
}

func descEq(a, b *Desc, seen map[[2]*Desc]bool) bool {
	if a == b {
		return true
	}
	key := [2]*Desc{a, b}
	if seen[key] {
		return true
	}
	seen[key] = true
	if a.Kind != b.Kind || a.Name != b.Name || a.Width != b.Width ||
		a.Len != b.Len || a.Tag != b.Tag ||
		len(a.Refs) != len(b.Refs) || len(a.Fields) != len(b.Fields) {
		return false
	}
	for i := range a.Refs {
		if !descEq(a.Refs[i], b.Refs[i], seen) {
			return false
		}
	}
	for i := range a.Fields {
		if a.Fields[i].Name != b.Fields[i].Name ||
			!descEq(a.Fields[i].Type, b.Fields[i].Type, seen) {
			return false
		}
	}
	return true
}

// checkWidth validates numeric descriptor widths: minimum min and powers
// of two only.
func (w *Writer) checkWidth(width, min uint64) error {
	switch width {
	case 1, 2, 4, 8:
		if width < min {
			return werr(kindMalformedOp, "gbon/wire: width %d invalid for kind", width)
		}
		return nil
	default:
		return werr(kindMalformedOp, "gbon/wire: width %d invalid", width)
	}
}

// ReadDesc reads a type-ref position: DESC literal or REF to a record
// interned descriptor. Literals are registered before their name
// and body, closing recursive type graphs.
func (r *Reader) ReadDesc() (*Desc, error) {
	class, err := r.peekFirst()
	if err != nil {
		return nil, err
	}
	switch class {
	case classDesc:
		return r.readDescLit()
	case classRef:
		id, err := r.ReadRef()
		if err != nil {
			return nil, err
		}
		kind, err := r.kindAt(id)
		if err != nil {
			return nil, err
		}
		if kind != entryDesc {
			return nil, werr(kindBadRef, "wire: ref %d is not a descriptor", id)
		}
		return r.descs[id], nil
	default:
		return nil, werr(kindMalformedOp, "wire: expected type-ref position, class 0x%X", class)
	}
}

// readDescLit reads a DESC literal: kind ARG, interned name, kind body.
// Recursion depth is capped by MaxDescDepth when set; the materialized
// graph is charged against MaxDescNodes — a node per literal and per
// struct field — before the fields materialize.
func (r *Reader) readDescLit() (*Desc, error) {
	r.descDepth++
	defer func() { r.descDepth-- }()
	if r.MaxDescDepth != 0 && r.descDepth > r.MaxDescDepth {
		return nil, werr(kindBudgetDepth, "wire: descriptor nesting exceeds depth budget %d", r.MaxDescDepth)
	}
	if err := r.chargeDescNode(); err != nil {
		return nil, err
	}
	form, err := r.readFirst(classDesc)
	if err != nil {
		return nil, err
	}
	var kind Kind
	switch {
	case form <= 11:
		kind = Kind(form)
	case form == argU8:
		v, err := r.argPayload(form)
		if err != nil {
			return nil, err
		}
		if v < 12 {
			return nil, werr(kindMalformedArg, "wire: non-minimal kind argument %d", v)
		}
		kind = Kind(v)
	default:
		return nil, werr(kindMalformedOp, "wire: unknown kind form 0x%X", form)
	}
	if kind >= maxKind {
		return nil, werr(kindMalformedOp, "wire: reserved kind %d", kind)
	}
	d := &Desc{Kind: kind}
	r.allocEntry(entryDesc, "", d, reflect.Value{}, 0)
	if d.Name, err = r.ReadString(); err != nil {
		return nil, err
	}
	switch kind {
	case KindStruct:
		n, err := r.ReadArg()
		if err != nil {
			return nil, err
		}
		if err := r.chargeDescNodes(n); err != nil {
			return nil, err
		}
		d.Fields = make([]Field, 0, min(n, 64))
		for range n {
			var f Field
			if f.Name, err = r.ReadString(); err != nil {
				return nil, err
			}
			if f.Type, err = r.ReadDesc(); err != nil {
				return nil, err
			}
			d.Fields = append(d.Fields, f)
		}
	case KindSlice, KindPointer, KindNamed:
		ref, err := r.ReadDesc()
		if err != nil {
			return nil, err
		}
		d.Refs = []*Desc{ref}
	case KindArray:
		L, err := r.ReadArg()
		if err != nil {
			return nil, err
		}
		ref, err := r.ReadDesc()
		if err != nil {
			return nil, err
		}
		d.Len, d.Refs = L, []*Desc{ref}
	case KindMap:
		key, err := r.ReadDesc()
		if err != nil {
			return nil, err
		}
		elem, err := r.ReadDesc()
		if err != nil {
			return nil, err
		}
		d.Refs = []*Desc{key, elem}
	case KindInt, KindUint:
		width, err := r.ReadArg()
		if err != nil {
			return nil, err
		}
		if err := checkDescWidth(width, 1); err != nil {
			return nil, err
		}
		d.Width = width
	case KindCoder:
		tag, err := r.ReadArg()
		if err != nil {
			return nil, err
		}
		d.Tag = tag
	case KindFloat:
		width, err := r.ReadArg()
		if err != nil {
			return nil, err
		}
		if err := checkDescFloatWidth(width); err != nil {
			return nil, err
		}
		d.Width = width
	case KindComplex:
		width, err := r.ReadArg()
		if err != nil {
			return nil, err
		}
		if err := checkDescWidth(width, 4); err != nil {
			return nil, err
		}
		d.Width = width
	case KindBool, KindInterface, KindString, KindBlob, KindBigint:
		// empty body
	}
	return d, nil
}

// chargeDescNode books one materialized descriptor-graph node (a literal)
// against MaxDescNodes; 0 disables the guard.
func (r *Reader) chargeDescNode() error {
	r.descNodes++
	if r.MaxDescNodes != 0 && r.descNodes > r.MaxDescNodes {
		return werr(kindBudgetNodes, "wire: descriptor graph exceeds node budget %d", r.MaxDescNodes)
	}
	return nil
}

// chargeDescNodes books n struct fields against MaxDescNodes before the
// Field slice grows — the derived materialization of a flat STRUCT
// descriptor amplifies its input bytes unless charged at the production
// point.
func (r *Reader) chargeDescNodes(n uint64) error {
	if n > uint64(math.MaxInt) {
		return werr(kindBudgetNodes, "wire: struct field count %d exceeds addressable nodes", n)
	}
	r.descNodes += int(n)
	if r.MaxDescNodes != 0 && r.descNodes > r.MaxDescNodes {
		return werr(kindBudgetNodes, "wire: descriptor graph exceeds node budget %d", r.MaxDescNodes)
	}
	return nil
}

// checkFloatWidth validates FLOAT descriptor widths: {4,8,16} — 16 is the
// decimal128 form 2.
func (w *Writer) checkFloatWidth(width uint64) error {
	switch width {
	case 4, 8, 16:
		return nil
	default:
		return werr(kindMalformedOp, "gbon/wire: float width %d invalid", width)
	}
}

// checkDescFloatWidth validates decoded FLOAT widths: {4,8,16}.
func checkDescFloatWidth(width uint64) error {
	switch width {
	case 4, 8, 16:
		return nil
	default:
		return werr(kindMalformedOp, "wire: float width %d invalid", width)
	}
}

// checkDescWidth validates decoded widths: {1,2,4,8} with the kind minimum.
func checkDescWidth(width, min uint64) error {
	switch width {
	case 1, 2, 4, 8:
		if width < min {
			return werr(kindMalformedOp, "wire: width %d below minimum %d", width, min)
		}
		return nil
	default:
		return werr(kindMalformedOp, "wire: width %d invalid", width)
	}
}
