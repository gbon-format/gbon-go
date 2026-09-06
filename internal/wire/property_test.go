package wire

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

// Property scenarios (P-min, P-nonmin, P-trunc, P-stream, P-det,
// P-zigzag) driven over the golden vector corpus and width boundaries.

// P-min: strict width boundaries — the value→length map is an injection.
func TestPropertyMinimalWidths(t *testing.T) {
	cases := []struct {
		n    uint64
		size int
	}{
		{0, 1}, {11, 1},
		{12, 2}, {255, 2},
		{256, 3}, {65535, 3},
		{65536, 5}, {1<<32 - 1, 5},
		{1 << 32, 9}, {math.MaxUint64, 9},
	}
	for _, c := range cases {
		w := NewWriter()
		w.writeArg(c.n)
		if got := len(w.Bytes()); got != c.size {
			t.Errorf("P-min: len(arg(%d)) = %d, want %d", c.n, got, c.size)
		}
	}
}

// P-nonmin: every spare-width encoding is ErrFormat on decode.
func TestPropertyNonMinimalRejected(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"0-in-u8", []byte{0x0C, 0x00}},
		{"11-in-u8", []byte{0x0C, 0x0B}},
		{"12-in-u16", []byte{0x0D, 0x00, 0x0C}},
		{"255-in-u16", []byte{0x0D, 0x00, 0xFF}},
		{"256-in-u32", []byte{0x0E, 0x00, 0x00, 0x01, 0x00}},
		{"65535-in-u32", []byte{0x0E, 0x00, 0x00, 0xFF, 0xFF}},
		{"65536-in-u64", []byte{0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}},
		{"2^32-1-in-u64", []byte{0x0F, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewReader(c.data).ReadArg(); !errors.Is(err, ErrFormat) {
				t.Errorf("P-nonmin %s: want ErrFormat, got %v", c.name, err)
			}
		})
	}
}

// P-trunc: every strict prefix of every golden vector fails to decode —
// no panic, no success (self-delimiting grammar).
func TestPropertyTruncation(t *testing.T) {
	for _, v := range goldenVectors() {
		full := unhex(t, v.setup+v.want)
		for k := range full {
			r := NewReader(full[:k])
			if err := v.dec(r); err == nil {
				t.Errorf("P-trunc %s[:%d]: decoded successfully from a strict prefix", v.name, k)
			}
		}
	}
}

// P-stream: a concatenation of N encodings yields exactly N decodes with
// the offset landing precisely at the end.
func TestPropertyStream(t *testing.T) {
	w := NewWriter()
	if err := w.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteInt(-9); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteBool(true); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFloat64(math.Copysign(0, -1)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteString("gbon"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteString("gbon"); err != nil { // second encounter → REF
		t.Fatal(err)
	}
	w.writeArg(300)
	if err := w.WriteUint(12); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFloat32(1.5); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteComplex128(complex(2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMapHeader(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteStructHeader(); err != nil {
		t.Fatal(err)
	}

	r := NewReader(w.Bytes())
	steps := []func(*Reader) error{
		func(r *Reader) error { _, _, err := r.ReadHeader(); return err },
		func(r *Reader) error { return wantInt(r, -9) },
		func(r *Reader) error { return wantBool(r, true) },
		func(r *Reader) error { return wantF64Bits(r, 0x8000000000000000) },
		func(r *Reader) error { return wantString(r, "gbon") },
		func(r *Reader) error { return wantString(r, "gbon") }, // REF path
		func(r *Reader) error { return wantArg(r, 300) },
		func(r *Reader) error { return wantUint(r, 12) },
		func(r *Reader) error {
			f, err := r.ReadFloat32()
			if err != nil {
				return err
			}
			if f != 1.5 {
				t.Errorf("f32 = %v, want 1.5", f)
			}
			return nil
		},
		func(r *Reader) error {
			c, err := r.ReadComplex128()
			if err != nil {
				return err
			}
			if c != complex(2, 3) {
				t.Errorf("c128 = %v, want (2+3i)", c)
			}
			return nil
		},
		func(r *Reader) error {
			n, _, err := r.ReadMapHeader()
			if err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("map count = %d, want 0", n)
			}
			return nil
		},
		func(r *Reader) error { return r.ReadStructHeader() },
	}
	for i, step := range steps {
		if err := step(r); err != nil {
			t.Fatalf("P-stream step %d: %v", i, err)
		}
	}
	if r.Pos() != len(w.Bytes()) {
		t.Errorf("P-stream: offset %d after %d decodes, stream length %d",
			r.Pos(), len(steps), len(w.Bytes()))
	}
}

// P-det: encoding the same value sequence twice yields identical bytes
// (the BLOB canonical form); a repeated value in one
// stream takes the REF form.
func TestPropertyDeterminism(t *testing.T) {
	encode := func() []byte {
		w := NewWriter()
		if err := w.WriteHeader(); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteString("payload"); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteInt(42); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteString("payload"); err != nil { // REF
			t.Fatal(err)
		}
		point := &Desc{Kind: KindStruct, Name: "Point", Fields: []Field{
			{Name: "X", Type: int64Desc},
			{Name: "Y", Type: int64Desc},
		}}
		if err := w.WriteDesc(point); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteDesc(point); err != nil { // REF
			t.Fatal(err)
		}
		return w.Bytes()
	}
	a, b := encode(), encode()
	if !bytes.Equal(a, b) {
		t.Errorf("P-det: two encodings differ:\n %x\n %x", a, b)
	}
	// The second string and descriptor occurrences are REF tokens: the last
	// byte is REF id=1 (the Point descriptor, interned after "payload").
	if a[len(a)-1] != 0xC1 {
		t.Errorf("P-det: repeated descriptor not REF-encoded, last byte %02X", a[len(a)-1])
	}
}

// P-zigzag: bijection at the int64 extremes and the spec edge table.
func TestPropertyZigzag(t *testing.T) {
	edges := []struct {
		n int64
		u uint64
	}{
		{0, 0},
		{-1, 1},
		{1, 2},
		{-2, 3},
		{math.MinInt64, math.MaxUint64},
		{math.MaxInt64, math.MaxUint64 - 1},
	}
	for _, e := range edges {
		if got := zigzag(e.n); got != e.u {
			t.Errorf("zigzag(%d) = %#x, want %#x", e.n, got, e.u)
		}
		if got := unzigzag(e.u); got != e.n {
			t.Errorf("unzigzag(%#x) = %d, want %d", e.u, got, e.n)
		}
	}
	// Round-trip through the token layer at the extremes.
	for _, n := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		if err := wantInt(NewWriter2Reader(t, n), n); err != nil {
			t.Errorf("P-zigzag roundtrip %d: %v", n, err)
		}
	}
}

// NewWriter2Reader encodes n as an INT token and returns a reader over it.
func NewWriter2Reader(t *testing.T, n int64) *Reader {
	t.Helper()
	w := NewWriter()
	if err := w.WriteInt(n); err != nil {
		t.Fatal(err)
	}
	return NewReader(w.Bytes())
}

func wantString(r *Reader, want string) error {
	got, err := r.ReadString()
	if err != nil {
		return err
	}
	if got != want {
		return errors.New("string = " + got + ", want " + want)
	}
	return nil
}
