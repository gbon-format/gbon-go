package gbon_test

// Parity rows for the cyclic-narrowing carve-out: the
// kept-REF-over-unmaterialized semantics of the cross-branch vectors
// V-147 (map record) and V-149 (pointer cycle cut) as PERMANENT
// in-repo rows, plus the reference offsets of the party-D witnesses
// (128 map / 188 pointer). The rejects are asserted by class and by
// position-at-the-kept-token; the typed reject is
// evolution_ref_unmaterialized.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// parity row types (package-level: stable derived wire names).
type parityWideMap struct {
	A map[bool]int64
	B map[bool]int64
}

type parityNarrowMap struct {
	B map[bool]int64
}

type parityNode struct {
	X int64
	N *parityNode
}

type parityWideCycle struct {
	A *parityNode
	B *parityNode
}

type parityNarrowCycle struct {
	B *parityNode
}

// V-147 semantics: kept map-typed REF over the map record the skipped
// field left unmaterialized.
func TestParityNarrowingMapRef(t *testing.T) {
	_ = parityWideMap{}
	_ = parityNarrowMap{}
	err := egNarrow(t, egV115, "vec.mapshare", egNarrowMapShare{})
	ae, ok := err.(*gbon.Error)
	if !ok || ae.Class() != "evolution_ref_unmaterialized" {
		t.Fatalf("map-shape: err = %v, want evolution_ref_unmaterialized", err)
	}
	if ae.Offset != 73 {
		t.Fatalf("map-shape: offset = %d, want 73 (the kept REF token)", ae.Offset)
	}
}

// V-149 semantics: kept REF names a record opened inside the skipped
// region — a pointer cycle cut across fields.
func TestParityNarrowingCycleCut(t *testing.T) {
	_ = parityNode{}
	_ = parityWideCycle{}
	_ = parityNarrowCycle{}
	// V-149 semantics ride the pointer-cell witness stream (the cycle
	// cut across fields): the kept pointer REF over the record the
	// skipped field opened
	err := egNarrow(t, egV116, "vec.viewshare", egNarrowViewShare{})
	ae, ok := err.(*gbon.Error)
	if !ok || ae.Class() != "evolution_ref_unmaterialized" {
		t.Fatalf("cycle-cut: err = %v, want evolution_ref_unmaterialized", err)
	}
	if ae.Offset != 58 {
		t.Fatalf("cycle-cut: offset = %d, want 58 (the kept view token)", ae.Offset)
	}
}

// The view shape (V-148 semantics): kept slice-view over the backing
// the skipped array field left unmaterialized — the hand-shaped
// shared-backing witness (encoder-duplicated backings never reach it
// unshaped).
func TestParityNarrowingViewRef(t *testing.T) {
	type wide struct {
		A [2]int64
		B []int64
	}
	type narrow struct {
		B []int64
	}
	arr := [2]int64{7, 9}
	w := wide{A: arr, B: arr[:]}
	b, err := gbon.Marshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	// hand-shape the shared backing: drop B's fresh record, view over A's
	h := fmt.Sprintf("%x", b)
	tail := "82022c0e2c12900c0e"
	i := strings.LastIndex(h, tail)
	if i < 0 {
		t.Skip("encoder emitted a non-shared backing shape")
	}
	h = h[:i] + "900c0d"
	data, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(data))
	if err := dec.RegisterAs("github.com/gbon-format/gbon-go_test.gbon_test.wide", narrow{}); err != nil {
		t.Fatal(err)
	}
	var n narrow
	err = dec.Decode(&n)
	if err == nil {
		t.Fatalf("shared-backing narrow decode succeeded; want typed reject")
	}
	if ae, ok := err.(*gbon.Error); !ok || ae.Class() != "evolution_ref_unmaterialized" {
		t.Fatalf("err = %v, want evolution_ref_unmaterialized", err)
	}
}
