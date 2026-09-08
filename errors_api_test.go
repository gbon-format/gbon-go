package gbon_test

// Coder-seam and io-seam API behavior (spec SC-C2b, SC-C8a; property
// P5): a user coder's raw marker error exits encode/decode wrapped by
// the core — class coder_error, seam-assigned path (and offset on the
// decode side), original fault through Unwrap; a failing underlying
// reader surfaces as io_read/ErrIO with the fault as the cause, while
// EOF mid-body is truncated and a drained stream's io.EOF is "no more
// values", not an error.

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gbon-format/gbon-go"
)

type apiMarked struct{ V int64 }

type apiOKCoder struct{}

func (apiOKCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(apiMarked).V)
}

func (apiOKCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var n int64
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.Set(reflect.ValueOf(apiMarked{V: n}))
	return nil
}

var errAPIEncodeMarker = errors.New("APIENCODEMARKER")
var errAPIDecodeMarker = errors.New("APIDECODEMARKER")

type apiFailEncodeCoder struct{}

func (apiFailEncodeCoder) EncodeValue(*gbon.Encoder, reflect.Value) error {
	return errAPIEncodeMarker
}

func (apiFailEncodeCoder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	return apiOKCoder{}.DecodeValue(d, v)
}

type apiFailDecodeCoder struct{}

func (apiFailDecodeCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return apiOKCoder{}.EncodeValue(e, v)
}

func (apiFailDecodeCoder) DecodeValue(*gbon.Decoder, reflect.Value) error {
	return errAPIDecodeMarker
}

// TestAPICoderWrapEncode (SC-C2b): a raw marker error from a user coder
// exits the encode seam wrapped — class coder_error, seam-assigned path,
// marker reachable through Unwrap.
func TestAPICoderWrapEncode(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(apiMarked{}, apiFailEncodeCoder{}); err != nil {
		t.Fatal(err)
	}
	err := enc.Encode(apiMarked{V: 1})
	var ae *gbon.Error
	if !errors.As(err, &ae) {
		t.Fatalf("encode seam must wrap the raw coder error: %v", err)
	}
	if ae.Class() != "coder_error" {
		t.Fatalf("class = %q, want coder_error", ae.Class())
	}
	if ae.Path == "" {
		t.Fatalf("seam must assign a path: %+v", ae)
	}
	if !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("coder_error must stay in the ErrUnsupported family: %v", err)
	}
	if ae.Unwrap() != errAPIEncodeMarker {
		t.Fatalf("Unwrap must reach the marker: %v", ae.Unwrap())
	}
}

// TestAPICoderWrapDecode (SC-C2b): the decode seam additionally assigns
// the byte offset of the sub-decode position.
func TestAPICoderWrapDecode(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(apiMarked{}, apiOKCoder{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(apiMarked{V: 7}); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err := dec.RegisterCoder(apiMarked{}, apiFailDecodeCoder{}); err != nil {
		t.Fatal(err)
	}
	var out apiMarked
	err := dec.Decode(&out)
	var ae *gbon.Error
	if !errors.As(err, &ae) {
		t.Fatalf("decode seam must wrap the raw coder error: %v", err)
	}
	if ae.Class() != "coder_error" {
		t.Fatalf("class = %q, want coder_error", ae.Class())
	}
	if ae.Path == "" || ae.Offset < 0 {
		t.Fatalf("seam must assign path and byte offset: %+v", ae)
	}
	if ae.Unwrap() != errAPIDecodeMarker {
		t.Fatalf("Unwrap must reach the marker: %v", ae.Unwrap())
	}
}

// TestAPIIOFaultInjection (SC-C8a, P5): a marker error from the
// underlying reader at any split point — including after the first bytes
// of the body — surfaces as io_read/ErrIO with the fault as the cause.
func TestAPIIOFaultInjection(t *testing.T) {
	good, err := gbon.Marshal([]int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	marker := errors.New("APIREADFAULT")
	for n := range good {
		dec := gbon.NewDecoder(&oracleFaultReader{data: good[:n], err: marker})
		var s []int64
		err := dec.Decode(&s)
		if !errors.Is(err, gbon.ErrIO) {
			t.Fatalf("split at %d: want ErrIO, got %v", n, err)
		}
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "io_read" {
			t.Fatalf("split at %d: want class io_read, got %v", n, err)
		}
		if ae.Unwrap() != marker {
			t.Fatalf("split at %d: Unwrap must reach the fault: %v", n, ae.Unwrap())
		}
		if errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("split at %d: io fault misattributed as format: %v", n, err)
		}
	}
}

// oracleFaultWriter accepts up to n bytes on each Write, then reports
// err (io injection seam for the write side).
type oracleFaultWriter struct {
	n   int
	err error
}

func (w *oracleFaultWriter) Write(p []byte) (int, error) {
	take := min(len(p), w.n)
	return take, w.err
}

// TestAPIIOWriterFault (SC-C8b): a marker error from the underlying writer at any
// split point of the flushed stream surfaces as io_write/ErrIO with the fault as
// the cause and no input context; the sticky channel carries the classified error.
func TestAPIIOWriterFault(t *testing.T) {
	good, err := gbon.Marshal([]int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	marker := errors.New("APIWRITEFAULT")
	for n := range good {
		enc := gbon.NewEncoder(&oracleFaultWriter{n: n, err: marker})
		err := enc.Encode([]int64{1, 2, 3})
		if !errors.Is(err, gbon.ErrIO) {
			t.Fatalf("split at %d: want ErrIO, got %v", n, err)
		}
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "io_write" {
			t.Fatalf("split at %d: want class io_write, got %v", n, err)
		}
		if ae.Unwrap() != marker {
			t.Fatalf("split at %d: Unwrap must reach the fault: %v", n, ae.Unwrap())
		}
		if ae.Offset != -1 || ae.Path != "" {
			t.Fatalf("split at %d: io_write carries no input context: %+v", n, ae)
		}
		err2 := enc.Encode(int64(9))
		if !errors.Is(err2, gbon.ErrIO) {
			t.Fatalf("split at %d: sticky repeat must stay ErrIO, got %v", n, err2)
		}
		var ae2 *gbon.Error
		if !errors.As(err2, &ae2) || ae2.Class() != "io_write" {
			t.Fatalf("split at %d: sticky repeat lost the class: %v", n, err2)
		}
	}
}

// TestAPINilSource (SC-C9): a nil interface or a typed-nil pointer handed to
// NewDecoder/NewEncoder makes Decode/Encode return a contract_mismatch error
// in the ErrUnsupported family — no panic; a repeat call re-runs the check.
func TestAPINilSource(t *testing.T) {
	assertNilSourceErr := func(t *testing.T, name string, err error) {
		t.Helper()
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "contract_mismatch" {
			t.Fatalf("%s: want contract_mismatch, got %v", name, err)
		}
		if !errors.Is(err, gbon.ErrUnsupported) {
			t.Fatalf("%s: want the ErrUnsupported family: %v", name, err)
		}
	}
	t.Run("reader", func(t *testing.T) {
		readers := map[string]io.Reader{
			"nil-interface": nil,
			"typed-nil":     (*bytes.Reader)(nil),
		}
		for name, r := range readers {
			dec := gbon.NewDecoder(r)
			var out apiMarked
			err := dec.Decode(&out)
			assertNilSourceErr(t, name, err)
			err2 := dec.Decode(&out)
			assertNilSourceErr(t, name+" repeat", err2)
		}
	})
	t.Run("writer", func(t *testing.T) {
		writers := map[string]io.Writer{
			"nil-interface": nil,
			"typed-nil":     (*bytes.Buffer)(nil),
		}
		for name, w := range writers {
			enc := gbon.NewEncoder(w)
			err := enc.Encode(apiMarked{V: 1})
			assertNilSourceErr(t, name, err)
			err2 := enc.Encode(apiMarked{V: 2})
			assertNilSourceErr(t, name+" repeat", err2)
		}
	})
	// a nil target beside a nil source: one of the two pre-flight
	// contract_mismatch checks fires — the class is the contract
	dec := gbon.NewDecoder(nil)
	err := dec.Decode(nil)
	assertNilSourceErr(t, "nil target with nil source", err)
}

// TestAPIIOEOFAttribution (SC-C8a): EOF mid-body is truncated (data
// attribution, ErrFormat) — including the exact body-boundary cut; a
// drained multi-value stream reports io.EOF, which is "no more values",
// not an error class.
func TestAPIIOEOFAttribution(t *testing.T) {
	good, err := gbon.Marshal([]int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{4, len(good) / 2, len(good) - 1} {
		dec := gbon.NewDecoder(&oracleFaultReader{data: good[:cut], err: io.EOF})
		var s []int64
		err := dec.Decode(&s)
		if !errors.Is(err, gbon.ErrFormat) {
			t.Fatalf("cut %d: want ErrFormat, got %v", cut, err)
		}
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "truncated" {
			t.Fatalf("cut %d: want class truncated, got %v", cut, err)
		}
	}

	// drained stream: decodeSub io.EOF is not an error
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	for i := range 2 {
		if err := enc.Encode(int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	for i := range 2 {
		var n int64
		if err := dec.Decode(&n); err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
	}
	var extra int64
	err = dec.Decode(&extra)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("drained stream must report io.EOF, got %v", err)
	}
	if ae, ok := errors.AsType[*gbon.Error](err); ok {
		t.Fatalf("drained-stream EOF must carry no error class: %+v", ae)
	}
}
