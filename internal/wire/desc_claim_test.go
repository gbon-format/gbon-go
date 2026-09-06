package wire

import "testing"

// The name-claim discipline of WriteDesc: a second descriptor under an
// already-claimed name must be structurally identical (REF) or the write
// rejects loudly — never a REF onto a structurally different type.

func TestWriteDescNameClaim(t *testing.T) {
	mk := func(x string) *Desc {
		return &Desc{Kind: KindStruct, Name: "cfg.Config", Fields: []Field{
			{Name: "X", Type: &Desc{Kind: KindString, Name: "string"}},
			{Name: x, Type: &Desc{Kind: KindInt, Name: "int", Width: 8}},
		}}
	}
	w := NewWriter()
	if err := w.WriteDesc(mk("A")); err != nil {
		t.Fatal(err)
	}
	n := len(w.buf)
	// same structure: REF (one byte here — id 0 rides the inline form)
	if err := w.WriteDesc(mk("A")); err != nil {
		t.Fatalf("identical re-emit: %v", err)
	}
	if got := len(w.buf) - n; got != 1 {
		t.Fatalf("identical re-emit wrote %d bytes, want a 1-byte REF", got)
	}
	// same name, different structure: loud reject, no bytes
	if err := w.WriteDesc(mk("B")); err == nil {
		t.Fatal("structural mismatch must reject")
	}
	if len(w.buf) != n+1 {
		t.Fatalf("rejected write must not emit bytes")
	}
}

// Recursive type graphs compare co-inductively: the back-edge pair
// terminates the walk; a mismatch on any forward path still rejects.
func TestDescEqualRecursive(t *testing.T) {
	node := func(val string) *Desc {
		n := &Desc{Kind: KindStruct, Name: "n.Node", Fields: []Field{
			{Name: "V", Type: &Desc{Kind: KindString, Name: "string"}},
		}}
		n.Fields = append(n.Fields, Field{Name: "Next", Type: &Desc{
			Kind: KindPointer, Name: "*n.Node", Refs: []*Desc{n}}})
		_ = val
		return n
	}
	if !descEqual(node("a"), node("b")) {
		t.Fatal("identical recursive graphs must be equal")
	}
	other := &Desc{Kind: KindStruct, Name: "n.Node", Fields: []Field{
		{Name: "V", Type: &Desc{Kind: KindString, Name: "string"}},
		{Name: "Next", Type: &Desc{Kind: KindInt, Name: "int", Width: 8}},
	}}
	if descEqual(node("a"), other) {
		t.Fatal("recursive vs flat mismatch must be unequal")
	}
}

// A crafted stream mixing the kind-14 spelling and the canonical kind-15
// descriptors under the one name "big.Int" is a structural rebinding the
// claim discipline rejects: one name, one structure, in both directions.
func TestWriteDescBigintMixReject(t *testing.T) {
	w := NewWriter()
	adapter := &Desc{Kind: KindCoder, Name: "big.Int", Tag: 0}
	if err := w.WriteDesc(adapter); err != nil {
		t.Fatal(err)
	}
	canonical := &Desc{Kind: KindBigint, Name: "big.Int"}
	if err := w.WriteDesc(canonical); err == nil {
		t.Fatal("kind-14/kind-15 mix under one name must reject")
	}
	// and the reverse order rejects the same way
	w2 := NewWriter()
	if err := w2.WriteDesc(canonical); err != nil {
		t.Fatal(err)
	}
	if err := w2.WriteDesc(adapter); err == nil {
		t.Fatal("kind-15/kind-14 mix under one name must reject")
	}
}
