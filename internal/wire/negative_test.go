package wire

import (
	"errors"
	"testing"
)

// Negative vectors and reserved-selector rejections.
// Every malformed input must yield ErrFormat/ErrBudget — never a panic and
// never a silent success (decoder hygiene).

func isErrFormat(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want ErrFormat, got nil", what)
	}
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("%s: want ErrFormat, got %v", what, err)
	}
}

func isErrBudget(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want ErrBudget, got nil", what)
	}
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("%s: want ErrBudget, got %v", what, err)
	}
}

// TestMalformedNoPanic: the nine negative vectors.
func TestMalformedNoPanic(t *testing.T) {
	t.Run("nonmin-int", func(t *testing.T) {
		r := NewReader([]byte{0x2C, 0x00})
		_, err := r.ReadInt()
		isErrFormat(t, "nonmin-int (zz=0 in u8 form)", err)
	})
	t.Run("bad-magic", func(t *testing.T) {
		r := NewReader([]byte{0x00, 0x00, 0x00, 0x00, 0x01, 0x00})
		_, _, err := r.ReadHeader()
		isErrFormat(t, "bad-magic", err)
	})
	t.Run("unknown-major", func(t *testing.T) {
		r := NewReader([]byte{0x67, 0x62, 0x6F, 0x6E, 0x02, 0x00})
		_, _, err := r.ReadHeader()
		isErrFormat(t, "unknown-major", err)
	})
	t.Run("unknown-minor", func(t *testing.T) {
		r := NewReader([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x02})
		maj, min, err := r.ReadHeader()
		if err != nil {
			t.Fatalf("unknown-minor: want parse success (additive-only), got %v", err)
		}
		if maj != 0 || min != 2 {
			t.Fatalf("unknown-minor: header = %d.%d, want 0.2", maj, min)
		}
	})
	t.Run("view-oob", func(t *testing.T) {
		// ARRAY L=1 E=0, then VIEW3 {id=0, off=1, len=0, cap=1}: off+cap=2 > L=1.
		r := NewReader([]byte{0x81, 0x00, 0x92, 0x00, 0x01, 0x00, 0x01})
		if _, _, _, err := r.ReadArrayHeader(); err != nil {
			t.Fatalf("view-oob setup: %v", err)
		}
		_, err := r.ReadView()
		isErrFormat(t, "view-oob (off+cap > L before any reflect)", err)
	})
	t.Run("huge-L", func(t *testing.T) {
		// ARRAY L=2^50 (u64 form), E=0; budget 2^20 applies to L.
		r := NewReader([]byte{0x8F, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		r.MaxSliceLen = 1 << 20
		_, _, _, err := r.ReadArrayHeader()
		isErrBudget(t, "huge-L (budget on backing length)", err)
	})
	t.Run("ref-unregistered", func(t *testing.T) {
		r := NewReader([]byte{0xC5})
		_, err := r.ReadRef()
		isErrFormat(t, "ref-unregistered", err)
	})
	t.Run("unknown-opcode", func(t *testing.T) {
		// E2 2B: escape-experimental with unknown subclass — reject, no skip.
		r := NewReader([]byte{0xE2, 0x2B})
		_, _, err := r.ReadEscToken()
		isErrFormat(t, "unknown-opcode (never a silent skip)", err)
	})
	t.Run("esc-nonzero-nibble", func(t *testing.T) {
		// E1 2B: escape class with a nonzero reserved low nibble — a
		// second spelling of the E0 subclass token (canonicality).
		r := NewReader([]byte{0xE1, 0x2B})
		_, _, err := r.ReadEscToken()
		isErrFormat(t, "esc-nonzero-nibble (one legal spelling)", err)
	})
	t.Run("desc-unknown-kind", func(t *testing.T) {
		// DC 20: kind 32 is reserved.
		r := NewReader([]byte{0xDC, 0x20})
		_, err := r.ReadDesc()
		isErrFormat(t, "desc-unknown-kind", err)
	})
}

// TestReservedSelectors: unknown selector/form values of every class are
// rejected (unknown opcode in a known position → ErrFormat).
func TestReservedSelectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		read func(*Reader) error
	}{
		{"nil-selector-4", []byte{0x04}, func(r *Reader) error { _, err := r.ReadNil(); return err }},
		{"nil-width-form", []byte{0x0C, 0x20}, func(r *Reader) error { _, err := r.ReadNil(); return err }},
		{"bool-selector-2", []byte{0x12}, func(r *Reader) error { _, err := r.ReadBool(); return err }},
		{"bool-width-form", []byte{0x1D, 0x00, 0x01}, func(r *Reader) error { _, err := r.ReadBool(); return err }},
		{"float-form-2", []byte{0x42}, func(r *Reader) error { _, err := r.ReadFloat32(); return err }},
		{"complex-form-2", []byte{0x52}, func(r *Reader) error { _, err := r.ReadComplex64(); return err }},
		{"view-form-3", []byte{0x93}, func(r *Reader) error { _, err := r.ReadView(); return err }},
		{"struct-form-1", []byte{0xB1}, func(r *Reader) error { return r.ReadStructHeader() }},
		{"struct-width-form", []byte{0xBC, 0x01}, func(r *Reader) error { return r.ReadStructHeader() }},
		{"desc-kind-u16-form", []byte{0xDD, 0x00, 0x0C}, func(r *Reader) error { _, err := r.ReadDesc(); return err }},
		{"desc-nonmin-kind", []byte{0xDC, 0x00}, func(r *Reader) error { _, err := r.ReadDesc(); return err }},
		{"int-nonmin-u16", []byte{0x2D, 0x00, 0x02}, func(r *Reader) error { _, err := r.ReadInt(); return err }},
		{"arg-unknown-form", []byte{0x37}, func(r *Reader) error { _, err := r.ReadArg(); return err }},
		{"string-wrong-class", []byte{0x20}, func(r *Reader) error { _, err := r.ReadString(); return err }},
		{"desc-wrong-class", []byte{0x20}, func(r *Reader) error { _, err := r.ReadDesc(); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.read(NewReader(c.data))
			isErrFormat(t, c.name, err)
		})
	}
}

// TestViewOverNonBacking: a VIEW token referencing a non-backing record
// (a string) is malformed.
func TestViewOverNonBacking(t *testing.T) {
	r := NewReader([]byte{0x60, 0x90, 0x00})
	if _, err := r.ReadStringLit(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := r.ReadView()
	isErrFormat(t, "view over string entry", err)
}

// TestRefKindMismatch: a REF resolved in a position of the wrong record
// sort is malformed (string position over a blob record).
func TestRefKindMismatch(t *testing.T) {
	r := NewReader([]byte{0x70, 0x00, 0xC0})
	if _, _, _, err := r.ReadBlobHeader(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := r.ReadString()
	isErrFormat(t, "string position over blob ref", err)
}
