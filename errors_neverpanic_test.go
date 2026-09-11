package gbon_test

// Never-panic oracle: the decode recover boundary is a permanent
// tripwire; each row of the panic-kind table probes the public API and
// the expected class is the contract.

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

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
