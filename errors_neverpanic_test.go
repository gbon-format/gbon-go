package gbon_test

// Never-panic oracle: the decode recover boundary is a permanent
// tripwire; each row of the panic-kind table probes the public API and
// the expected class is the contract.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/gbon-format/gbon-go"
)

// npBox is the wire carrier of the panicking coder probes.
type npBox struct{ N int64 }

// npPanicKind selects what the coder panics with.
type npPanicKind int

const (
	npIndexPanic  npPanicKind = iota // index out of range (runtime error)
	npNilDeref                       // nil pointer dereference (runtime error)
	npAllocString                    // allocation look-alike phrase (string panic)
	npCoderString                    // plain coder string panic
)

type npCoder struct{ kind npPanicKind }

func (npCoder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	return e.Encode(v.Interface().(npBox).N)
}

func (c npCoder) DecodeValue(*gbon.Decoder, reflect.Value) error {
	switch c.kind {
	case npIndexPanic:
		s := []int{1}
		_ = s[5]
	case npNilDeref:
		var p *int
		_ = *p
	case npAllocString:
		panic("makeslice: len out of range")
	default:
		panic("NPBOOM")
	}
	return nil
}

// allocBombProbe decodes the crafted 2^60 []int8 bomb under raised
// limits: the charge passes, the allocator refuses.
func allocBombProbe() error {
	dec := gbon.NewDecoder(bytes.NewReader(craftedAllocBomb()))
	dec.SetLimits(gbon.Limits{MaxBytes: math.MaxInt64, MaxSliceLen: math.MaxInt64})
	var v []int8
	return dec.Decode(&v)
}

// panicCoderProbe decodes a registered coder that panics with kind.
func panicCoderProbe(kind npPanicKind) error {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(npBox{}, npCoder{kind: kind}); err != nil {
		return err
	}
	if err := enc.Encode(npBox{N: 7}); err != nil {
		return err
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err := dec.RegisterCoder(npBox{}, npCoder{kind: kind}); err != nil {
		return err
	}
	var v npBox
	return dec.Decode(&v)
}

func TestNeverPanicTripwire(t *testing.T) {
	rows := []struct {
		name    string
		class   string
		err     error
		render  string // expected %v cause phrase prefix
		wantGot bool   // panic value expected in Got
	}{
		{"crafted-2^60-makeslice", "budget_alloc", allocBombProbe(),
			"allocation during decode exceeded limits: ", false},
		{"index-out-of-range", "internal_panic", panicCoderProbe(npIndexPanic),
			"unexpected panic during decode", true},
		{"nil-deref", "internal_panic", panicCoderProbe(npNilDeref),
			"unexpected panic during decode", true},
		{"alloc-lookalike-string", "internal_panic", panicCoderProbe(npAllocString),
			"unexpected panic during decode", true},
		{"coder-string-panic", "internal_panic", panicCoderProbe(npCoderString),
			"unexpected panic during decode", true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.err == nil {
				t.Fatalf("probe produced no error")
			}
			var ae *gbon.Error
			if !errors.As(row.err, &ae) {
				t.Fatalf("not As-recoverable: %v", row.err)
			}
			if ae.Class() != row.class {
				t.Fatalf("class = %q, want %q (err %v)", ae.Class(), row.class, row.err)
			}
			want := gbon.ErrInternal
			if row.class == "budget_alloc" {
				want = gbon.ErrBudget
			}
			if !errors.Is(row.err, want) {
				t.Fatalf("Is(%v) false (err %v)", want, row.err)
			}
			for _, other := range []error{gbon.ErrFormat, gbon.ErrBudget, gbon.ErrUnsupported, gbon.ErrIO, gbon.ErrInternal} {
				if got := errors.Is(row.err, other); got != (other == want) {
					t.Fatalf("Is(%v) = %v, want %v", other, got, other == want)
				}
			}
			if msg := row.err.Error(); !strings.Contains(msg, row.render) {
				t.Fatalf("%%v %q lacks cause phrase %q", msg, row.render)
			}
			verbose := fmt.Sprintf("%+v", ae)
			if row.wantGot {
				if ae.Got == nil {
					t.Fatalf("panic value missing from Got: %+v", ae)
				}
				if !strings.Contains(verbose, "got = ") {
					t.Fatalf("%%+v lacks the got field: %q", verbose)
				}
				if n := len(verbose); n > 4096 {
					t.Fatalf("%%+v rendering unbounded: %d bytes", n)
				}
			}
			if row.class == "internal_panic" {
				if len(ae.Stack()) == 0 {
					t.Fatalf("internal_panic carries no stack")
				}
			} else if ae.Stack() != nil {
				t.Fatalf("honest error %q carries a stack", row.class)
			}
		})
	}
}

// TestNeverPanicHonestStackBoundary pins the honest side of the stack
// contract: the crafted budget classes carry no stack.
func TestNeverPanicHonestStackBoundary(t *testing.T) {
	err := allocBombProbe()
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "budget_alloc" {
		t.Fatalf("alloc bomb: %v", err)
	}
	if ae.Stack() != nil {
		t.Fatalf("budget_alloc carries a stack")
	}
	if ae.Got != nil || ae.Want != nil {
		t.Fatalf("budget_alloc Got/Want must stay nil: %+v", ae)
	}
}

// TestNeverPanicStackNamesCoder verifies the tripwire attribution for
// coder panics: the coder frames are visible in the captured stack.
func TestNeverPanicStackNamesCoder(t *testing.T) {
	err := panicCoderProbe(npCoderString)
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "internal_panic" {
		t.Fatalf("coder panic: %v", err)
	}
	if !strings.Contains(string(ae.Stack()), "npCoder.DecodeValue") {
		t.Fatalf("stack lacks the coder frame")
	}
}

// npWindowReader serves a stream in controlled chunks and panics on a
// chosen read call: 1 = header fill, 2 = end-of-stream probe, 3+ = body;
// alloc raises a make/grow allocation panic instead of a plain value.
type npWindowReader struct {
	data  []byte
	off   int
	calls int
	serve map[int]int // per-call byte cap
	boom  int         // 1-based call index that panics; 0 = never
	alloc bool
}

func (r *npWindowReader) Read(p []byte) (int, error) {
	r.calls++
	if r.boom == r.calls {
		if r.alloc {
			_ = make([]byte, 1<<62)
		}
		panic("WBOOM")
	}
	rem := r.data[r.off:]
	if c, ok := r.serve[r.calls]; ok && len(rem) > c {
		rem = rem[:c]
	}
	n := copy(p, rem)
	r.off += n
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// windowStream returns a one-int64 stream and its header length.
func windowStream(t *testing.T) ([]byte, int) {
	t.Helper()
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(int64(42)); err != nil {
		t.Fatalf("windowStream encode: %v", err)
	}
	full := buf.Bytes()
	return full, headerLen(t, full)
}

// TestNeverPanicDecodeWindow pins the closed decode window: a panicking
// reader classifies at every fill (header, probe, body) and no fill
// re-reads after the classified error.
func TestNeverPanicDecodeWindow(t *testing.T) {
	full, hdr := windowStream(t)
	rows := []struct {
		name  string
		serve map[int]int
		boom  int
		alloc bool
		class string
		want  error
		got   any
	}{
		{"fill1-header", nil, 1, false, "internal_panic", gbon.ErrInternal, "WBOOM"},
		{"fill2-probe", map[int]int{1: hdr}, 2, false, "internal_panic", gbon.ErrInternal, "WBOOM"},
		{"fill3-body", map[int]int{1: hdr, 2: len(full) - hdr - 1}, 3, false, "internal_panic", gbon.ErrInternal, "WBOOM"},
		{"fill1-alloc", nil, 1, true, "budget_alloc", gbon.ErrBudget, nil},
		{"fill2-alloc", map[int]int{1: hdr}, 2, true, "budget_alloc", gbon.ErrBudget, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			r := &npWindowReader{data: full, serve: row.serve, boom: row.boom, alloc: row.alloc}
			dec := gbon.NewDecoder(r)
			var v int64
			err := dec.Decode(&v)
			var ae *gbon.Error
			if !errors.As(err, &ae) {
				t.Fatalf("want *gbon.Error, got %T: %v", err, err)
			}
			if ae.Class() != row.class {
				t.Fatalf("class = %q, want %q (err %v)", ae.Class(), row.class, err)
			}
			if !errors.Is(err, row.want) {
				t.Fatalf("Is(%v) false (err %v)", row.want, err)
			}
			if r.calls != row.boom {
				t.Fatalf("reader calls = %d, want %d (fill retried or window mismatch)", r.calls, row.boom)
			}
			if row.class == "internal_panic" {
				if ae.Got != "WBOOM" {
					t.Fatalf("panic value missing from Got: %+v", ae)
				}
				if len(ae.Stack()) == 0 {
					t.Fatalf("internal_panic carries no stack")
				}
				if msg := err.Error(); !strings.Contains(msg, "unexpected panic during decode") {
					t.Fatalf("%v lacks the decode panic phrase", msg)
				}
			} else {
				if ae.Got != nil || ae.Stack() != nil {
					t.Fatalf("budget_alloc carries panic context: %+v", ae)
				}
				if msg := err.Error(); !strings.Contains(msg, "allocation during decode exceeded limits") {
					t.Fatalf("%v lacks the alloc phrase", msg)
				}
			}
			// The classified fill is sticky through the facade: a repeat
			// Decode returns the same error without touching the reader.
			again := dec.Decode(&v)
			if again.Error() != err.Error() {
				t.Fatalf("repeat Decode: want the same sticky error, got %v", again)
			}
			if r.calls != row.boom {
				t.Fatalf("reader touched after the sticky error: calls = %d, want %d", r.calls, row.boom)
			}
		})
	}
}

// TestNeverPanicDecodeWindowCtlClean: typed outcomes survive the
// lazy-init reshuffle — a header-only stream stays typed truncated
// (sticky), a fully decoded stream ends in a clean io.EOF.
func TestNeverPanicDecodeWindowCtlClean(t *testing.T) {
	full, hdr := windowStream(t)

	cut := full[:hdr]
	dec := gbon.NewDecoder(bytes.NewReader(cut))
	var v int64
	err := dec.Decode(&v)
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "truncated" || !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("header-only: want sticky-surfacing truncated, got %v", err)
	}
	if again := dec.Decode(&v); again.Error() != err.Error() {
		t.Fatalf("header-only repeat: want the same sticky error, got %v", again)
	}

	dec2 := gbon.NewDecoder(bytes.NewReader(full))
	if err := dec2.Decode(&v); err != nil || v != 42 {
		t.Fatalf("full stream: Decode err %v, v %d", err, v)
	}
	if err := dec2.Decode(&v); !errors.Is(err, io.EOF) {
		t.Fatalf("stream end: want io.EOF, got %v", err)
	}
}

// npEncBoomCoder panics in EncodeValue with val; npErrCoder returns a
// plain typed error (the non-panic control).
type npEncBoomCoder struct{ val any }

func (c npEncBoomCoder) EncodeValue(*gbon.Encoder, reflect.Value) error {
	panic(c.val)
}

func (npEncBoomCoder) DecodeValue(*gbon.Decoder, reflect.Value) error { return nil }

type npErrCoder struct{}

func (npErrCoder) EncodeValue(*gbon.Encoder, reflect.Value) error {
	return errors.New("NPCTL")
}

func (npErrCoder) DecodeValue(*gbon.Decoder, reflect.Value) error { return nil }

// npBinBoom / npTxtBoom panic inside the automatic Binary/Text adapters;
// npBoomWriter panics inside Write after partial progress.
type npBinBoom struct{ N int64 }

func (npBinBoom) MarshalBinary() ([]byte, error) { panic("MBBOOM") }

func (*npBinBoom) UnmarshalBinary([]byte) error { return nil }

type npTxtBoom struct{ S string }

func (npTxtBoom) MarshalText() ([]byte, error) { panic("MTBOOM") }

func (*npTxtBoom) UnmarshalText([]byte) error { return nil }

type npBoomWriter struct{ inner bytes.Buffer }

func (w *npBoomWriter) Write(p []byte) (int, error) {
	_, _ = w.inner.Write(p[:len(p)/2])
	panic("WBOOM")
}

// npZeroWriter clears the unexported writer field of an Encoder: the API
// offers no writer mutation, and the broken+nil-writer state exists only
// through it (the gate precedes every panic path).
func npZeroWriter(enc *gbon.Encoder) {
	rv := reflect.ValueOf(enc).Elem().FieldByName("w")
	f := reflect.NewAt(rv.Type(), unsafe.Pointer(rv.Addr().UnsafePointer())).Elem()
	f.Set(reflect.Zero(rv.Type()))
}

// TestNeverPanicPanicThenReuse pins the encode panic contract: a panic
// escaping Encode re-panics verbatim and breaks the Encoder; reuse
// returns the sticky internal_panic error without stream bytes.
func TestNeverPanicPanicThenReuse(t *testing.T) {
	rows := []struct {
		name  string
		setup func(t *testing.T) (*gbon.Encoder, *bytes.Buffer, any)
		boom  any
	}{
		{"coder-encodevalue", func(t *testing.T) (*gbon.Encoder, *bytes.Buffer, any) {
			buf := &bytes.Buffer{}
			enc := gbon.NewEncoder(buf)
			if err := enc.RegisterCoder(npBox{}, npEncBoomCoder{val: "ECBOOM"}); err != nil {
				t.Fatal(err)
			}
			return enc, buf, npBox{N: 7}
		}, "ECBOOM"},
		{"marshalbinary", func(t *testing.T) (*gbon.Encoder, *bytes.Buffer, any) {
			buf := &bytes.Buffer{}
			return gbon.NewEncoder(buf), buf, npBinBoom{N: 1}
		}, "MBBOOM"},
		{"marshaltext", func(t *testing.T) (*gbon.Encoder, *bytes.Buffer, any) {
			buf := &bytes.Buffer{}
			return gbon.NewEncoder(buf), buf, npTxtBoom{S: "x"}
		}, "MTBOOM"},
		{"writer-flushto", func(t *testing.T) (*gbon.Encoder, *bytes.Buffer, any) {
			w := &npBoomWriter{}
			return gbon.NewEncoder(w), &w.inner, int64(5)
		}, "WBOOM"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			enc, buf, v := row.setup(t)
			got := func() (r any) {
				defer func() { r = recover() }()
				_ = enc.Encode(v)
				return nil
			}()
			if got != row.boom {
				t.Fatalf("first Encode: recover %v, want verbatim panic %v", got, row.boom)
			}
			before := buf.Len()
			err2 := enc.Encode(v)
			var ae *gbon.Error
			if !errors.As(err2, &ae) {
				t.Fatalf("reuse: want *gbon.Error, got %T: %v", err2, err2)
			}
			if ae.Class() != "internal_panic" || !errors.Is(err2, gbon.ErrInternal) {
				t.Fatalf("reuse: class %q, Is(ErrInternal) false (err %v)", ae.Class(), err2)
			}
			if msg := err2.Error(); !strings.Contains(msg, "broken by a mid-Encode panic") {
				t.Fatalf("reuse message lacks the stem: %q", msg)
			}
			if len(ae.Stack()) == 0 {
				t.Fatalf("sticky error carries no stack")
			}
			if buf.Len() != before {
				t.Fatalf("rejected reuse appended %d bytes to the stream", buf.Len()-before)
			}
			if err3 := enc.Encode(v); err3 != err2 {
				t.Fatalf("third Encode: want the same sticky error, got %v", err3)
			}
			if buf.Len() != before {
				t.Fatalf("third Encode appended %d bytes to the stream", buf.Len()-before)
			}
		})
	}

	// The sticky check rides the entry error check: with a broken Encoder
	// even a nil writer yields the sticky internal_panic error, not a
	// fresh contract_mismatch.
	t.Run("sticky-over-nil-writer", func(t *testing.T) {
		buf := &bytes.Buffer{}
		enc := gbon.NewEncoder(buf)
		if err := enc.RegisterCoder(npBox{}, npEncBoomCoder{val: "ECBOOM"}); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() { _ = recover() }()
			_ = enc.Encode(npBox{N: 1})
		}()
		npZeroWriter(enc)
		err := enc.Encode(npBox{N: 2})
		var ae *gbon.Error
		if !errors.As(err, &ae) || ae.Class() != "internal_panic" || !errors.Is(err, gbon.ErrInternal) {
			t.Fatalf("broken+nil-writer: want sticky internal_panic, got %v", err)
		}
		if errors.Is(err, gbon.ErrUnsupported) {
			t.Fatalf("broken+nil-writer: contract_mismatch won over the sticky error")
		}
	})

	// Control: a plain typed coder error stays a sticky coder_error —
	// only the panic path gains the internal_panic stickiness.
	t.Run("ctl-typed-coder-error", func(t *testing.T) {
		buf := &bytes.Buffer{}
		enc := gbon.NewEncoder(buf)
		if err := enc.RegisterCoder(npBox{}, npErrCoder{}); err != nil {
			t.Fatal(err)
		}
		err1 := enc.Encode(npBox{N: 1})
		var ae *gbon.Error
		if !errors.As(err1, &ae) || ae.Class() != "coder_error" || !errors.Is(err1, gbon.ErrUnsupported) {
			t.Fatalf("ctl: want coder_error, got %v", err1)
		}
		if err2 := enc.Encode(npBox{N: 2}); err2 != err1 {
			t.Fatalf("ctl reuse: want the same sticky error, got %v", err2)
		}
		if buf.Len() != 0 {
			t.Fatalf("ctl: bytes reached the stream: %d", buf.Len())
		}
	})
}

// npNilScope stubs: non-pointer nilable kinds and wrappers whose methods
// panic in user code — the source gate is pointer-scope by contract and
// stays silent for all of them.
type npNilMapWriter map[int]int

func (m npNilMapWriter) Write([]byte) (int, error) {
	m[0] = 1
	return 0, nil
}

type npNilMapReader map[int]int

func (m npNilMapReader) Read([]byte) (int, error) {
	m[0] = 1
	return 0, nil
}

type npNilChanReader chan int

func (c npNilChanReader) Read([]byte) (int, error) {
	close(c)
	return 0, nil
}

type npFuncReader func([]byte) (int, error)

func (f npFuncReader) Read(p []byte) (int, error) { return f(p) }

type npWrapWriter struct{ inner *bytes.Buffer }

func (w npWrapWriter) Write(p []byte) (int, error) { return w.inner.Write(p) }

type npWrapReader struct{ inner *bytes.Buffer }

func (w npWrapReader) Read(p []byte) (int, error) { return w.inner.Read(p) }

func npAssertMismatch(t *testing.T, err error) {
	t.Helper()
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "contract_mismatch" || !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("want contract_mismatch, got %v", err)
	}
	if errors.Is(err, gbon.ErrInternal) {
		t.Fatalf("contract_mismatch must not carry the internal family")
	}
}

func npAssertContained(t *testing.T, err error) {
	t.Helper()
	var ae *gbon.Error
	if !errors.As(err, &ae) || ae.Class() != "internal_panic" || !errors.Is(err, gbon.ErrInternal) {
		t.Fatalf("want contained internal_panic, got %v", err)
	}
}

// TestNeverPanicNilSourceScope pins the gate's pointer-only scope both
// ways: nil interface / typed-nil pointer sources are rejected
// non-sticky; every other nilable shape passes and owns its failure.
func TestNeverPanicNilSourceScope(t *testing.T) {
	t.Run("nil-interface-writer", func(t *testing.T) {
		enc := gbon.NewEncoder(nil)
		npAssertMismatch(t, enc.Encode(int64(1)))
		npAssertMismatch(t, enc.Encode(int64(2)))
		if err := enc.RegisterCoder(npBox{}, npErrCoder{}); err != nil {
			t.Fatalf("rejection stuck on a usable Encoder: %v", err)
		}
	})
	t.Run("nil-interface-reader", func(t *testing.T) {
		dec := gbon.NewDecoder(nil)
		npAssertMismatch(t, dec.Decode(new(int64)))
		npAssertMismatch(t, dec.Decode(new(int64)))
	})
	t.Run("typed-nil-ptr-writer", func(t *testing.T) {
		enc := gbon.NewEncoder((*bytes.Buffer)(nil))
		npAssertMismatch(t, enc.Encode(int64(1)))
	})
	t.Run("typed-nil-ptr-reader", func(t *testing.T) {
		dec := gbon.NewDecoder((*bytes.Buffer)(nil))
		npAssertMismatch(t, dec.Decode(new(int64)))
	})
	t.Run("typed-nil-map-writer", func(t *testing.T) {
		enc := gbon.NewEncoder(npNilMapWriter(nil))
		got := func() (r any) {
			defer func() { r = recover() }()
			_ = enc.Encode(int64(1))
			return nil
		}()
		if got == nil {
			t.Fatal("nil-map writer: the gate must not intercept, the panic must propagate")
		}
	})
	t.Run("typed-nil-map-reader", func(t *testing.T) {
		dec := gbon.NewDecoder(npNilMapReader(nil))
		npAssertContained(t, dec.Decode(new(int64)))
	})
	t.Run("typed-nil-chan-reader", func(t *testing.T) {
		dec := gbon.NewDecoder(npNilChanReader(nil))
		npAssertContained(t, dec.Decode(new(int64)))
	})
	t.Run("typed-nil-func-reader", func(t *testing.T) {
		dec := gbon.NewDecoder(npFuncReader(nil))
		npAssertContained(t, dec.Decode(new(int64)))
	})
	t.Run("nil-inner-writer", func(t *testing.T) {
		enc := gbon.NewEncoder(npWrapWriter{})
		got := func() (r any) {
			defer func() { r = recover() }()
			_ = enc.Encode(int64(1))
			return nil
		}()
		if got == nil {
			t.Fatal("nil-inner writer: the gate must not intercept, the panic must propagate")
		}
	})
	t.Run("nil-inner-reader", func(t *testing.T) {
		dec := gbon.NewDecoder(npWrapReader{})
		npAssertContained(t, dec.Decode(new(int64)))
	})
}
