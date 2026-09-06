package wire

import (
	"reflect"
)

// View is a decoded slice-view token: a window over a backing record.
// Form 0 implies Off=0, Len=Cap=L; form 1 implies Cap=Len.
type View struct {
	ID  uint64
	Off uint64
	Len uint64
	Cap uint64
}

// WriteBlobHeader writes a BLOB record header: ARG L (backing length),
// ARG E (dense prefix), followed by E raw bytes written by the caller.
// The blob is registered in the intern space before its body
// (registration before children).
func (w *Writer) WriteBlobHeader(L, E uint64) error {
	if E > L {
		return werr(kindMalformedOp, "gbon/wire: blob dense prefix E=%d exceeds L=%d", E, L)
	}
	w.allocID()
	w.writeTokenArg(classBlob, L)
	w.writeArg(E)
	return nil
}

// ReadBlobHeader reads a BLOB record header, validates E ≤ L and the
// slice budget against L, registers the record, and
// returns its id.
func (r *Reader) ReadBlobHeader() (L, E, id uint64, err error) {
	L, err = r.readTokenArg(classBlob)
	if err != nil {
		return 0, 0, 0, err
	}
	E, err = r.ReadArg()
	if err != nil {
		return 0, 0, 0, err
	}
	if E > L {
		return 0, 0, 0, werr(kindMalformedOp, "wire: blob E=%d exceeds L=%d", E, L)
	}
	if err := r.checkBudget(L); err != nil {
		return 0, 0, 0, err
	}
	id = r.allocEntry(entryBlob, "", nil, reflect.Value{}, L)
	return L, E, id, nil
}

// WriteArrayHeader writes an ARRAY record header: ARG L (backing length),
// ARG E (dense prefix), followed by E element tokens written by the caller
// (dense prefix; elements at [E, L) are implicit zeros). The array is registered
// before its elements.
func (w *Writer) WriteArrayHeader(L, E uint64) error {
	if E > L {
		return werr(kindMalformedOp, "gbon/wire: array dense prefix E=%d exceeds L=%d", E, L)
	}
	w.allocID()
	w.writeTokenArg(classArray, L)
	w.writeArg(E)
	return nil
}

// ReadArrayHeader reads an ARRAY record header, validates E ≤ L and the
// slice budget, registers the record, and returns its id.
func (r *Reader) ReadArrayHeader() (L, E, id uint64, err error) {
	L, err = r.readTokenArg(classArray)
	if err != nil {
		return 0, 0, 0, err
	}
	E, err = r.ReadArg()
	if err != nil {
		return 0, 0, 0, err
	}
	if E > L {
		return 0, 0, 0, werr(kindMalformedOp, "wire: array E=%d exceeds L=%d", E, L)
	}
	if err := r.checkBudget(L); err != nil {
		return 0, 0, 0, err
	}
	id = r.allocEntry(entryArray, "", nil, reflect.Value{}, L)
	return L, E, id, nil
}

// checkBudget applies MaxSliceLen to the backing length L.
func (r *Reader) checkBudget(L uint64) error {
	if r.MaxSliceLen != 0 && L > r.MaxSliceLen {
		return werr(kindBudgetBytes, "wire: backing length %d exceeds budget %d", L, r.MaxSliceLen)
	}
	return nil
}

// WriteViewFull writes a form-0 view token: {id}, implied Off=0, Len=Cap=L
// (dense-prefix fast-path).
func (w *Writer) WriteViewFull(id uint64) error {
	w.buf = append(w.buf, classView<<4) // form 0
	w.writeArg(id)
	return nil
}

// WriteView writes a form-1 view token: {id, off, len}, implied cap=len.
func (w *Writer) WriteView(id, off, length uint64) error {
	w.buf = append(w.buf, classView<<4|1)
	w.writeArg(id)
	w.writeArg(off)
	w.writeArg(length)
	return nil
}

// WriteView3 writes a form-2 view token: {id, off, len, cap}.
func (w *Writer) WriteView3(id, off, length, capacity uint64) error {
	w.buf = append(w.buf, classView<<4|2)
	w.writeArg(id)
	w.writeArg(off)
	w.writeArg(length)
	w.writeArg(capacity)
	return nil
}

// ReadView reads a view token of any form and pre-validates {off, len, cap}
// against the backing length L of the referenced record — before any
// reflect use or allocation. The referenced record must be an
// array or blob.
func (r *Reader) ReadView() (View, error) {
	form, err := r.readFirst(classView)
	if err != nil {
		return View{}, err
	}
	switch form {
	case 0, 1, 2:
	default:
		return View{}, werr(kindBadView, "wire: unknown view form %d", form)
	}
	id, err := r.ReadArg()
	if err != nil {
		return View{}, err
	}
	kind, err := r.kindAt(id)
	if err != nil {
		return View{}, err
	}
	if kind != entryArray && kind != entryBlob {
		return View{}, werr(kindBadRef, "wire: view id %d is not a backing record", id)
	}
	L := r.lens[id]
	v := View{ID: id, Off: 0, Len: L, Cap: L}
	if form >= 1 {
		if v.Off, err = r.ReadArg(); err != nil {
			return View{}, err
		}
		if v.Len, err = r.ReadArg(); err != nil {
			return View{}, err
		}
		v.Cap = v.Len
	}
	if form == 2 {
		if v.Cap, err = r.ReadArg(); err != nil {
			return View{}, err
		}
	}
	if err := validateView(v, L); err != nil {
		return View{}, err
	}
	// View-form minimality: the token MUST use the
	// smallest form that carries the geometry. Form 1 with the form-0
	// geometry (off=0, len=cap=L) and form 2 with cap=len (form-1
	// geometry, which includes the form-0 case) are non-canonical →
	// ErrFormat: there is exactly one legal byte sequence per view.
	switch {
	case form == 1 && v.Off == 0 && v.Len == L:
		return View{}, werr(kindBadView, "wire: view form 1 carries form-0 geometry (off=0, len=cap=L=%d)", L)
	case form == 2 && v.Cap == v.Len:
		return View{}, werr(kindBadView, "wire: view form 2 carries form-1 geometry (cap=len)")
	}
	return v, nil
}

// validateView checks the record invariant 0 ≤ off ≤ off+len ≤ off+cap ≤ L
// with overflow detection, before any reflect operation.
func validateView(v View, L uint64) error {
	if v.Off+v.Len < v.Off {
		return werr(kindBadView, "wire: view off+len overflow")
	}
	if v.Off+v.Cap < v.Off {
		return werr(kindBadView, "wire: view off+cap overflow")
	}
	if v.Off+v.Len > v.Off+v.Cap {
		return werr(kindBadView, "wire: view len %d exceeds cap %d", v.Len, v.Cap)
	}
	if v.Off+v.Cap > L {
		return werr(kindBadView, "wire: view off+cap=%d exceeds backing L=%d", v.Off+v.Cap, L)
	}
	return nil
}
