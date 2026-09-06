package wire

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// Golden vectors for the wire primitives, byte literals derived from the
// format tables (independent test-oracle). White-box,
// same-package: no module imports (test isolation).

// goldenVec is one golden test case: a setup prefix that establishes intern
// context (its bytes are produced by the same enc path), the spec hex
// literal for the vector itself, and decoders that assert decoded values.
type goldenVec struct {
	name  string
	setup string
	want  string
	enc   func(w *Writer) error
	dec   func(r *Reader) error
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex literal %q: %v", s, err)
	}
	return b
}

func hexOf(b []byte) string {
	return hex.EncodeToString(b)
}

// writeRaw appends body bytes (blob/array payload) directly.
func writeRaw(w *Writer, b ...byte) { w.buf = append(w.buf, b...) }

func wantDesc(want *Desc) func(*Reader) error {
	return func(r *Reader) error {
		got, err := r.ReadDesc()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("descriptor mismatch:\n got %+v\nwant %+v", got, want)
		}
		return nil
	}
}

var int64Desc = &Desc{Kind: KindInt, Name: "int64", Width: 8}

func goldenVectors() []goldenVec {
	return []goldenVec{
		{
			name: "header",
			want: "67 62 6F 6E 00 00",
			enc:  func(w *Writer) error { return w.WriteHeader() },
			dec: func(r *Reader) error {
				maj, min, err := r.ReadHeader()
				if err != nil {
					return err
				}
				if maj != 0 || min != 0 {
					return fmt.Errorf("header = %d.%d, want 0.0", maj, min)
				}
				return nil
			},
		},
		{name: "arg-0", want: "00",
			enc: func(w *Writer) error { w.writeArg(0); return nil },
			dec: func(r *Reader) error { return wantArg(r, 0) }},
		{name: "arg-1", want: "01",
			enc: func(w *Writer) error { w.writeArg(1); return nil },
			dec: func(r *Reader) error { return wantArg(r, 1) }},
		{name: "arg-11", want: "0B",
			enc: func(w *Writer) error { w.writeArg(11); return nil },
			dec: func(r *Reader) error { return wantArg(r, 11) }},
		{name: "arg-12", want: "0C 0C",
			enc: func(w *Writer) error { w.writeArg(12); return nil },
			dec: func(r *Reader) error { return wantArg(r, 12) }},
		{name: "arg-127", want: "0C 7F",
			enc: func(w *Writer) error { w.writeArg(127); return nil },
			dec: func(r *Reader) error { return wantArg(r, 127) }},
		{name: "arg-128", want: "0C 80",
			enc: func(w *Writer) error { w.writeArg(128); return nil },
			dec: func(r *Reader) error { return wantArg(r, 128) }},
		{name: "arg-256", want: "0D 01 00",
			enc: func(w *Writer) error { w.writeArg(256); return nil },
			dec: func(r *Reader) error { return wantArg(r, 256) }},
		{name: "arg-65536", want: "0E 00 01 00 00",
			enc: func(w *Writer) error { w.writeArg(65536); return nil },
			dec: func(r *Reader) error { return wantArg(r, 65536) }},
		{name: "arg-2^32", want: "0F 00 00 00 01 00 00 00 00",
			enc: func(w *Writer) error { w.writeArg(1 << 32); return nil },
			dec: func(r *Reader) error { return wantArg(r, 1<<32) }},
		{name: "arg-2^63", want: "0F 80 00 00 00 00 00 00 00",
			enc: func(w *Writer) error { w.writeArg(1 << 63); return nil },
			dec: func(r *Reader) error { return wantArg(r, 1<<63) }},

		{name: "int-0", want: "20",
			enc: func(w *Writer) error { return w.WriteInt(0) },
			dec: func(r *Reader) error { return wantInt(r, 0) }},
		{name: "int-neg1", want: "21",
			enc: func(w *Writer) error { return w.WriteInt(-1) },
			dec: func(r *Reader) error { return wantInt(r, -1) }},
		{name: "int-1", want: "22",
			enc: func(w *Writer) error { return w.WriteInt(1) },
			dec: func(r *Reader) error { return wantInt(r, 1) }},
		{name: "int-neg2", want: "23",
			enc: func(w *Writer) error { return w.WriteInt(-2) },
			dec: func(r *Reader) error { return wantInt(r, -2) }},
		{name: "int-127", want: "2C FE",
			enc: func(w *Writer) error { return w.WriteInt(127) },
			dec: func(r *Reader) error { return wantInt(r, 127) }},
		{name: "int-128", want: "2D 01 00",
			enc: func(w *Writer) error { return w.WriteInt(128) },
			dec: func(r *Reader) error { return wantInt(r, 128) }},
		{name: "int-min", want: "2F FF FF FF FF FF FF FF FF",
			enc: func(w *Writer) error { return w.WriteInt(math.MinInt64) },
			dec: func(r *Reader) error { return wantInt(r, math.MinInt64) }},
		{name: "int-max", want: "2F FF FF FF FF FF FF FF FE",
			enc: func(w *Writer) error { return w.WriteInt(math.MaxInt64) },
			dec: func(r *Reader) error { return wantInt(r, math.MaxInt64) }},

		{name: "uint-12", want: "3C 0C",
			enc: func(w *Writer) error { return w.WriteUint(12) },
			dec: func(r *Reader) error { return wantUint(r, 12) }},
		{name: "uint-2^32", want: "3F 00 00 00 01 00 00 00 00",
			enc: func(w *Writer) error { return w.WriteUint(1 << 32) },
			dec: func(r *Reader) error { return wantUint(r, 1<<32) }},
		{name: "uint-2^63", want: "3F 80 00 00 00 00 00 00 00",
			enc: func(w *Writer) error { return w.WriteUint(1 << 63) },
			dec: func(r *Reader) error { return wantUint(r, 1<<63) }},

		{name: "bool-f", want: "10",
			enc: func(w *Writer) error { return w.WriteBool(false) },
			dec: func(r *Reader) error { return wantBool(r, false) }},
		{name: "bool-t", want: "11",
			enc: func(w *Writer) error { return w.WriteBool(true) },
			dec: func(r *Reader) error { return wantBool(r, true) }},

		{name: "nil-ptr", want: "00",
			enc: func(w *Writer) error { return w.WriteNil(NilPointer) },
			dec: func(r *Reader) error { return wantNil(r, NilPointer) }},
		{name: "nil-slice", want: "01",
			enc: func(w *Writer) error { return w.WriteNil(NilSlice) },
			dec: func(r *Reader) error { return wantNil(r, NilSlice) }},
		{name: "nil-map", want: "02",
			enc: func(w *Writer) error { return w.WriteNil(NilMap) },
			dec: func(r *Reader) error { return wantNil(r, NilMap) }},
		{name: "nil-iface", want: "03",
			enc: func(w *Writer) error { return w.WriteNil(NilInterface) },
			dec: func(r *Reader) error { return wantNil(r, NilInterface) }},

		{name: "f64-pos0", want: "41 00 00 00 00 00 00 00 00",
			enc: func(w *Writer) error { return w.WriteFloat64(0) },
			dec: func(r *Reader) error { return wantF64Bits(r, 0x0000000000000000) }},
		{name: "f64-neg0", want: "41 80 00 00 00 00 00 00 00",
			enc: func(w *Writer) error { return w.WriteFloat64(math.Copysign(0, -1)) },
			dec: func(r *Reader) error { return wantF64Bits(r, 0x8000000000000000) }},
		{name: "f64-nan", want: "41 7F F8 00 00 00 00 00 01",
			enc: func(w *Writer) error { return w.WriteFloat64(math.Float64frombits(0x7FF8000000000001)) },
			dec: func(r *Reader) error { return wantF64Bits(r, 0x7FF8000000000001) }},
		{name: "f64-one", want: "41 3F F0 00 00 00 00 00 00",
			enc: func(w *Writer) error { return w.WriteFloat64(1.0) },
			dec: func(r *Reader) error { return wantF64Bits(r, 0x3FF0000000000000) }},
		{name: "f64-subnormal", want: "41 00 00 00 00 00 00 00 01",
			enc: func(w *Writer) error { return w.WriteFloat64(math.Float64frombits(1)) },
			dec: func(r *Reader) error { return wantF64Bits(r, 0x0000000000000001) }},

		{name: "f32-pos0", want: "40 00 00 00 00",
			enc: func(w *Writer) error { return w.WriteFloat32(0) },
			dec: func(r *Reader) error { return wantF32Bits(r, 0x00000000) }},
		{name: "f32-nan", want: "40 7F C0 00 01",
			enc: func(w *Writer) error { return w.WriteFloat32(math.Float32frombits(0x7FC00001)) },
			dec: func(r *Reader) error { return wantF32Bits(r, 0x7FC00001) }},

		{name: "c128", want: "51 3F F0 00 00 00 00 00 00 7F F8 00 00 00 00 00 01",
			enc: func(w *Writer) error {
				return w.WriteComplex128(complex(1.0, math.Float64frombits(0x7FF8000000000001)))
			},
			dec: func(r *Reader) error {
				c, err := r.ReadComplex128()
				if err != nil {
					return err
				}
				if math.Float64bits(real(c)) != 0x3FF0000000000000 ||
					math.Float64bits(imag(c)) != 0x7FF8000000000001 {
					return fmt.Errorf("c128 rawbits mismatch: %016x %016x",
						math.Float64bits(real(c)), math.Float64bits(imag(c)))
				}
				return nil
			}},

		{name: "str-empty", want: "60",
			enc: func(w *Writer) error { return w.WriteStringLit("") },
			dec: func(r *Reader) error { return wantStringLit(r, "") }},
		{name: "str-gbon", want: "64 67 62 6F 6E",
			enc: func(w *Writer) error { return w.WriteStringLit("gbon") },
			dec: func(r *Reader) error { return wantStringLit(r, "gbon") }},
		{name: "str-len12", want: "6C 0C 30 31 32 33 34 35 36 37 38 39 41 42",
			enc: func(w *Writer) error { return w.WriteStringLit("0123456789AB") },
			dec: func(r *Reader) error { return wantStringLit(r, "0123456789AB") }},
		{name: "str-invalid-utf8", want: "62 FF FE",
			enc: func(w *Writer) error { return w.WriteStringLit("\xff\xfe") },
			dec: func(r *Reader) error { return wantStringLit(r, "\xff\xfe") }},
		{name: "str-ref", want: "64 67 62 6F 6E C0",
			enc: func(w *Writer) error {
				if err := w.WriteStringLit("gbon"); err != nil {
					return err
				}
				return w.WriteRef(0)
			},
			dec: func(r *Reader) error {
				if err := wantStringLit(r, "gbon"); err != nil {
					return err
				}
				id, err := r.ReadRef()
				if err != nil {
					return err
				}
				if id != 0 {
					return fmt.Errorf("str-ref: want id 0, got %d", id)
				}
				return nil
			}},

		{
			name: "blob",
			want: "75 02 AB CD",
			enc: func(w *Writer) error {
				if err := w.WriteBlobHeader(5, 2); err != nil {
					return err
				}
				writeRaw(w, 0xAB, 0xCD)
				return nil
			},
			dec: func(r *Reader) error {
				L, E, _, err := r.ReadBlobHeader()
				if err != nil {
					return err
				}
				if L != 5 || E != 2 {
					return fmt.Errorf("blob header = (%d,%d), want (5,2)", L, E)
				}
				b, err := r.readN(E)
				if err != nil {
					return err
				}
				if len(b) != 2 || b[0] != 0xAB || b[1] != 0xCD {
					return fmt.Errorf("blob body = % x, want AB CD", b)
				}
				return nil
			},
		},

		{
			name: "array",
			want: "83 02 20 22",
			enc: func(w *Writer) error {
				if err := w.WriteArrayHeader(3, 2); err != nil {
					return err
				}
				if err := w.WriteInt(0); err != nil {
					return err
				}
				return w.WriteInt(1)
			},
			dec: func(r *Reader) error {
				return readIntArray(r, 3, 2, []int64{0, 1})
			},
		},
		{
			name: "array-huge",
			want: "8E 00 10 00 00 02 20 22",
			enc: func(w *Writer) error {
				if err := w.WriteArrayHeader(1<<20, 2); err != nil {
					return err
				}
				if err := w.WriteInt(0); err != nil {
					return err
				}
				return w.WriteInt(1)
			},
			dec: func(r *Reader) error {
				return readIntArray(r, 1<<20, 2, []int64{0, 1})
			},
		},

		{
			name:  "view-full",
			setup: "70 00 70 00",
			want:  "90 01",
			enc: func(w *Writer) error {
				if err := w.WriteBlobHeader(0, 0); err != nil {
					return err
				}
				if err := w.WriteBlobHeader(0, 0); err != nil {
					return err
				}
				return w.WriteViewFull(1)
			},
			dec: func(r *Reader) error {
				if err := skipTwoBlobs(r); err != nil {
					return err
				}
				return wantView(r, View{ID: 1, Off: 0, Len: 0, Cap: 0})
			},
		},
		{
			name:  "view-offlen",
			setup: "70 00 75 00",
			want:  "91 01 02 03",
			enc: func(w *Writer) error {
				if err := w.WriteBlobHeader(0, 0); err != nil {
					return err
				}
				if err := w.WriteBlobHeader(5, 0); err != nil {
					return err
				}
				return w.WriteView(1, 2, 3)
			},
			dec: func(r *Reader) error {
				if err := skipTwoBlobs(r); err != nil {
					return err
				}
				return wantView(r, View{ID: 1, Off: 2, Len: 3, Cap: 3})
			},
		},
		{
			name:  "view-full3",
			setup: "70 00 76 00",
			want:  "92 01 02 03 04",
			enc: func(w *Writer) error {
				if err := w.WriteBlobHeader(0, 0); err != nil {
					return err
				}
				if err := w.WriteBlobHeader(6, 0); err != nil {
					return err
				}
				return w.WriteView3(1, 2, 3, 4)
			},
			dec: func(r *Reader) error {
				if err := skipTwoBlobs(r); err != nil {
					return err
				}
				return wantView(r, View{ID: 1, Off: 2, Len: 3, Cap: 4})
			},
		},
		{
			name:  "view-empty",
			setup: "70 00 75 00",
			want:  "91 01 00 00",
			enc: func(w *Writer) error {
				if err := w.WriteBlobHeader(0, 0); err != nil {
					return err
				}
				if err := w.WriteBlobHeader(5, 0); err != nil {
					return err
				}
				return w.WriteView(1, 0, 0)
			},
			dec: func(r *Reader) error {
				if err := skipTwoBlobs(r); err != nil {
					return err
				}
				return wantView(r, View{ID: 1, Off: 0, Len: 0, Cap: 0})
			},
		},
		{
			name:  "view-huge-cap",
			setup: "83 02 20 22 8E 00 10 00 00 02 20 22",
			want:  "92 01 00 02 0E 00 10 00 00",
			enc: func(w *Writer) error {
				for _, L := range []uint64{3, 1 << 20} {
					if err := w.WriteArrayHeader(L, 2); err != nil {
						return err
					}
					if err := w.WriteInt(0); err != nil {
						return err
					}
					if err := w.WriteInt(1); err != nil {
						return err
					}
				}
				return w.WriteView3(1, 0, 2, 1<<20)
			},
			dec: func(r *Reader) error {
				for _, L := range []uint64{3, 1 << 20} {
					if err := readIntArray(r, L, 2, []int64{0, 1}); err != nil {
						return err
					}
				}
				return wantView(r, View{ID: 1, Off: 0, Len: 2, Cap: 1 << 20})
			},
		},

		{
			name:  "ref-0",
			setup: "70 00",
			want:  "C0",
			enc: func(w *Writer) error {
				if err := w.WriteBlobHeader(0, 0); err != nil {
					return err
				}
				return w.WriteRef(0)
			},
			dec: func(r *Reader) error {
				if _, _, _, err := r.ReadBlobHeader(); err != nil {
					return err
				}
				id, err := r.ReadRef()
				if err != nil {
					return err
				}
				if id != 0 {
					return fmt.Errorf("ref id = %d, want 0", id)
				}
				return nil
			},
		},
		{
			name:  "ref-12",
			setup: "70 00 70 00 70 00 70 00 70 00 70 00 70 00 70 00 70 00 70 00 70 00 70 00 70 00",
			want:  "CC 0C",
			enc: func(w *Writer) error {
				for range 13 {
					if err := w.WriteBlobHeader(0, 0); err != nil {
						return err
					}
				}
				return w.WriteRef(12)
			},
			dec: func(r *Reader) error {
				for range 13 {
					if _, _, _, err := r.ReadBlobHeader(); err != nil {
						return err
					}
				}
				id, err := r.ReadRef()
				if err != nil {
					return err
				}
				if id != 12 {
					return fmt.Errorf("ref id = %d, want 12", id)
				}
				return nil
			},
		},

		{name: "map-empty", want: "A0",
			enc: func(w *Writer) error { return w.WriteMapHeader(0) },
			dec: func(r *Reader) error {
				n, _, err := r.ReadMapHeader()
				if err != nil {
					return err
				}
				if n != 0 {
					return fmt.Errorf("map count = %d, want 0", n)
				}
				return nil
			}},

		{
			name: "desc-struct",
			want: "D0 65 50 6F 69 6E 74 02 61 58 D8 65 69 6E 74 36 34 08 61 59 C3",
			enc: func(w *Writer) error {
				point := &Desc{Kind: KindStruct, Name: "Point", Fields: []Field{
					{Name: "X", Type: int64Desc},
					{Name: "Y", Type: int64Desc},
				}}
				return w.WriteDesc(point)
			},
			dec: func(r *Reader) error {
				got, err := r.ReadDesc()
				if err != nil {
					return err
				}
				want := &Desc{Kind: KindStruct, Name: "Point", Fields: []Field{
					{Name: "X", Type: int64Desc},
					{Name: "Y", Type: int64Desc},
				}}
				if !reflect.DeepEqual(got, want) {
					return fmt.Errorf("descriptor mismatch:\n got %+v\nwant %+v", got, want)
				}
				// REF sharing: X.Type and Y.Type resolve to one descriptor.
				if got.Fields[0].Type != got.Fields[1].Type {
					return errors.New("desc-struct: X.Type and Y.Type are not shared via REF")
				}
				return nil
			},
		},
		{
			name: "desc-struct-nested",
			want: "D0 65 4F 75 74 65 72 02 61 49 D0 65 49 6E 6E 65 72 01 61 41 D8 65 69 6E 74 36 34 08 61 42 D7 64 62 6F 6F 6C",
			enc: func(w *Writer) error {
				inner := &Desc{Kind: KindStruct, Name: "Inner", Fields: []Field{
					{Name: "A", Type: int64Desc},
				}}
				boolean := &Desc{Kind: KindBool, Name: "bool"}
				outer := &Desc{Kind: KindStruct, Name: "Outer", Fields: []Field{
					{Name: "I", Type: inner},
					{Name: "B", Type: boolean},
				}}
				return w.WriteDesc(outer)
			},
			dec: wantDesc(&Desc{Kind: KindStruct, Name: "Outer", Fields: []Field{
				{Name: "I", Type: &Desc{Kind: KindStruct, Name: "Inner", Fields: []Field{
					{Name: "A", Type: int64Desc},
				}}},
				{Name: "B", Type: &Desc{Kind: KindBool, Name: "bool"}},
			}}),
		},
		{
			name: "desc-slice-of-struct",
			want: "D1 67 5B 5D 50 6F 69 6E 74 D0 65 50 6F 69 6E 74 01 61 58 D8 65 69 6E 74 36 34 08",
			enc: func(w *Writer) error {
				point := &Desc{Kind: KindStruct, Name: "Point", Fields: []Field{
					{Name: "X", Type: int64Desc},
				}}
				slice := &Desc{Kind: KindSlice, Name: "[]Point", Refs: []*Desc{point}}
				return w.WriteDesc(slice)
			},
			dec: wantDesc(&Desc{Kind: KindSlice, Name: "[]Point", Refs: []*Desc{
				{Kind: KindStruct, Name: "Point", Fields: []Field{
					{Name: "X", Type: int64Desc},
				}},
			}}),
		},
		{
			name: "desc-map-struct-slice",
			want: "D3 6C 11 6D 61 70 5B 50 6F 69 6E 74 5D 5B 5D 69 6E 74 36 34 D0 65 50 6F 69 6E 74 01 61 58 D8 65 69 6E 74 36 34 08 D1 67 5B 5D 69 6E 74 36 34 C5",
			enc: func(w *Writer) error {
				point := &Desc{Kind: KindStruct, Name: "Point", Fields: []Field{
					{Name: "X", Type: int64Desc},
				}}
				ints := &Desc{Kind: KindSlice, Name: "[]int64", Refs: []*Desc{int64Desc}}
				m := &Desc{Kind: KindMap, Name: "map[Point][]int64", Refs: []*Desc{point, ints}}
				return w.WriteDesc(m)
			},
			dec: wantDesc(&Desc{Kind: KindMap, Name: "map[Point][]int64", Refs: []*Desc{
				{Kind: KindStruct, Name: "Point", Fields: []Field{
					{Name: "X", Type: int64Desc},
				}},
				{Kind: KindSlice, Name: "[]int64", Refs: []*Desc{int64Desc}},
			}}),
		},
		{
			name: "desc-named",
			want: "D4 67 43 65 6C 73 69 75 73 DA 67 66 6C 6F 61 74 36 34 08",
			enc: func(w *Writer) error {
				f64 := &Desc{Kind: KindFloat, Name: "float64", Width: 8}
				celsius := &Desc{Kind: KindNamed, Name: "Celsius", Refs: []*Desc{f64}}
				return w.WriteDesc(celsius)
			},
			dec: wantDesc(&Desc{Kind: KindNamed, Name: "Celsius", Refs: []*Desc{
				{Kind: KindFloat, Name: "float64", Width: 8},
			}}),
		},
		{
			name: "desc-iface-field",
			want: "D0 63 42 6F 78 01 61 56 D6 63 61 6E 79",
			enc: func(w *Writer) error {
				any := &Desc{Kind: KindInterface, Name: "any"}
				box := &Desc{Kind: KindStruct, Name: "Box", Fields: []Field{
					{Name: "V", Type: any},
				}}
				return w.WriteDesc(box)
			},
			dec: wantDesc(&Desc{Kind: KindStruct, Name: "Box", Fields: []Field{
				{Name: "V", Type: &Desc{Kind: KindInterface, Name: "any"}},
			}}),
		},
		{
			name: "desc-recursive",
			want: "D0 64 4E 6F 64 65 01 64 4E 65 78 74 D5 65 2A 4E 6F 64 65 C0",
			enc: func(w *Writer) error {
				node := &Desc{Kind: KindStruct, Name: "Node"}
				ptr := &Desc{Kind: KindPointer, Name: "*Node", Refs: []*Desc{node}}
				node.Fields = []Field{{Name: "Next", Type: ptr}}
				return w.WriteDesc(node)
			},
			dec: func(r *Reader) error {
				got, err := r.ReadDesc()
				if err != nil {
					return err
				}
				node := &Desc{Kind: KindStruct, Name: "Node"}
				ptr := &Desc{Kind: KindPointer, Name: "*Node", Refs: []*Desc{node}}
				node.Fields = []Field{{Name: "Next", Type: ptr}}
				if !reflect.DeepEqual(got, node) {
					return fmt.Errorf("descriptor mismatch:\n got %+v\nwant %+v", got, node)
				}
				// Cycle closed through REF: Next.Type pointee is Node itself.
				if got.Fields[0].Type.Refs[0] != got {
					return errors.New("desc-recursive: cycle not closed via REF")
				}
				return nil
			},
		},
	}
}

// TestGoldenVectors drives every golden vector through encode (hex compare)
// and decode (value assertions).
func TestGoldenVectors(t *testing.T) {
	vecs := goldenVectors()
	// 55 base vectors (54 + str-ref); the bool-f/t and
	// nil-ptr/slice/map/iface rows expand to multiple byte vectors (2 + 4),
	// giving 59 actual vectors.
	if len(vecs) != 59 {
		t.Fatalf("golden vector count = %d, want 59 (55 base vectors expanded)", len(vecs))
	}
	for _, v := range vecs {
		t.Run(v.name, func(t *testing.T) {
			w := NewWriter()
			if err := v.enc(w); err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := hexOf(w.Bytes())
			wantFull := strings.ToLower(strings.ReplaceAll(v.setup+v.want, " ", ""))
			if got != wantFull {
				t.Errorf("bytes mismatch:\n got %s\nwant %s", got, wantFull)
			}
			r := NewReader(unhex(t, wantFull))
			if err := v.dec(r); err != nil {
				t.Errorf("decode: %v", err)
			}
			if r.Pos() != len(w.Bytes()) {
				t.Errorf("decode consumed %d bytes, stream is %d", r.Pos(), len(w.Bytes()))
			}
		})
	}
}

func wantArg(r *Reader, want uint64) error {
	got, err := r.ReadArg()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("arg = %d, want %d", got, want)
	}
	return nil
}

func wantInt(r *Reader, want int64) error {
	got, err := r.ReadInt()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("int = %d, want %d", got, want)
	}
	return nil
}

func wantUint(r *Reader, want uint64) error {
	got, err := r.ReadUint()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("uint = %d, want %d", got, want)
	}
	return nil
}

func wantBool(r *Reader, want bool) error {
	got, err := r.ReadBool()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("bool = %v, want %v", got, want)
	}
	return nil
}

func wantNil(r *Reader, want NilKind) error {
	got, err := r.ReadNil()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("nil kind = %d, want %d", got, want)
	}
	return nil
}

func wantF64Bits(r *Reader, want uint64) error {
	f, err := r.ReadFloat64()
	if err != nil {
		return err
	}
	if got := math.Float64bits(f); got != want {
		return fmt.Errorf("f64 bits = %016x, want %016x", got, want)
	}
	return nil
}

func wantF32Bits(r *Reader, want uint32) error {
	f, err := r.ReadFloat32()
	if err != nil {
		return err
	}
	if got := math.Float32bits(f); got != want {
		return fmt.Errorf("f32 bits = %08x, want %08x", got, want)
	}
	return nil
}

func wantStringLit(r *Reader, want string) error {
	got, err := r.ReadStringLit()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("string = %q, want %q", got, want)
	}
	return nil
}

func wantView(r *Reader, want View) error {
	got, err := r.ReadView()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("view = %+v, want %+v", got, want)
	}
	return nil
}

func readIntArray(r *Reader, L, E uint64, elems []int64) error {
	gotL, gotE, _, err := r.ReadArrayHeader()
	if err != nil {
		return err
	}
	if gotL != L || gotE != E {
		return fmt.Errorf("array header = (%d,%d), want (%d,%d)", gotL, gotE, L, E)
	}
	for i, want := range elems {
		if err := wantInt(r, want); err != nil {
			return fmt.Errorf("elem %d: %w", i, err)
		}
	}
	return nil
}

func skipTwoBlobs(r *Reader) error {
	for range 2 {
		if _, _, _, err := r.ReadBlobHeader(); err != nil {
			return err
		}
	}
	return nil
}

// BIGINT value-body vectors: the bare/ext argument
// of the zigzag image, the nil/zero coincidence byte, and the skip mirrors.
func bigintBodyVectors() []goldenVec {
	zz := func(hexBytes string) string { return hexBytes }
	return []goldenVec{
		{name: "big-nil", want: "00",
			enc: func(w *Writer) error { return w.WriteBigint(nil) },
			dec: func(r *Reader) error {
				v, err := r.ReadBigint(0)
				if err != nil || v != nil {
					return fmt.Errorf("bigint nil = %v %v", v, err)
				}
				return nil
			}},
		{name: "big-zero", want: "00",
			enc: func(w *Writer) error { return w.WriteBigint(big.NewInt(0)) },
			dec: func(r *Reader) error {
				// the coincidence byte: nil to every projection (the
				// value shape materializes zero)
				v, err := r.ReadBigint(0)
				if err != nil || v != nil {
					return fmt.Errorf("bigint zero coincidence = %v %v", v, err)
				}
				return nil
			}},
		{name: "big-5", want: zz("0A"),
			enc: func(w *Writer) error { return w.WriteBigint(big.NewInt(5)) },
			dec: func(r *Reader) error { return wantBigint(r, big.NewInt(5)) }},
		{name: "big-minint64", want: "0F FFFFFFFFFFFFFFFF",
			enc: func(w *Writer) error { return w.WriteBigint(big.NewInt(-1 << 63)) },
			dec: func(r *Reader) error { return wantBigint(r, big.NewInt(-1<<63)) }},
		{name: "big-2^64", want: "10 09 02 0000000000000000",
			enc: func(w *Writer) error {
				return w.WriteBigint(new(big.Int).SetBytes([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0}))
			},
			dec: func(r *Reader) error {
				return wantBigint(r, new(big.Int).SetBytes([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0}))
			}},
		{name: "big--(2^64+1)", want: "10 09 02 0000000000000001",
			enc: func(w *Writer) error {
				v := new(big.Int).SetBytes([]byte{1, 0, 0, 0, 0, 0, 0, 0, 1})
				return w.WriteBigint(v.Neg(v))
			},
			dec: func(r *Reader) error {
				v := new(big.Int).SetBytes([]byte{1, 0, 0, 0, 0, 0, 0, 0, 1})
				return wantBigint(r, v.Neg(v))
			}},
	}
}

func wantBigint(r *Reader, want *big.Int) error {
	got, err := r.ReadBigint(1 << 20)
	if err != nil {
		return err
	}
	if got == nil || got.Cmp(want) != 0 {
		return fmt.Errorf("bigint = %v, want %v", got, want)
	}
	return nil
}

// TestGoldenBigintBodies drives the BIGINT value-body vectors.
func TestGoldenBigintBodies(t *testing.T) {
	for _, v := range bigintBodyVectors() {
		t.Run(v.name, func(t *testing.T) {
			w := NewWriter()
			if err := v.enc(w); err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := hexOf(w.Bytes())
			wantFull := strings.ToLower(strings.ReplaceAll(v.want, " ", ""))
			if got != wantFull {
				t.Fatalf("bytes mismatch:\n got %s\nwant %s", got, wantFull)
			}
			r := NewReader(unhex(t, wantFull))
			if err := v.dec(r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if r.Pos() != len(w.Bytes()) {
				t.Fatalf("decode consumed %d bytes, stream is %d", r.Pos(), len(w.Bytes()))
			}
		})
	}
}

// BIGINT/ext-ARG negative paths: minimality and budget rejects fire before
// any body consumption beyond the token, and the skip mirrors parse-only.
func TestBigintExtErrors(t *testing.T) {
	mk := func(body string) *Reader {
		return NewReader(unhex(t, strings.ReplaceAll(body, " ", "")))
	}
	if _, err := mk("10 08 0200000000000000").ReadBigint(1 << 20); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("short ext: want ErrFormat, got %v", err)
	}
	if _, err := mk("10 09 0000000000000001").ReadBigint(1 << 20); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("zero-leading ext: want ErrFormat, got %v", err)
	}
	if _, err := mk("10 09 02").ReadBigint(1 << 20); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("truncated ext: want ErrFormat, got %v", err)
	}
	if _, err := mk("10 0F 0000010000000000").ReadBigint(1 << 20); err == nil || !errors.Is(err, ErrBudget) {
		t.Fatalf("budget ext: want ErrBudget, got %v", err)
	}
	if err := mk("10 09 02 0000000000000000").SkipBigint(1 << 20); err != nil {
		t.Fatalf("skip ext: %v", err)
	}
	if err := mk("00").SkipBigint(0); err != nil {
		t.Fatalf("skip nil coincidence: %v", err)
	}
	if err := mk("0A").SkipBigint(0); err != nil {
		t.Fatalf("skip inline: %v", err)
	}
	// the ext selector is an unknown argument form in plain bare positions
	if _, err := mk("10 09 02").ReadArg(); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("ext in bare position: want ErrFormat, got %v", err)
	}
}

// FLOAT form-2 token: skip consumes the token and 16 raw bytes; the
// materializing readers keep rejecting the form.
func TestDecimal128Skip(t *testing.T) {
	w := NewWriter()
	w.buf = append(w.buf, classFloat<<4|2)
	w.buf = append(w.buf, make([]byte, 16)...)
	r := NewReader(w.Bytes())
	if err := r.SkipDecimal128(); err != nil {
		t.Fatalf("skip decimal128: %v", err)
	}
	if r.Pos() != 17 {
		t.Fatalf("skip consumed %d bytes, want 17", r.Pos())
	}
	if _, err := NewReader(w.Bytes()).ReadFloat64(); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("form 2 in float64 reader: want ErrFormat, got %v", err)
	}
	if err := NewReader(append([]byte{classFloat<<4 | 2}, make([]byte, 15)...)).SkipDecimal128(); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("truncated decimal128: want ErrFormat, got %v", err)
	}
	if err := NewReader([]byte{classFloat<<4 | 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}).SkipDecimal128(); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("unknown form 3: want ErrFormat, got %v", err)
	}
}

// FLOAT width-16 descriptor: the width rides (4/8/16) and parses back;
// 16 pairs with form 2 (decimal128); other float widths stay invalid.
func TestFloatWidth16Desc(t *testing.T) {
	d := &Desc{Kind: KindFloat, Name: "dec", Width: 16}
	w := NewWriter()
	if err := w.WriteDesc(d); err != nil {
		t.Fatal(err)
	}
	r := NewReader(w.Bytes())
	got, err := r.ReadDesc()
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindFloat || got.Width != 16 {
		t.Fatalf("desc = %+v, want FLOAT/16", got)
	}
	if err := w.WriteDesc(&Desc{Kind: KindFloat, Name: "x", Width: 2}); err == nil {
		t.Fatal("float width 2 must reject on write")
	}
	r2 := NewReader(unhex(t, "DA617802"))
	if _, err := r2.ReadDesc(); err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("float width 2 read: want ErrFormat, got %v", err)
	}
	if err := w.WriteDesc(&Desc{Kind: KindComplex, Name: "x", Width: 16}); err == nil {
		t.Fatal("complex width 16 must reject")
	}
}
