package gbon_test

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// The public API surface compiles with exactly the specified shapes.
func TestAPIPresence(t *testing.T) {
	var _ func(any) ([]byte, error) = gbon.Marshal
	var _ func([]byte, any) error = gbon.Unmarshal

	var _ func(io.Writer) *gbon.Encoder = gbon.NewEncoder
	var _ func(*gbon.Encoder, any) error = (*gbon.Encoder).Encode
	var _ func(*gbon.Encoder, any, gbon.Coder) error = (*gbon.Encoder).RegisterCoder

	var _ func(io.Reader) *gbon.Decoder = gbon.NewDecoder
	var _ func(*gbon.Decoder, any) error = (*gbon.Decoder).Decode
	var _ func(*gbon.Decoder, ...any) error = (*gbon.Decoder).Register
	var _ func(*gbon.Decoder, any, gbon.Coder) error = (*gbon.Decoder).RegisterCoder
	var _ func(*gbon.Decoder, gbon.Limits) = (*gbon.Decoder).SetLimits

	var _ gbon.Coder = (*stubCoder)(nil)
	var _ = gbon.Limits{
		MaxDepth: 1, MaxNodes: 1, MaxBytes: 1,
		MaxMapPairs: 1, MaxSliceLen: 1,
	}
}

// Sentinel errors exist and are distinct.
func TestSentinels(t *testing.T) {
	sentinels := []error{gbon.ErrUnsupported, gbon.ErrBudget, gbon.ErrFormat}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i != j && errors.Is(a, b) {
				t.Fatalf("sentinels %v and %v are not distinct", a, b)
			}
		}
	}
}

// The API contract on live methods: Marshal/Unmarshal round-trip a valid
// value without error; RegisterCoder on the Encoder facade registers (nil
// legal pairing) and rejects misuse (nil arguments, built-in time.Time
// override) with ErrUnsupported.
func TestAPIContract(t *testing.T) {
	data, err := gbon.Marshal([]int{1, 2, 3})
	if err != nil {
		t.Fatalf("Marshal valid value: %v", err)
	}
	var back []int
	if err := gbon.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal valid value: %v", err)
	}
	if !reflect.DeepEqual([]int{1, 2, 3}, back) {
		t.Fatalf("round trip: got %v, want [1 2 3]", back)
	}

	enc := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc.RegisterCoder(1, stubCoder{}); err != nil {
		t.Fatalf("Encoder.RegisterCoder legal pairing: %v", err)
	}
	if err := enc.RegisterCoder(1, nil); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("Encoder.RegisterCoder nil coder: want ErrUnsupported, got %v", err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(nil))
	if err := dec.RegisterCoder(time.Time{}, stubCoder{}); !errors.Is(err, gbon.ErrUnsupported) {
		t.Fatalf("Decoder.RegisterCoder built-in override: want ErrUnsupported, got %v", err)
	}
}

// SetLimits stores its argument; the stored limits apply to subsequent
// Decode calls.
func TestSetLimitsStores(t *testing.T) {
	dec := gbon.NewDecoder(bytes.NewReader(nil))
	dec.SetLimits(gbon.Limits{MaxDepth: 7})
}

type stubCoder struct{}

func (stubCoder) EncodeValue(*gbon.Encoder, reflect.Value) error { return nil }
func (stubCoder) DecodeValue(*gbon.Decoder, reflect.Value) error { return nil }
