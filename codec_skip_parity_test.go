package gbon_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// Parity oracle of the skip path over shared substructure: narrowing
// differentials where dropped fields carry the records the encoder
// allocated and kept positions resolve what remains materializable.
// Evolution pairs bind one wire name to the wide encoder-side type and
// the narrow decoder-side type; RegisterAs examples take the value
// form. A kept position resolving a REF or VIEW into a record the
// narrow target skipped rejects loud (bad_ref), and the skipped record
// itself stays unmaterialized.

// narEncode marshals v under the pair's wire name (encoder side).
func narEncode(t *testing.T, name string, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	if err := e.RegisterAs(name, v); err != nil {
		t.Fatal(err)
	}
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// narDecode decodes b into a fresh narrow target bound to the wire name.
func narDecode(t *testing.T, name string, ex any, b []byte) (any, error) {
	t.Helper()
	d := gbon.NewDecoder(bytes.NewReader(b))
	if err := d.RegisterAs(name, ex); err != nil {
		t.Fatal(err)
	}
	out := reflect.New(reflect.TypeOf(ex)).Interface()
	err := d.Decode(out)
	return reflect.ValueOf(out).Elem().Interface(), err
}

// narWantBadRef asserts the reject is a bad_ref carrying the sanctioned
// fragment verbatim; anything else is a third outcome.
func narWantBadRef(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want bad_ref (%q), got success", fragment)
	}
	ge := &gbon.Error{}
	if !errors.As(err, &ge) {
		t.Fatalf("want a *gbon.Error, got %T: %v", err, err)
	}
	if ge.Class() != "bad_ref" {
		t.Fatalf("want class bad_ref, got %q: %v", ge.Class(), err)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("reject %q misses the sanctioned fragment %q", err.Error(), fragment)
	}
}

// --- fixtures ---

type narMapW struct{ F1, F2 map[string]int }
type narMapKeep1 struct{ F1 map[string]int }
type narMapKeep2 struct{ F2 map[string]int }
type narMapNone struct{}

type narIfaceW struct {
	Keep map[string]int
	Hold any
}
type narIfaceKeep struct{ Keep map[string]int }

type narWrongK struct{ M map[string]int64 }

// A hand-built stream: the grouped byte fields open one BLOB record
// (id 13) and the trailing map position carries a REF naming that blob
// record instead of a map record.
const narWrongSortStream = "67626f6e0001d06c0c73702e77726f6e67736f7274036153dc0d665b5d627974656152c3614dd36c106d61705b737472696e675d696e743634dc0c66737472696e67d865696e74363408b0740401020304900c0d920c0d010203cc0d"

// TestSkipParityNarrowMapRef drives the map-position REF shapes of the
// skip path: consume a map-record REF (the interface recursion
// included), reject a wrong-sort REF typed, skip a plain map record.
func TestSkipParityNarrowMapRef(t *testing.T) {
	m := map[string]int{"k": 1, "x": 2}

	t.Run("consume shared-map REF in dropped map position", func(t *testing.T) {
		b := narEncode(t, "narrow.map", narMapW{F1: m, F2: m})
		got, err := narDecode(t, "narrow.map", narMapKeep1{}, b)
		if err != nil {
			t.Fatalf("narrow decode: %v", err)
		}
		if d := got.(narMapKeep1); !reflect.DeepEqual(d.F1, m) {
			t.Fatalf("projection: got %v want %v", d.F1, m)
		}
	})

	t.Run("iface-held shared map, REF skipped through KindInterface", func(t *testing.T) {
		b := narEncode(t, "narrow.iface", narIfaceW{Keep: m, Hold: m})
		got, err := narDecode(t, "narrow.iface", narIfaceKeep{}, b)
		if err != nil {
			t.Fatalf("narrow decode: %v", err)
		}
		if d := got.(narIfaceKeep); !reflect.DeepEqual(d.Keep, m) {
			t.Fatalf("projection: got %v want %v", d.Keep, m)
		}
	})

	t.Run("wrong-sort REF rejects typed at the REF offset", func(t *testing.T) {
		b, err := hex.DecodeString(narWrongSortStream)
		if err != nil {
			t.Fatal(err)
		}
		_, derr := narDecode(t, "sp.wrongsort", narWrongK{}, b)
		narWantBadRef(t, derr, "ref 13 is not a map record")
		ge := &gbon.Error{}
		if !errors.As(derr, &ge) {
			t.Fatal(derr)
		}
		if ge.Offset != 92 {
			t.Fatalf("reject offset %d, want 92 (the REF token)", ge.Offset)
		}
	})

	t.Run("distinct maps, plain record skip", func(t *testing.T) {
		other := map[string]int{"z": 9}
		b := narEncode(t, "narrow.map", narMapW{F1: m, F2: other})
		got, err := narDecode(t, "narrow.map", narMapKeep1{}, b)
		if err != nil {
			t.Fatalf("narrow decode: %v", err)
		}
		if d := got.(narMapKeep1); !reflect.DeepEqual(d.F1, m) {
			t.Fatalf("projection: got %v want %v", d.F1, m)
		}
	})
}

// --- differential corpus fixtures ---

type diffC2W struct{ B, W []int64 }
type diffC2WOnly struct{ W []int64 }
type diffC2N struct{}

type diffRing struct {
	V    int64
	Next *diffRing
}

type diffC3W struct{ A, B *diffRing }
type diffC3B struct{ B *diffRing }
type diffC3N struct{}

type diffC4W struct{ S1, S2, S3 string }
type diffC4S23 struct{ S2, S3 string }
type diffC4S3 struct{ S3 string }
type diffC4N struct{}

type diffC5W struct {
	T1, T2 time.Time
	M1, M2 map[string]int
}
type diffC5T2 struct {
	T2     time.Time
	M1, M2 map[string]int
}
type diffC5M struct {
	M1, M2 map[string]int
}
type diffC5M2 struct {
	M2 map[string]int
}
type diffC5N struct{}

type diffC6Outer struct{ M map[string]int }
type diffC6W struct{ A, B diffC6Outer }
type diffC6B struct{ B diffC6Outer }
type diffC6N struct{}

type diffCase struct {
	name string
	wire string
	src  any
	rows []diffRow
}

type diffRow struct {
	kept string
	dst  any
	// want is the projection of the full decode onto the kept fields;
	// fragment non-empty expects the A7/A8 bad_ref wording instead.
	want     any
	fragment string
}

// TestSkipParityNarrowingDifferential walks the declared sharing-shape
// corpus over prefix-closed dropped-field subsets: each row yields the
// projection of the full decode or the sanctioned bad_ref wording.
func TestSkipParityNarrowingDifferential(t *testing.T) {
	m := map[string]int{"k": 1, "x": 2}
	back := []int64{1, 2, 3, 4}
	now := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	r1 := diffRing{V: 1}
	r1.Next = &r1
	r2 := diffRing{V: 2}
	r2.Next = &r2
	inner := map[string]int{"i": 9}

	cases := []diffCase{
		{
			name: "c1 repeated maps",
			wire: "diff.c1",
			src:  narMapW{F1: m, F2: m},
			rows: []diffRow{
				{kept: "F1", dst: narMapKeep1{}, want: narMapKeep1{F1: m}},
				{kept: "F2", dst: narMapKeep2{}, fragment: "map record 10 is not materialized"},
				{kept: "-", dst: narMapNone{}},
			},
		},
		{
			name: "c2 shared backing",
			wire: "diff.c2",
			src:  diffC2W{B: back, W: back[1:3]},
			rows: []diffRow{
				{kept: "W", dst: diffC2WOnly{}, fragment: "shared backing 8 unavailable for []int64"},
				{kept: "-", dst: diffC2N{}},
			},
		},
		{
			name: "c3 ring",
			wire: "diff.c3",
			src:  diffC3W{A: &r1, B: &r2},
			rows: []diffRow{
				{kept: "B", dst: diffC3B{}, want: diffC3B{B: &r2}},
				{kept: "-", dst: diffC3N{}},
			},
		},
		{
			name: "c4 repeated strings",
			wire: "diff.c4",
			src:  diffC4W{S1: "dup", S2: "dup", S3: "dup"},
			rows: []diffRow{
				{kept: "S2,S3", dst: diffC4S23{}, want: diffC4S23{S2: "dup", S3: "dup"}},
				{kept: "S3", dst: diffC4S3{}, want: diffC4S3{S3: "dup"}},
				{kept: "-", dst: diffC4N{}},
			},
		},
		{
			name: "c5 time fields beside aliased maps",
			wire: "diff.c5",
			src:  diffC5W{T1: now, T2: now, M1: m, M2: m},
			rows: []diffRow{
				{kept: "T2,M1,M2", dst: diffC5T2{}, want: diffC5T2{T2: now, M1: m, M2: m}},
				{kept: "M1,M2", dst: diffC5M{}, want: diffC5M{M1: m, M2: m}},
				{kept: "M2", dst: diffC5M2{}, fragment: "map record 15 is not materialized"},
				{kept: "-", dst: diffC5N{}},
			},
		},
		{
			name: "c6 map-in-map",
			wire: "diff.c6",
			src:  diffC6W{A: diffC6Outer{M: inner}, B: diffC6Outer{M: inner}},
			rows: []diffRow{
				{kept: "B", dst: diffC6B{}, fragment: "map record 13 is not materialized"},
				{kept: "-", dst: diffC6N{}},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := narEncode(t, c.wire, c.src)
			// full decode: the projection oracle source
			full, err := narDecode(t, c.wire, c.src, b)
			if err != nil {
				t.Fatalf("full decode: %v", err)
			}
			if !reflect.DeepEqual(full, c.src) {
				t.Fatalf("full decode diverged: got %s want %s", safeDescValue(full), safeDescValue(c.src))
			}
			for _, r := range c.rows {
				t.Run("keep "+r.kept, func(t *testing.T) {
					got, derr := narDecode(t, c.wire, r.dst, b)
					if r.fragment != "" {
						narWantBadRef(t, derr, r.fragment)
						return
					}
					if derr != nil {
						t.Fatalf("third outcome: want projection, got %v", derr)
					}
					want := r.want
					if want == nil {
						want = reflect.Zero(reflect.TypeOf(r.dst)).Interface()
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("projection: got %s want %s", safeDescValue(got), safeDescValue(want))
					}
				})
			}
		})
	}
}
