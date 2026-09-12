package gbon

import (
	"bytes"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// internPoolShared is a named struct whose type name doubles as plain
// string content in the scenes below: one content reaches the stream
// through the string-value role and the descriptor-name role.
type internPoolShared struct{ X int }

func internMustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// TestInternStringValueSharesDescNamePool drives the shared content
// through both slice orders and the map-key role: every order encodes
// and decodes back through a registered decoder.
func TestInternStringValueSharesDescNamePool(t *testing.T) {
	str := "internPoolShared"
	b1 := internMustMarshal(t, []any{str, internPoolShared{X: 7}})
	b2 := internMustMarshal(t, []any{internPoolShared{X: 7}, str})
	if bytes.Equal(b1, b2) {
		t.Fatal("the two orders must produce different bytes")
	}

	decode := func(b []byte) []any {
		t.Helper()
		dec := NewDecoder(bytes.NewReader(b))
		if err := dec.Register(internPoolShared{}); err != nil {
			t.Fatalf("register: %v", err)
		}
		var out []any
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	out1 := decode(b1)
	out2 := decode(b2)

	if s, ok := out1[0].(string); !ok || s != str {
		t.Fatalf("out1[0] = %#v, want string %q", out1[0], str)
	}
	if s, ok := out2[1].(string); !ok || s != str {
		t.Fatalf("out2[1] = %#v, want string %q", out2[1], str)
	}
	if _, ok := out1[1].(internPoolShared); !ok {
		t.Fatalf("out1[1] = %T, want internPoolShared", out1[1])
	}
	if !reflect.DeepEqual(out1[1], out2[0]) {
		t.Fatalf("struct elements differ across orders: %#v vs %#v", out1[1], out2[0])
	}

	// Map-key role: the shared content as a key, the named type as the
	// value it names.
	bm := internMustMarshal(t, map[string]any{str: internPoolShared{X: 9}})
	decM := NewDecoder(bytes.NewReader(bm))
	if err := decM.Register(internPoolShared{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var outm map[string]any
	if err := decM.Decode(&outm); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if len(outm) != 1 {
		t.Fatalf("map decoded %d pairs, want 1", len(outm))
	}
	if v, ok := outm[str].(internPoolShared); !ok || v.X != 9 {
		t.Fatalf("map value = %#v, want internPoolShared{9}", outm[str])
	}
}

// TestInternRoundTripLenEdgesAndRepeats round-trips short-string
// edges with repeats through slices and maps; every re-encode is
// byte-stable.
func TestInternRoundTripLenEdgesAndRepeats(t *testing.T) {
	vals := []string{"", "a", "ab", "abc", "abcd", "\xff\x00\xfe", "a", "", "abcd", "\xff\x00\xfe"}
	b := internMustMarshal(t, vals)
	var out []string
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal slice: %v", err)
	}
	if !slices.Equal(vals, out) {
		t.Fatalf("slice round-trip: %q", out)
	}
	if b2 := internMustMarshal(t, out); !bytes.Equal(b, b2) {
		t.Fatal("slice re-encode is not byte-stable")
	}

	m := map[string]string{"": "", "same": "same", "k": "v", "v": "k", "ab": "ab"}
	bm := internMustMarshal(t, m)
	var outm map[string]string
	if err := Unmarshal(bm, &outm); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	if !reflect.DeepEqual(m, outm) {
		t.Fatalf("map round-trip: %#v", outm)
	}
	if bm2 := internMustMarshal(t, outm); !bytes.Equal(bm, bm2) {
		t.Fatal("map re-encode is not byte-stable")
	}
}

// TestInternRoundTripGrowthCrossing round-trips a corpus crossing
// the intern table's power-of-two growth thresholds several times,
// unique keys over a repeating value pool.
func TestInternRoundTripGrowthCrossing(t *testing.T) {
	m := make(map[string]string, 320)
	for i := range 320 {
		m["key"+strconv.Itoa(i)] = "val" + strconv.Itoa(i%50)
	}
	b := internMustMarshal(t, m)
	var out map[string]string
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(m, out) {
		t.Fatalf("round-trip mismatch: %d pairs back, want %d", len(out), len(m))
	}
	if b2 := internMustMarshal(t, out); !bytes.Equal(b, b2) {
		t.Fatal("re-encode is not byte-stable")
	}
}
