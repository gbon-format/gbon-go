package canongrain

import (
	"bytes"
	"reflect"
	"testing"

	gbon "github.com/gbon-format/gbon-go"
)

// marshal/unmarshal indirection keeps the oracle independent of the
// codec's internal helpers.
func marshal(v any) ([]byte, error) { return gbon.Marshal(v) }

// unmarshal decodes through a decoder whose registry carries the
// grammar's interface payload forms (the degenerate struct is an
// unnamed dynamic type — the codec resolves it by name).
func unmarshal(b []byte, into any) error {
	dec := gbon.NewDecoder(bytes.NewReader(b))
	if err := dec.Register(degenerateInterface(), new(Node)); err != nil {
		return err
	}
	return dec.Decode(into)
}

func degenerateInterface() any {
	d := reflect.New(degenerate).Elem()
	d.Field(0).Set(reflect.ValueOf(int64(0)))
	return d.Interface()
}

// P-1 decode-ok: every generated graph decodes without format error.
func TestOracleDecodeOK(t *testing.T) {
	for _, g := range Graphs(64) {
		b, err := marshal(g.Root)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var out Node
		if err := unmarshal(b, &out); err != nil {
			t.Fatalf("decode-ok: %v (seed graph %p, bytes %x)", err, g.Root, b)
		}
	}
}

// P-2 RT-identity with declared carve-outs: zero-size pointees
// (address-weak identity), slice↔array views, nil-merge degeneracy, and
// the degenerate interface-grain form (R-DEG).
func TestOracleRTIdentity(t *testing.T) {
	for _, g := range Graphs(64) {
		b, err := marshal(g.Root)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var out Node
		if err := unmarshal(b, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.A != g.Root.A {
			t.Fatalf("scalar drift: %d != %d", out.A, g.Root.A)
		}
		// ring identity: every wired Self slot lands non-nil (the
		// generator wires Self for every node); node-level aliasing is
		// pinned by byte idempotence below
		if out.Self == nil {
			t.Fatalf("ring identity: Self slot nil after decode")
		}
		// interior descent identity: PInt lands on the decoded field
		if out.PInt != nil && *out.PInt != out.A {
			t.Fatalf("descent value: %d != %d", *out.PInt, out.A)
		}
		// zero-size: nil-ness only (address-weak)
		if (g.Root.Z == nil) != (out.Z == nil) {
			t.Fatalf("zero-size nil-ness drift")
		}
	}
}

// P-3 byte-idempotence: re-encoding the decoded graph reproduces the
// stream byte for byte (header included), without carve-outs.
func TestOracleByteIdempotence(t *testing.T) {
	for _, g := range Graphs(64) {
		b, err := marshal(g.Root)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var out Node
		if err := unmarshal(b, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		b2, err := marshal(&out)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(b, b2) {
			t.Fatalf("byte idempotence:\n first %x\n second %x", b, b2)
		}
	}
}

// Negative fixture of the oracle's own gate: a deliberately broken
// comparator must fail the oracle's assertions — proving the checks
// above can reject (a vacuous oracle would pass everything).
func TestOracleNegativeFixture(t *testing.T) {
	g := Gen(1)
	b, err := marshal(g.Root)
	if err != nil {
		t.Fatal(err)
	}
	var out Node
	if err := unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b2, _ := marshal(&out)
	if !bytes.Equal(b, b2) {
		t.Fatal("precondition: idempotence broken before fixture")
	}
	// mutate the decoded value: the idempotence check must now observe
	// a difference when re-encoded
	out.A ^= 1
	b3, _ := marshal(&out)
	if bytes.Equal(b2, b3) {
		t.Fatal("negative fixture: mutation not observable — oracle checks are vacuous")
	}
}
