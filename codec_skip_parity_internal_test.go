package gbon

import (
	"bytes"
	"reflect"
	"testing"
)

// Byte-position parity oracle: skipValue and the matching decodeX arm
// consume the same encoder-emitted token form from the same stream
// position. Twin decoders read identical whole-value streams (the
// stream header consumed by the stream-decoder init); the invariant is
// the reader position after the value and full-stream consumption on
// both sides.

// bpDecoder builds a bare per-value decoder over the full stream bytes:
// readHeader consumes the header and the decoder starts at the first
// token of the root record.
func bpDecoder(b []byte) *codecDecoder {
	sd := &codecStreamDecoder{fac: &Decoder{}}
	sd.init(bytes.NewReader(b))
	if err := sd.readHeader(); err != nil {
		panic(err)
	}
	return newBudgetDecoder(sd.r, effLimits(Limits{}), nil, nil, nil, nil, nil, nil, nil)
}

// bpValue runs the twin probe over one encoder-emitted value: both
// twins read the root descriptor, then skipValue and decodeBody consume
// the value to the same end position.
func bpValue(t *testing.T, label string, v any, rt reflect.Type) {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	dSkip := bpDecoder(b)
	descS, err := dSkip.r.ReadDesc()
	if err != nil {
		t.Fatalf("%s: skip-side root descriptor: %v", label, err)
	}
	if err := dSkip.skipValue(descS, "$"); err != nil {
		t.Fatalf("%s: skipValue: %v (pos %d/%d)", label, err, dSkip.r.Pos(), len(b))
	}
	dDec := bpDecoder(b)
	descD, err := dDec.r.ReadDesc()
	if err != nil {
		t.Fatalf("%s: decode-side root descriptor: %v", label, err)
	}
	if err := dDec.decodeBody(descD, reflect.New(rt).Elem(), pathNode{idx: -1}); err != nil {
		t.Fatalf("%s: decodeBody: %v (pos %d/%d)", label, err, dDec.r.Pos(), len(b))
	}
	if ps, pd := dSkip.r.Pos(), dDec.r.Pos(); ps != pd {
		t.Fatalf("%s: positions differ after the value: skip %d, decode %d", label, ps, pd)
	}
	if dSkip.r.Pos() != len(b) {
		t.Fatalf("%s: skip left %d of %d bytes unconsumed", label, len(b)-dSkip.r.Pos(), len(b))
	}
}

// TestSkipParityBytePosition covers every encoder-emitted token form at
// a droppable position — literals, REF repeats, nil selectors, views,
// container bodies, the map-REF form, grain-tagged rings.
func TestSkipParityBytePosition(t *testing.T) {
	t.Run("b1 string literal", func(t *testing.T) {
		bpValue(t, "b1", "bp literal", reflect.TypeFor[string]())
	})
	t.Run("b2 string ref", func(t *testing.T) {
		bpValue(t, "b2", [2]string{"dup", "dup"}, reflect.TypeFor[[2]string]())
	})
	t.Run("b3 nil selector", func(t *testing.T) {
		var nilMap map[string]int64
		bpValue(t, "b3", nilMap, reflect.TypeFor[map[string]int64]())
	})
	t.Run("b4 view over consumed backing", func(t *testing.T) {
		back := []int64{1, 2, 3, 4}
		bpValue(t, "b4", bpViews{B: back, W: back[1:3]}, reflect.TypeFor[bpViews]())
	})
	t.Run("b5 map header and pair bodies", func(t *testing.T) {
		bpValue(t, "b5", map[string]int64{"k": 7}, reflect.TypeFor[map[string]int64]())
	})
	t.Run("b6 ref-to-map", func(t *testing.T) {
		m := map[string]int64{"k": 7}
		bpValue(t, "b6", bpSharedMap{F1: m, F2: m}, reflect.TypeFor[bpSharedMap]())
	})
	t.Run("b7 struct header and field bodies", func(t *testing.T) {
		bpValue(t, "b7", bpFields{A: "f", B: 9}, reflect.TypeFor[bpFields]())
	})
	t.Run("b8 grain-tag ref ring", func(t *testing.T) {
		g := bpRing{Q: 3}
		g.P = &g
		bpValue(t, "b8", &g, reflect.TypeFor[*bpRing]())
	})
}

type bpViews struct{ B, W []int64 }
type bpSharedMap struct{ F1, F2 map[string]int64 }
type bpFields struct {
	A string
	B int64
}
type bpRing struct {
	P *bpRing
	Q int64
}
