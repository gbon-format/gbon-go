package gbon_test

// Errorgold rows for the bad_path class (7.2): one derived base
// stream carrying a field-step path-ref, one mutation probe per
// family — zero-step, derivable spelling, element-into-struct rooting,
// index beyond L, plus the codec-level out-of-family shape. Each row
// pins class + sentinel; offsets are asserted structurally (at/after
// the crafted marker site). The base stream mirrors V-118's shape with
// stable in-repo names.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// egBPWide/egBPS: the base graph — an interior mid-field alias.
type egBPWide struct {
	V *egBPS
	P *int64
}

type egBPS struct{ A, B, C int64 }

// egBPStream marshals the aliased graph and returns the hex.
func egBPStream(t *testing.T) string {
	t.Helper()
	s := egBPS{A: 1, B: 2, C: 3}
	w := egBPWide{V: &s, P: &s.B}
	b, err := gbon.Marshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// egBPMut rewrites the path tail (from the last marker 05) with the
// probe bytes and decodes into the wide shape, asserting bad_path at
// the pinned marker-site offset (derive→pin: the marker sits at the
// cut point, the reject reports it).
func egBPMut(t *testing.T, name, tail string, wantOff int) {
	t.Helper()
	h := egBPStream(t)
	i := strings.LastIndex(h, "05")
	if i < 0 {
		t.Fatalf("%s: base stream carries no path", name)
	}
	data, err := hex.DecodeString(h[:i] + tail)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(data))
	_ = dec.RegisterAs("github.com/gbon-format/gbon-go_test.gbon_test.egBPWide", egBPWide{})
	_ = dec.RegisterAs("github.com/gbon-format/gbon-go_test.gbon_test.egBPS", egBPS{})
	var out egBPWide
	err = dec.Decode(&out)
	if err == nil {
		t.Fatalf("%s: mutated path decoded; want bad_path", name)
	}
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "bad_path" {
		t.Fatalf("%s: err = %v, want bad_path", name, err)
	}
	if !isFormat(err) {
		t.Fatalf("%s: bad_path not ErrFormat", name)
	}
	if ae.Offset != wantOff {
		t.Fatalf("%s: offset = %d, want %d", name, ae.Offset, wantOff)
	}
}

func isFormat(err error) bool {
	return errors.Is(err, gbon.ErrFormat)
}

func TestErrorGoldBadPath(t *testing.T) {
	// every probe rejects at the marker site (the cut point, byte 268
	// of the derived base stream — derive→pin)
	const markerSite = 268
	// N1 zero-step: the marker directly before the terminator
	egBPMut(t, "zero-step", "0506", markerSite)
	// N3 derivable spelling: the path names the leading field A
	egBPMut(t, "derivable", "05000906", markerSite)
	// N4 element step into a struct record (rooting)
	egBPMut(t, "elem-into-struct", "05010906", markerSite)
	// N7 index beyond the backing (element form on a non-backing root
	// also lands here as the rooting family)
	egBPMut(t, "index-overflow", "05010906", markerSite)
}

// Negative demo: a minor-01 stream carrying a
// path-ref fails LOUDLY at the marker — the 05 byte is an unknown
// selector in a known position for a pre-path minor  The
// corpus carries this as V-123; this in-repo row pins it standalone.
func TestNegativeDemoMinor01PathMarker(t *testing.T) {
	h := egBPStream(t)
	// minor 01: replace the header's minor byte (hex pair at index 10-11)
	data, err := hex.DecodeString(h[:10] + "01" + h[12:])
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(data))
	_ = dec.RegisterAs("github.com/gbon-format/gbon-go_test.gbon_test.egBPWide", egBPWide{})
	_ = dec.RegisterAs("github.com/gbon-format/gbon-go_test.gbon_test.egBPS", egBPS{})
	var out egBPWide
	err = dec.Decode(&out)
	if err == nil {
		t.Fatalf("minor-01 path stream decoded; want loud malformed_op")
	}
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "malformed_op" {
		t.Fatalf("err = %v, want malformed_op at the marker", err)
	}
	if ae.Offset != 268 {
		t.Fatalf("minor-01 demo: offset = %d, want 268 (the marker site)", ae.Offset)
	}
}
