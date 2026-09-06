package gbon

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// White-box tests for registry branches unreachable through the public API
// from a single package: a name collision requires two distinct types with
// the same nameOf, and an Implements failure requires a structurally
// identical registered type with a different method set.

type imGood struct{ N int64 }

func (imGood) Face() int { return 1 }

func TestRegisterNameConflict(t *testing.T) {
	d := &Decoder{reg: map[string]reflect.Type{nameOf(reflect.TypeFor[int64]()): reflect.TypeFor[int32]()}}
	err := d.Register(int64(0))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("name conflict: want ErrUnsupported, got %v", err)
	}
	if err := d.Register(make(chan int)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("non-serializable example: want ErrUnsupported, got %v", err)
	}
}

type imFace2 interface {
	Face() int
	Other() string
}

// The Implements gate is unreachable through whole streams in-process: the
// name-keyed registry only resolves names it got from real types, and a
// static interface field always held an implementing value at encode time.
// It guards cross-binary skew, so the test calls resolveConcrete directly.
func TestRegistryImplementsGate(t *testing.T) {
	cd, err := descOf(reflect.TypeFor[imGood](), "")
	if err != nil {
		t.Fatal(err)
	}
	d := &codecDecoder{reg: map[string]reflect.Type{
		nameOf(reflect.TypeFor[imGood]()): reflect.TypeFor[imGood](),
	}}
	target := reflect.New(reflect.TypeFor[imFace2]()).Elem()
	err = d.resolveConcrete(cd, target, pathNode{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Implements gate: want ErrUnsupported, got %v", err)
	}
}

// The stream decoder shares the registry map with
// Decoder.reg — registrations between Decode calls are visible
// immediately.
func TestDecoderRegistryShared(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(int64(1)); err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(&buf)
	if dec.reg == nil {
		t.Fatal("registry not allocated eagerly")
	}
	var v int64
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.dec == nil {
		t.Fatal("stream decoder not initialized")
	}
	// Live-map check: mutate through one reference, observe through the other.
	dec.reg["witness"] = nil
	if _, ok := dec.dec.reg["witness"]; !ok {
		t.Fatal("registry map is not shared (mutation invisible)")
	}
}
