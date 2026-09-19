package gbon_test

// K2 error-golden corpus (program gbon-defcells-closure, charter A4):
// one row per touched error surface of C1-C4, pinning the realized
// truth — class, sentinel, absolute offset, path, detail shape — so
// wire revisions cannot drift silently (the M8-family tripwire).
// Offsets are hand-derived from the wire grammar (arithmetic commented
// per row); definitional fragments are pinned from the errors.go class
// table plus a recorded run (impl-ledger RL records).

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// egCheck asserts one golden row and echoes the realized shape for the
// ledger. off < 0 skips the offset pin; path "" skips the path pin.
func egCheck(t *testing.T, err error, class string, sent error, off int, path, frag string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got success", class)
	}
	var ae *gbon.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *gbon.Error, got %T: %v", err, err)
	}
	if ae.Class() != class {
		t.Fatalf("class = %q, want %q (%v)", ae.Class(), class, err)
	}
	if !errors.Is(err, sent) {
		t.Fatalf("sentinel: %v not Is %v", err, sent)
	}
	if off >= 0 && ae.Offset != off {
		t.Fatalf("offset = %d, want %d", ae.Offset, off)
	}
	if path != "" && ae.Path != path {
		t.Fatalf("path = %q, want %q", ae.Path, path)
	}
	if frag != "" && !strings.Contains(err.Error(), frag) {
		t.Fatalf("text %q misses fragment %q", err.Error(), frag)
	}
	t.Logf("egRow class=%s off=%d path=%q got=%v text=%q", ae.Class(), ae.Offset, ae.Path, ae.Got, err.Error())
}

// egPinNoSite pins the lettered no-site axes of the register_conflict
// rows — Offset exactly -1 and empty Path — as assertions, distinct
// from egCheck's off < 0 / path "" skip sentinels.
func egPinNoSite(t *testing.T, err error) {
	t.Helper()
	var ae *gbon.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *gbon.Error, got %T: %v", err, err)
	}
	if ae.Offset != -1 || ae.Path != "" {
		t.Fatalf("no-site pins: offset = %d, path = %q; want -1, \"\"", ae.Offset, ae.Path)
	}
}

// egAbsentBox types feed the unknown_name probe; egA/egB feed the
// register_conflict family.
type egA struct{ N int64 }
type egB struct{ S string }

// egBoomCoder panics inside EncodeValue — the C4 sticky-encode probe.
type egBoomBox struct{ N int64 }

type egBoomCoder struct{}

func (egBoomCoder) EncodeValue(*gbon.Encoder, reflect.Value) error { panic("EGBOOM") }

func (egBoomCoder) DecodeValue(*gbon.Decoder, reflect.Value) error { return nil }

func TestErrorGoldCorpus(t *testing.T) {
	t.Run("c1_budget_charge_single", func(t *testing.T) {
		// header 6 + DESC-BLOB {DC 0D, 66 "[]byte"} 9 + BLOB {7C 64, 00} 3
		// = charge site 18 (after the L/E args, per the PB6 letter).
		c := newCraft()
		c.descPos(dBlob)
		r := c.blobRec(100, 0, nil)
		afterCharge := len(c.buf) // grammar pin: 18
		c.view0(r)
		dec := gbon.NewDecoder(bytes.NewReader(c.buf))
		dec.SetLimits(gbon.Limits{MaxBytes: 50})
		var b []byte
		egCheck(t, dec.Decode(&b), "budget_bytes", gbon.ErrBudget, afterCharge, "$", "exceeds MaxBytes budget")
	})
	t.Run("c1_budget_charge_multirecord_absolute", func(t *testing.T) {
		// rec1: DESC 9 + BLOB {7C 14, 14} + 20 dense + VIEW {90 02} = 40;
		// rec2: DESC-REF C0 1 + BLOB {7C 64, 00} 3 → charge at 45 —
		// strictly beyond rec1 (a window-relative site would be ≤ 5).
		c := newCraft()
		c.descPos(dBlob)
		r1 := c.blobRec(20, 20, bytes.Repeat([]byte{0x61}, 20))
		c.view0(r1)
		rec2base := len(c.buf) // grammar pin: 41 (rec1 E=20 rides the u8 ARG form)
		c.descPos(dBlob)
		r2 := c.blobRec(100, 0, nil)
		afterCharge := len(c.buf) // grammar pin: 45
		c.view0(r2)
		dec := gbon.NewDecoder(bytes.NewReader(c.buf))
		dec.SetLimits(gbon.Limits{MaxBytes: 64})
		var b []byte
		if err := dec.Decode(&b); err != nil {
			t.Fatalf("record 1 must decode clean: %v", err)
		}
		egCheck(t, dec.Decode(&b), "budget_bytes", gbon.ErrBudget, afterCharge, "$", "exceeds MaxBytes budget")
		if afterCharge <= rec2base {
			t.Fatalf("charge site %d not beyond record 1 (%d)", afterCharge, rec2base)
		}
	})
	t.Run("c1_trunc_absolute_eof", func(t *testing.T) {
		// header 6 + DESC-STRING 9 + STR {6C 64} 2: the short-body guard
		// fires at the length-arg end 17 (the >largeRead EOF-site letter
		// is pinned by TestBudgetTRUNCOffsetAbsolute; this row pins the
		// buffered small-input site).
		c := newCraft()
		c.descPos(dString)
		c.tokenArg(0x6, 100)
		c.buf = append(c.buf, bytes.Repeat([]byte{0x61}, 10)...)
		dec := gbon.NewDecoder(bytes.NewReader(c.buf))
		dec.SetLimits(gbon.Limits{MaxBytes: 1 << 20})
		var s string
		egCheck(t, dec.Decode(&s), "truncated", gbon.ErrFormat, 17, "", "truncated body")
	})
	t.Run("c2_unknown_name_untrusted", func(t *testing.T) {
		// header 6 + DESC-NAMED {D4, 69 "eg.Absent", D8 65 "int64" 08}
		// 19 → fail site 25, before the value token.
		c := newCraft()
		c.descPos(dNamed("eg.Absent", dInt64))
		failSite := len(c.buf) // grammar pin: 25
		c.intTok(5)
		dec := gbon.NewDecoder(bytes.NewReader(c.buf))
		var v any
		err := dec.Decode(&v)
		egCheck(t, err, "unknown_name", gbon.ErrFormat, failSite, "$", "interface concrete type not registered: use Decoder.Register")
		if strings.Contains(err.Error(), "eg.Absent") {
			t.Fatalf("untrusted render leaks the input-derived name literal: %v", err)
		}
	})
	t.Run("c2_unknown_name_trusted_two_mode", func(t *testing.T) {
		c := newCraft()
		c.descPos(dNamed("eg.Absent", dInt64))
		failSite := len(c.buf)
		c.intTok(5)
		dec := gbon.NewDecoder(bytes.NewReader(c.buf))
		dec.SetTrustedInput(true)
		var v any
		egCheck(t, dec.Decode(&v), "unknown_name", gbon.ErrFormat, failSite, "$", `(missing name "eg.Absent")`)
	})
	t.Run("c2_register_conflict_name", func(t *testing.T) {
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		if err := enc.RegisterAs("eg.name", egA{}); err != nil {
			t.Fatal(err)
		}
		err := enc.RegisterAs("eg.name", egB{})
		egCheck(t, err, "register_conflict", gbon.ErrUnsupported, -1, "", "already bound to")
		egPinNoSite(t, err)
	})
	t.Run("c2_register_conflict_type", func(t *testing.T) {
		d := gbon.NewDecoder(bytes.NewReader(nil))
		if err := d.RegisterAs("eg.n1", egA{}); err != nil {
			t.Fatal(err)
		}
		err := d.RegisterAs("eg.n2", egA{})
		egCheck(t, err, "register_conflict", gbon.ErrUnsupported, -1, "", "already bound to wire name")
		egPinNoSite(t, err)
	})
}

// Spec-vector bytes (V-115..V-117) embedded verbatim from the spec repo
// vectors/aliased.json — an independent source, not implementation
// output. Narrow targets mirror the vector bindings.
const (
	egV115 = "67626f6e0001d06c0c7665632e6d6170736861726502624631d36c106d61705b737472696e675d696e743634dc0c66737472696e67d865696e74363408624632c3b0a2616b22617824ca"
	egV116 = "67626f6e0001d06c0d7665632e766965777368617265026142d1675b5d696e743634d865696e743634086157c3b084042c142c282c3c2c5090089208010203"
	egV117 = "67626f6e0001d06c0d7665632e77726f6e67736f7274036153dc0d665b5d627974656152c3614dd36c106d61705b737472696e675d696e743634dc0c66737472696e67d865696e74363408b0740401020304900c0d920c0d010203cc0d"
)

type egNarrowMapShare struct{ F2 map[string]int64 }
type egNarrowViewShare struct{ W []int64 }
type egNarrowWrongSort struct{ M map[string]int64 }

// egNarrow decodes a spec-vector stream into the narrow target bound to
// the vector's wire name.
func egNarrow(t *testing.T, hexStr, name string, ex any) error {
	t.Helper()
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	d := gbon.NewDecoder(bytes.NewReader(b))
	if err := d.RegisterAs(name, ex); err != nil {
		t.Fatal(err)
	}
	out := ex
	return d.Decode(&out)
}

func TestErrorGoldCorpusVectors(t *testing.T) {
	// Offsets 74/63/93 are the spec vectors' narrow_offset pins.
	t.Run("c3_v115_mapshare", func(t *testing.T) {
		egCheck(t, egNarrow(t, egV115, "vec.mapshare", egNarrowMapShare{}), "bad_ref", gbon.ErrFormat, 74, "", "map record 10 is not materialized")
	})
	t.Run("c3_v116_viewshare", func(t *testing.T) {
		egCheck(t, egNarrow(t, egV116, "vec.viewshare", egNarrowViewShare{}), "bad_ref", gbon.ErrFormat, 63, "$.W", "shared backing 8 unavailable")
	})
	t.Run("c3_v117_wrongsort", func(t *testing.T) {
		egCheck(t, egNarrow(t, egV117, "vec.wrongsort", egNarrowWrongSort{}), "bad_ref", gbon.ErrFormat, 93, "", "ref 13 is not a map record")
	})
}

func TestErrorGoldCorpusWindows(t *testing.T) {
	t.Run("c4_window_fill2_internal_panic", func(t *testing.T) {
		full, hdr := windowStream(t)
		r := &npWindowReader{data: full, serve: map[int]int{1: hdr}, boom: 2}
		dec := gbon.NewDecoder(r)
		var n int64
		err := dec.Decode(&n)
		egCheck(t, err, "internal_panic", gbon.ErrInternal, -1, "", "unexpected panic during decode")
		var ae *gbon.Error
		if !errors.As(err, &ae) {
			t.Fatal(err)
		}
		if len(ae.Stack()) == 0 {
			t.Fatalf("internal_panic without a captured stack")
		}
	})
	t.Run("c4_window_fill1_alloc_budget", func(t *testing.T) {
		full, _ := windowStream(t)
		r := &npWindowReader{data: full, boom: 1, alloc: true}
		dec := gbon.NewDecoder(r)
		var n int64
		egCheck(t, dec.Decode(&n), "budget_alloc", gbon.ErrBudget, -1, "", "allocation during decode exceeded limits")
	})
	t.Run("c4_encode_sticky_reuse", func(t *testing.T) {
		var buf bytes.Buffer
		enc := gbon.NewEncoder(&buf)
		if err := enc.RegisterCoder(egBoomBox{}, egBoomCoder{}); err != nil {
			t.Fatal(err)
		}
		panicked := func() (p any) {
			defer func() { p = recover() }()
			_ = enc.Encode(egBoomBox{N: 1})
			return nil
		}()
		if panicked != "EGBOOM" {
			t.Fatalf("first Encode: panic = %v, want verbatim EGBOOM", panicked)
		}
		after := buf.Len()
		err := enc.Encode(egBoomBox{N: 2})
		egCheck(t, err, "internal_panic", gbon.ErrInternal, -1, "", "broken by a mid-Encode panic")
		if buf.Len() != after {
			t.Fatalf("rejected reuse appended %d bytes", buf.Len()-after)
		}
		var again = enc.Encode(egBoomBox{N: 3})
		if !errors.Is(again, gbon.ErrInternal) || again != err {
			t.Fatalf("sticky identity broken: %v vs %v", again, err)
		}
	})
}
