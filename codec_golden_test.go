package gbon_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// Kind-level golden vectors, hand-computed from the
// format tables. Marshal bytes are checked directly;
// values are never verified through Decode here.
type Point struct{ X, Y int64 }

type Celsius float64

type Blank struct {
	_ int
	X int
}

var hdr = []byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := gbon.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", safeDescValue(v), err)
	}
	return b
}

var golden = []struct {
	name  string
	input any
	want  string // hex after the 6-byte header
}{
	{"g1 false", false, "D764626F6F6C10"},
	{"g2 true", true, "D764626F6F6C11"},
	{"g3 int5", int(5), "D863696E74082A"},
	{"g4 int-1", int(-1), "D863696E740821"},
	{"g5 uint16-300", uint16(300), "D96675696E74313602 3D012C"},
	{"g6 float64-1", float64(1.0), "DA67666C6F6174363408 413FF0000000000000"},
	{"g7 hi", "hi", "DC0C66737472696E67 626869"},
	{"g8 intslice12", []int{1, 2}, "D1655B5D696E74 D863696E7408 82022224 9004"},
	{"g9 elision", []int{5, 0, 0, 0}, "D1655B5D696E74 D863696E7408 84012A 9004"},
	{"g10 lencap", func() []int { s := make([]int, 2, 4); s[0], s[1] = 7, 8; return s }(),
		"D1655B5D696E74 D863696E7408 84022C0E2C10 9204000204"},
	{"g11 nilslice", []int(nil), "D1655B5D696E74 D863696E7408 01"},
	{"g12 dupstr", []string{"dup", "dup"},
		"D1685B5D737472696E67 DC0C66737472696E67 820263647570C5 9004"},
	{"g13 blob", []byte{0xAB, 0xCD}, "DC0D665B5D62797465 7202ABCD 9002"},
	{"g14 blobzeros", []byte{0, 0}, "DC0D665B5D62797465 7200 9002"},
	{"g15 mapsi", map[string]int{"b": 2, "a": 1},
		"D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 A2616122616224"},
	{"g16 mapii", map[int]int{2: 20, 1: 10},
		"D36B6D61705B696E745D696E74 D863696E7408 C2 A2222C14242C28"},
	{"g17 ptr", func() any { p := 5; return &p }(), "D5642A696E74 D863696E7408 2A"},
	{"g18 nilptr", (*int)(nil), "D5642A696E74 D863696E7408 00"},
	{"g19 nilmap", map[string]int(nil),
		"D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 02"},
	{"g20 emptymap", map[string]int{},
		"D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 A0"},
	{"g21 emptyslice", []int{},
		"D1655B5D696E74 D863696E7408 8000 9004"},
	{"g22 struct", Point{X: 1, Y: 2},
		"D06C336769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E506F696E74026158D865696E743634086159C3B02224"},
	{"g23 named", Celsius(7),
		"D46C356769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E43656C73697573DA67666C6F617436340841401C000000000000"},
	{"g24 complex", complex128(complex(1, 2)),
		"DB6A636F6D706C657831323808 513FF00000000000004000000000000000"},
	{"g25 array", [2]int64{1, 2},
		"D2685B325D696E743634 02 D865696E74363408 82022224"},
	// Slice-view grouping + KO-full keys.
	{"g26 views", func() any {
		arr := []int{1, 2, 3, 4}
		return P{A: arr[0:2], B: arr[2:4]}
	}(), "D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E50026141D1655B5D696E74D863696E74086142C3B0840422242628920800020491080202"},
	{"g27 blobviews", func() any {
		b := []byte{0xAB, 0, 0xCD}
		return Q{X: b[0:1], Y: b[1:3]}
	}(), "D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E51026158DC0D665B5D627974656159C3B07303AB00CD920600010391060102"},
	{"g28 elision", func() any {
		big := make([]int, 2, 8)
		big[0] = 5
		return R{W1: big, W2: big[1:2]}
	}(), "D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5202625731D1655B5D696E74D863696E7408625732C3B088012A92080002089208010107"},
	{"g29 ptrkeys", func() any {
		x1, x2 := X{N: 1}, X{N: 1}
		return map[*X]int{&x1: 1, &x2: 2}
	}(), "D36C386D61705B2A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E585D696E74D56C302A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E58D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5801614ED865696E74363408D863696E7408A2B02222B02224"},
	{"g30 arraykeys", map[[2]int64]int{{1, 2}: 10, {2, 1}: 10},
		"D36C10 6D61705B5B325D696E7436345D696E74 D2685B325D696E74363402 D865696E74363408 D863696E7408 A2 820222 242C14 820224 222C14"},
	{"g31 longkey", map[string]int{"longkey_abcdef": 1},
		"D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 A16C0E6C6F6E676B65795F61626364656622"},
	{"g32 cyckey", func() any {
		n := N{}
		n.Next = &n
		return map[*N]int{&n: 1}
	}(), "D36C386D61705B2A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E4E5D696E74D56C302A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E4ED06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E4E01644E657874C2D863696E7408A1B0CA22"},
	{"g33 structkeys", map[Point]int{{X: 1, Y: 2}: 10, {X: 2, Y: 1}: 10},
		"D36C3B6D61705B6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E506F696E745D696E74D06C336769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E506F696E74026158D865696E743634086159C5D863696E7408A2B022242C14B024222C14"},
	// Interface ∃-model.
	{"g34 iface int64", S{V: int64(1)},
		"D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E53016156D66C0C696E74657266616365207B7DB0D865696E7436340822"},
	{"g35 typed nil", S{V: (*Z)(nil)},
		"D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E53016156D66C0C696E74657266616365207B7DB0D56C302A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5AD06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5A0000"},
	{"g36 nil iface", S{},
		"D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E53016156D66C0C696E74657266616365207B7DB003"},
	{"g37 iface map", any(map[string]int{"a": 1}),
		"D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 A1616122"},
	{"g38 iface keys", map[any]int{int64(1): 1, int32(1): 2},
		"D36C14 6D61705B696E74657266616365207B7D5D696E74 D66C0C696E74657266616365207B7D D863696E7408 A2 D865696E74333204 22 24 D865696E74363408 22 22"},
	{"g39 iface ref", func() any { p := &X{N: 1}; return T{A: p, B: p} }(),
		"D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E54026141D66C0C696E74657266616365207B7D6142C3B0D56C302A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E58D06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5801614ED865696E74363408B022C6CC0D"},
	{"g40 nil keys", map[any]int{nil: 1, (*Z)(nil): 2},
		"D36C146D61705B696E74657266616365207B7D5D696E74D66C0C696E74657266616365207B7DD863696E7408A20322D56C302A6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5AD06C2F6769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E5A000024"},
	// Coder dispatch, adapters, time coder, evolution pairs
	// (the current header throughout).
	{"g41 time utc", time.Unix(1, 2).UTC(),
		"DC0E 6974696D652E54696D65 00 22 32 20 63555443"},
	{"g42 time moscow", func() any {
		loc, err := time.LoadLocation("Europe/Moscow")
		if err != nil {
			panic("tzdata unavailable: " + err.Error())
		}
		return time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	}(), "DC0E 6974696D652E54696D65 00 2ED2AB1DA0 30 2D5460 6C0D 4575726F70652F4D6F73636F77"},
	{"g43 coder bin", Bin{N: 0xAB},
		"DC0E6C316769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E42696E007101AB"},
	{"g44 evo new3", EvN{A: 1, B: 5, C: 2},
		"D06C316769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E45764E036141D865696E743634086142C36143C3B0222A24"},
	{"g45 evo old2", EvO{A: 1, C: 2},
		"D06C316769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E45764F026141D865696E743634086143C3B02224"},
	// Map intern: shared, self, mutual; slice/map cycles through any.
	{"g46 mapshare", func() any {
		m := map[string]int{"a": 1}
		return MS{X: m, Y: m}
	}(), "D06C306769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E4D53026158D36C0E6D61705B737472696E675D696E74DC0C66737472696E67D863696E74086159C3B0A1616122CA"},
	{"g47 mapself", func() any {
		m := map[string]any{}
		m["self"] = m
		return m
	}(), "D36C17 6D61705B737472696E675D696E74657266616365207B7D DC0C66737472696E67 D66C0C696E74657266616365207B7D A1 6473656C66 C0 C6"},
	{"g48 mapmutual", func() any {
		m1, m2 := map[string]any{}, map[string]any{}
		m1["a"], m2["b"] = m2, m1
		return m1
	}(), "D36C17 6D61705B737472696E675D696E74657266616365207B7D DC0C66737472696E67 D66C0C696E74657266616365207B7D A1 6161 C0 A1 6162 C0 C6"},
	{"g49 sliceself", func() any {
		s := make([]any, 2)
		s[0], s[1] = int64(1), nil
		s[1] = s
		return s
	}(), "D16C0E5B5D696E74657266616365207B7D D66C0C696E74657266616365207B7D 8202 D865696E74363408 22 C0 9004 9004"},
	{"g50 mapinslice", func() any {
		m := map[string]int{"a": 1}
		return []map[string]int{m, m}
	}(), "D16C105B5D6D61705B737472696E675D696E74 D36C0E6D61705B737472696E675D696E74 DC0C66737472696E67 D863696E7408 8202 A1616122 C9 9008"},
	// Sharing-topology vectors (T0–T3), fixed from the pre-refactor
	// mechanics: T0 disjoint records, T1 exact duplicates, T2 fan chain
	// over one backing, T3 mixed (chain + seam + duplicate + blob pair +
	// zerobase).
	{"t0 disjoint", func() any {
		w0 := []int64{1, 2}
		w1 := []int64{3, 4, 5}
		return [][]int64{w0, w1}
	}(), "D1695B5D5B5D696E743634 D1675B5D696E743634 D865696E74363408 8202 82022224 9007 830326282A 9008 9006"},
	{"t1 duplicates", func() any {
		base := []int64{5, 6, 7, 8}
		v := base[1:3:4]
		return [][]int64{v, v}
	}(), "D1695B5D5B5D696E743634 D1675B5D696E743634 D865696E74363408 8202 83032C0C2C0E2C10 9207 000203 9207 000203 9006"},
	{"t2 fan chain", func() any {
		base := make([]int64, 10)
		for i := range base {
			base[i] = int64(i + 1)
		}
		return [][]int64{base[0:4:4], base[2:6:6], base[4:8:8], base[6:10:10]}
	}(), "D1695B5D5B5D696E743634 D1675B5D696E743634 D865696E74363408 8404 8A0A222426282A2C0C2C0E2C102C122C14 9107 0004 9107 0204 9107 0404 9107 0604 9006"},
	{"t3 mixed", func() any {
		a := make([]int64, 12)
		for i := range a {
			a[i] = int64(i + 1)
		}
		b := []byte{0xAB, 0, 0xCD, 0, 0xEF}
		return topoMix{
			A: [][]int64{a[0:5:5], a[3:8:8], a[8:12:12], a[3:8:8], {9}},
			B: [][]byte{b[0:4:4], b[2:5:5], {}},
		}
	}(), "D06C356769746875622E636F6D2F67626F6E2D666F726D61742F67626F6E2D676F5F746573742E67626F6E5F746573742E746F706F4D6978026141D1695B5D5B5D696E743634D1675B5D696E743634D865696E743634086142D1685B5D5B5D62797465DC0D665B5D62797465B085058808222426282A2C0C2C0E2C10910C0F0005910C0F030584042C122C142C162C18900C10910C0F030581012C12900C11900C0E83037505AB00CD00EF910C130004910C1302037000900C14900C12"},
}

// g51 (crafted ErrBudget vector): a []int64 ARRAY record
// claiming L=10^8 E=0 — 31 bytes of input advertising an 8×10^8-byte
// backing. Decode must refuse with ErrBudget before the MakeSlice; the
// 50 legal vectors above stay byte-stable.
func TestGoldenG51ErrBudget(t *testing.T) {
	const g51 = "67626F6E0000" + // header
		"D1" + "67" + "5B5D696E743634" + // DESC-SLICE id0, "[]int64" id1
		"D8" + "65" + "696E743634" + "08" + // DESC-INT id2, "int64" id3, width 8
		"8E" + "05F5E100" + "00" + // ARRAY id4: L=10^8 (u32), E=0
		"90" + "04" // VIEW form0 → id4
	data, err := hex.DecodeString(g51)
	if err != nil {
		t.Fatalf("g51 hex: %v", err)
	}
	if len(data) != 31 {
		t.Fatalf("g51 = %d bytes, want 31", len(data))
	}
	var v []int64
	if err := gbon.Unmarshal(data, &v); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("Unmarshal(g51): err = %v, want ErrBudget", err)
	}
}

// Fixture types: shared-backing windows and key categories.
type P struct{ A, B []int }

type Q struct{ X, Y []byte }

type R struct{ W1, W2 []int }

type X struct{ N int64 }

type N struct{ Next *N }

// Interface fixtures: Z is the empty-struct fixture
// for typed-nil interface keys.
type S struct{ V any }

type T struct{ A, B any }

type Z struct{}

// Map-intern fixture: two map-typed fields over one map object.
type MS struct{ X, Y map[string]int }

// Coder fixtures: Bin carries the Binary pair (g43 adapter vector), EvN/EvO
// are the g44/g45 evolution pair (equal-length names — the splice fixture
// technique pins the name anchor of the evolution contract).
type Bin struct{ N byte }

func (b Bin) MarshalBinary() ([]byte, error) { return []byte{b.N}, nil }

func (b *Bin) UnmarshalBinary(p []byte) error {
	if len(p) != 1 {
		return fmt.Errorf("Bin wants 1 byte, got %d", len(p))
	}
	b.N = p[0]
	return nil
}

type EvN struct{ A, B, C int64 }

type EvO struct{ A, C int64 }

func TestGoldenMarshal(t *testing.T) {
	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			b := mustMarshal(t, tc.input)
			if !bytes.HasPrefix(b, hdr) {
				t.Fatalf("missing stream header: % x", b)
			}
			wb, err := hex.DecodeString(strings.ReplaceAll(tc.want, " ", ""))
			if err != nil {
				t.Fatalf("bad want hex: %v", err)
			}
			want := hex.EncodeToString(wb)
			if got := hex.EncodeToString(b[6:]); got != want {
				t.Fatalf("bytes after header:\n got  %s\n want %s", got, want)
			}
		})
	}
}

// Cross-value sharing vectors (per-stream backings):
// multi-value Encoder streams where value 2 encounters memory already
// closed by value 1. Full-stream hex including the header; the id-scan
// chains follow the byte order (DESC before name before blob/record).
var goldenCrossValue = []struct {
	name  string
	value func(e *gbon.Encoder)
	want  string // full stream hex
}{
	{"g52 repeat window", func(e *gbon.Encoder) {
		b := []byte{0xAA}
		_ = e.Encode(b)
		_ = e.Encode(b)
	}, "67626F6E0000 DC0D665B5D62797465 7101AA9002 C09002"},
	{"g53 sub window", func(e *gbon.Encoder) {
		v := []byte{01, 02, 03, 00}
		_ = e.Encode(v)
		_ = e.Encode(v[1:3])
	}, "67626F6E0000 DC0D665B5D62797465 74030102039002 C09202010203"},
	{"g54 tail mutation fallback", func(e *gbon.Encoder) {
		v := []byte{01, 02, 00, 00}
		_ = e.Encode(v)
		v[2] = 0xAA
		_ = e.Encode(v)
	}, "67626F6E0000 DC0D665B5D62797465 740201029002 C074030102AA9003"},
	{"g55 zerobase dedup", func(e *gbon.Encoder) {
		_ = e.Encode([]byte{})
		_ = e.Encode([]byte{})
	}, "67626F6E0000 DC0D665B5D62797465 70009002 C09002"},
}

func TestGoldenCrossValue(t *testing.T) {
	for _, tc := range goldenCrossValue {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := gbon.NewEncoder(&buf)
			tc.value(e)
			want, err := hex.DecodeString(strings.ReplaceAll(tc.want, " ", ""))
			if err != nil {
				t.Fatalf("bad want hex: %v", err)
			}
			if got := buf.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("stream bytes:\n got  % x\n want % x", got, want)
			}
		})
	}
}

// nil maps never intern: every nil-map position is the nil token, and a
// repeated nil-map position emits the token again — never a REF.
func TestNilMapNoIntern(t *testing.T) {
	type twoMaps struct{ A, B map[string]int }
	b := mustMarshal(t, twoMaps{})
	if bytes.Contains(b, []byte{0xCA}) || bytes.Contains(b, []byte{0xC2}) {
		t.Fatalf("nil-map stream must carry no REF: % x", b)
	}
	// Exactly two nil-map selectors (02) after the two field-name
	// literals: body = B0 02 02.
	if !bytes.HasSuffix(b, []byte{0xB0, 0x02, 0x02}) {
		t.Fatalf("nil-map body must be B0 02 02: % x", b)
	}
	var out twoMaps
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.A != nil || out.B != nil {
		t.Fatal("nil-ness lost")
	}
}

// C2 numeric vectors: the BIGINT kind over the whole integer domain.
// Expected bytes are hand-computed from the format tables — the spec
// tables are the test oracle, the encoder is what gets checked.
// Marshal output carries the current header; crafted decode-only
// vectors pin their own header bytes.

var goldenBigint = []struct {
	name  string
	input any
	want  string // hex after the 6-byte header
}{
	{"V1 big 2^64", new(big.Int).SetBytes([]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0}),
		"DC0F 67 6269672E496E74 10 09 02 0000000000000000"},
	{"V2 big -(2^64+1)", func() any {
		v := new(big.Int).SetBytes([]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 1})
		return v.Neg(v)
	}(), "DC0F 67 6269672E496E74 10 09 02 0000000000000001"},
	{"V3 big 5", big.NewInt(5), "DC0F 67 6269672E496E74 0A"},
	{"V4 big intern", []any{big.NewInt(1), big.NewInt(2)},
		"D1 6C0E 5B5D696E74657266616365207B7D D6 6C0C 696E74657266616365207B7D 82 02 DC0F 67 6269672E496E74 02 C5 04 90 04"},
}

func TestGoldenBigintMarshal(t *testing.T) {
	for _, tc := range goldenBigint {
		t.Run(tc.name, func(t *testing.T) {
			b := mustMarshal(t, tc.input)
			if !bytes.HasPrefix(b, hdr) {
				t.Fatalf("missing stream header: % x", b)
			}
			if b[5] != 0x00 {
				t.Fatalf("header minor = %#x, want 00", b[5])
			}
			wb, err := hex.DecodeString(strings.ReplaceAll(tc.want, " ", ""))
			if err != nil {
				t.Fatalf("bad want hex: %v", err)
			}
			if got := hex.EncodeToString(b[6:]); got != hex.EncodeToString(wb) {
				t.Fatalf("bytes after header:\n got  %s\n want %s", got, tc.want)
			}
			// every golden vector doubles as a round-trip fixture: the
			// interface-list V4 included ([]any holds *big.Int).
			rt := reflect.New(reflect.TypeOf(tc.input))
			if err := gbon.Unmarshal(b, rt.Interface()); err != nil {
				t.Fatalf("round-trip decode: %v", err)
			}
		})
	}
}

// V0L: the kind-14 adapter stream for a big integer (CODER kind 14,
// STRING body) decodes identically forever, and the re-encode of the
// read value is the canonical kind-15 spelling (evolution).
func TestGoldenV0LLegacyBigint(t *testing.T) {
	const v0l = "67626F6E0000" + // header
		"DC0E 67 6269672E496E74 00" + // CODER kind 14, name, tag 0
		"61 35" // STRING body "5"
	data, err := hex.DecodeString(strings.ReplaceAll(v0l, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	// value target: decodes the same as the kind-15 spelling
	var got big.Int
	if err := gbon.Unmarshal(data, &got); err != nil || got.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("V0L value decode: %v %v", err, &got)
	}
	// pointer target: pointer-ness is absorbed by the kind's name
	var gotp *big.Int
	if err := gbon.Unmarshal(data, &gotp); err != nil || gotp == nil || gotp.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("V0L pointer decode: %v %v", err, gotp)
	}
	// re-encode of the adapter-read value is the canonical kind-15 spelling
	cb := mustMarshal(t, gotp)
	want := append(append([]byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00},
		0xDC, 0x0F, 0x67, 'b', 'i', 'g', '.', 'I', 'n', 't'), 0x0A)
	if !bytes.Equal(cb, want) {
		t.Fatalf("re-encode:\n got  % x\n want % x", cb, want)
	}
}

// V5: decimal128 is specified (FLOAT form 2, 16 raw bytes) with no Go
// projection — a materializing decode is a loud named error; a skipped
// field consumes grammatically.
func TestGoldenV5Decimal(t *testing.T) {
	// V5A: root float64 descriptor of width 16 — the name matches, the
	// width lands in the deliberate match-layer branch.
	v5a, err := hex.DecodeString(strings.ReplaceAll("67626F6E0000"+
		"DA 67 666C6F61743634 0C 10"+ // FLOAT "float64", width 16
		"42"+strings.Repeat("00", 16), " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	var f float64
	err = gbon.Unmarshal(v5a, &f)
	var d128 *gbon.Error
	if err == nil || !errors.As(err, &d128) || d128.Class() != "unsupported_kind" || !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("V5A: want pinned decimal128 unsupported_kind error, got %v", err)
	}
	var a any
	if err := gbon.Unmarshal(v5a, &a); err == nil || !errors.As(err, &d128) || d128.Class() != "unsupported_kind" {
		t.Fatalf("V5A interface target: want unsupported_kind error, got %v", err)
	}

	// V5B: struct stream where field D (decimal, distinct name) is absent
	// from the target — decode succeeds, D is skipped by grammar.
	v5b, err := hex.DecodeString(strings.ReplaceAll("67626F6E0000"+
		"D0 61 53 02"+ // STRUCT "S", 2 fields
		"61 58 DA 67 666C6F61743634 08"+ // "X": FLOAT "float64" w8
		"61 44 DA 63 646563 0C 10"+ // "D": FLOAT "dec" w16
		"B0"+ // STRUCT token
		"41 3FF0000000000000"+ // X = 1.0
		"42"+strings.Repeat("00", 16), " ", "")) // D = decimal128 +0, skipped
	if err != nil {
		t.Fatal(err)
	}
	type decSkip struct{ X float64 }
	d := gbon.NewDecoder(bytes.NewReader(v5b))
	if err := d.RegisterAs("S", decSkip{}); err != nil {
		t.Fatal(err)
	}
	var ds decSkip
	if err := d.Decode(&ds); err != nil {
		t.Fatalf("V5B decode: %v", err)
	}
	if ds.X != 1.0 {
		t.Fatalf("V5B X = %v, want 1.0", ds.X)
	}
}

// Negative-path oracle for the reserved and unknown token classes:
// each rejects exactly at its token.
func TestGoldenNegativeC2(t *testing.T) {
	unhex := func(h string) []byte {
		b, err := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// N1: a decoder rejects a reserved descriptor kind on its kind byte —
	// kinds 16..255 are reserved.
	live := unhex("67626F6E0000 DC10 67 6269672E496E74 0A")
	if kind := live[7]; kind < 16 {
		t.Fatalf("N1 fixture malformed: kind byte %#x", kind)
	}
	var lv any
	if err := gbon.Unmarshal(live, &lv); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("N1 reserved kind 16: want ErrFormat, got %v", err)
	}

	// N2: an unknown float form rejects at the token — the materializing
	// float readers accept only forms 0 and 1.
	n2 := unhex("67626F6E0000 DA 67 666C6F61743634 08 42" + strings.Repeat("00", 16))
	var f2 float64
	if err := gbon.Unmarshal(n2, &f2); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("N2: want ErrFormat, got %v", err)
	}

	// N3: the ext selector is illegal in a bare length position — an
	// unknown argument form, rejected at the E field of the ARRAY record.
	n3 := unhex("67626F6E0000" +
		"D1 67 5B5D696E743634 D8 65 696E743634 08" +
		"82" + "10 09 02 0000000000000000") // L=2, E in ext form
	var s3 []int64
	if err := gbon.Unmarshal(n3, &s3); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("N3: want ErrFormat, got %v", err)
	}

	// N4: non-minimal ext bodies reject — byte count below 9, and a zero
	// leading byte (the value then fits a narrower form).
	mkBig := func(body string) []byte {
		return unhex("67626F6E0000 DC0F 67 6269672E496E74 " + body)
	}
	var b4 *big.Int
	if err := gbon.Unmarshal(mkBig("10 08 0200000000000000"), &b4); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("N4 short ext: want ErrFormat, got %v", err)
	}
	if err := gbon.Unmarshal(mkBig("10 09 0000000000000001"), &b4); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("N4 zero-leading ext: want ErrFormat, got %v", err)
	}

	// N5: an advertised ext length beyond the byte budget fails by
	// budget before any allocation.
	n5 := unhex("67626F6E0000 DC0F 67 6269672E496E74 10 0F 0000010000000000")
	var b5 *big.Int
	if err := gbon.Unmarshal(n5, &b5); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("N5: want ErrBudget, got %v", err)
	}
}

// Nil big integers round-trip through their pointer projection: the
// zero/nil coincidence byte of the BIGINT body materializes nil for
// pointer and interface shapes (named shapes).
func TestBigintNilRoundtrip(t *testing.T) {
	type nilBig struct {
		P *big.Int
		V any
	}
	in := nilBig{P: nil, V: (*big.Int)(nil)}
	b := mustMarshal(t, in)
	var out nilBig
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.P != nil {
		t.Fatalf("pointer nil lost: %v", out.P)
	}
	pv, _ := out.V.(*big.Int)
	if out.V == nil || pv != nil {
		t.Fatalf("interface typed nil lost: %s", safeDescValue(out.V))
	}
}

// TestSafeDescCyclicGolden: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicGolden(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	s := safeDescValue(m)
	if len(s) == 0 || len(s) > descMaxRunes {
		t.Fatalf("safeDescValue render out of bounds (%d runes): %q", len(s), s)
	}
	if !strings.Contains(s, "<cycle>") {
		t.Fatalf("safeDescValue misses the cycle marker: %q", s)
	}
}
