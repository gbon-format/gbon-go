package gbon_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// --- K2: RegisterAs wire-name binding ---

type raUserV2 struct {
	ID    int64
	Name  string
	Flags uint32
}

type raUserV1 struct {
	ID   int64
	Name string
}

type raUserDrift struct {
	ID   string
	Name string
}

type raPoint struct {
	X, Y int32
}

type raA struct{ X int }
type raB struct{ Y int }

func TestRegisterAsCrossNameEvolution(t *testing.T) {
	// producer v2 under a contract name, consumer v1: dropped skipped
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterAs("app.User", raUserV2{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(raUserV2{ID: 7, Name: "n", Flags: 3}); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(&buf)
	if err := dec.RegisterAs("app.User", raUserV1{}); err != nil {
		t.Fatal(err)
	}
	var v1 raUserV1
	if err := dec.Decode(&v1); err != nil {
		t.Fatal(err)
	}
	if v1 != (raUserV1{ID: 7, Name: "n"}) {
		t.Fatalf("dropped-field evolution drift: %+v", v1)
	}

	// absent fields zero: v1 on the wire, v2 target
	var buf2 bytes.Buffer
	enc2 := gbon.NewEncoder(&buf2)
	if err := enc2.RegisterAs("app.User", raUserV1{}); err != nil {
		t.Fatal(err)
	}
	if err := enc2.Encode(raUserV1{ID: 9, Name: "x"}); err != nil {
		t.Fatal(err)
	}
	dec2 := gbon.NewDecoder(&buf2)
	if err := dec2.RegisterAs("app.User", raUserV2{}); err != nil {
		t.Fatal(err)
	}
	var v2 raUserV2
	if err := dec2.Decode(&v2); err != nil {
		t.Fatal(err)
	}
	if v2 != (raUserV2{ID: 9, Name: "x"}) {
		t.Fatalf("absent-field zero drift: %+v", v2)
	}

	// kind drift under a shared name is a format error
	var buf3 bytes.Buffer
	enc3 := gbon.NewEncoder(&buf3)
	if err := enc3.RegisterAs("app.User", raUserDrift{}); err != nil {
		t.Fatal(err)
	}
	if err := enc3.Encode(raUserDrift{ID: "7", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	dec3 := gbon.NewDecoder(&buf3)
	if err := dec3.RegisterAs("app.User", raUserV1{}); err != nil {
		t.Fatal(err)
	}
	var drift raUserV1
	if err := dec3.Decode(&drift); !errors.Is(err, gbon.ErrFormat) {
		t.Fatalf("kind drift: got %v, want ErrFormat", err)
	}
}

func TestRegisterAsIfacePosition(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterAs("app.User", raUserV2{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode([]any{raUserV2{ID: 5, Name: "in", Flags: 1}}); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(&buf)
	if err := dec.RegisterAs("app.User", raUserV1{}); err != nil {
		t.Fatal(err)
	}
	var got []any
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len drift: %d", len(got))
	}
	u, ok := got[0].(raUserV1)
	if !ok {
		t.Fatalf("iface slot resolved to %T, want raUserV1", got[0])
	}
	if u != (raUserV1{ID: 5, Name: "in"}) {
		t.Fatalf("iface evolution drift: %+v", u)
	}
}

func TestRegisterAsSymmetric(t *testing.T) {
	want := raPoint{X: -4, Y: 9}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterAs("cfg.Point", raPoint{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(want); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(&buf)
	if err := dec.RegisterAs("cfg.Point", raPoint{}); err != nil {
		t.Fatal(err)
	}
	var got raPoint
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("symmetric RT drift: %+v vs %+v", got, want)
	}
	// nameOf default stays in force without RegisterAs: same types, no
	// bindings, plain round-trip
	b, err := gbon.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var plain raPoint
	if err := gbon.Unmarshal(b, &plain); err != nil {
		t.Fatal(err)
	}
	if plain != want {
		t.Fatalf("default-name RT drift: %+v vs %+v", plain, want)
	}
}

func TestRegisterAsCollisions(t *testing.T) {
	dec := gbon.NewDecoder(bytes.NewReader(nil))
	if err := dec.RegisterAs("n.A", raA{}); err != nil {
		t.Fatal(err)
	}
	if err := dec.RegisterAs("n.A", raB{}); err == nil {
		t.Fatal("name collision accepted")
	}
	if err := dec.RegisterAs("n.B", raA{}); err == nil {
		t.Fatal("conflicting binding accepted")
	}
	if err := dec.RegisterAs("n.A", raA{}); err != nil {
		t.Fatalf("same-pair rebind not a no-op: %v", err)
	}
	for _, name := range []string{"", "int64", "string", "[]byte", "time.Time"} {
		if err := dec.RegisterAs(name, raB{}); err == nil {
			t.Fatalf("reserved name %q accepted", name)
		}
	}
	// Register under an existing binding of the same type is a no-op
	if err := dec.Register(raA{}); err != nil {
		t.Fatalf("Register of a bound type: %v", err)
	}

	enc := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc.RegisterAs("n.A", raA{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.RegisterAs("n.A", raB{}); err == nil {
		t.Fatal("encoder name collision accepted")
	}
	if err := enc.RegisterAs("n.B", raA{}); err == nil {
		t.Fatal("encoder conflicting binding accepted")
	}
	if err := enc.RegisterAs("int", raB{}); err == nil {
		t.Fatal("encoder reserved name accepted")
	}
	// binding a type already encoded in the stream is rejected
	if err := enc.Encode(raB{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.RegisterAs("n.C", raB{}); err == nil {
		t.Fatal("mid-stream binding of an encoded type accepted")
	}
}

// A binding registered between Decode calls is visible to subsequent
// Decode calls (registry lifecycle, mirrors Register).
func TestRegisterAsBetweenDecodes(t *testing.T) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(raPoint{X: 1, Y: 2}); err != nil {
		t.Fatal(err)
	}
	if err := enc.RegisterAs("app.User", raUserV2{}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(raUserV2{ID: 7, Name: "n", Flags: 3}); err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(&buf)
	var first raPoint
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first != (raPoint{X: 1, Y: 2}) {
		t.Fatalf("first record drift: %+v", first)
	}
	if err := dec.RegisterAs("app.User", raUserV2{}); err != nil {
		t.Fatal(err)
	}
	var second raUserV2
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("binding after first Decode invisible: %v", second)
	}
	if second != (raUserV2{ID: 7, Name: "n", Flags: 3}) {
		t.Fatalf("second record drift: %+v", second)
	}
}

// --- K3: auto-registered basic types ---

func TestUnmarshalBasics(t *testing.T) {
	singles := []any{int64(-42), "txt", true, 2.5}
	for _, want := range singles {
		b, err := gbon.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if err := gbon.Unmarshal(b, &got); err != nil {
			t.Fatalf("%T: %v", want, err)
		}
		if got != want {
			t.Fatalf("basic RT drift: %#v vs %#v", got, want)
		}
	}
	payload := []any{int64(1), "s", true, 2.5, []byte("bb")}
	b, err := gbon.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	if err := gbon.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Fatalf("any-payload drift: %#v vs %#v", got, payload)
	}
	// non-basic concrete types still need a registry
	bad, err := gbon.Marshal([]any{raA{X: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var leak []any
	uerr := gbon.Unmarshal(bad, &leak)
	if !errors.Is(uerr, gbon.ErrFormat) {
		t.Fatalf("unregistered struct in iface: got %v, want ErrFormat", uerr)
	}
	var lr *gbon.Error
	if !errors.As(uerr, &lr) || lr.Class() != "unknown_name" {
		t.Fatalf("unregistered struct in iface: got %v, want class unknown_name", uerr)
	}
}

func TestDecoderBasicsAutoRegister(t *testing.T) {
	b, err := gbon.Marshal([]any{int64(7)})
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(b))
	var got []any
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != any(int64(7)) {
		t.Fatalf("iface DESC(int64) drift: %#v", got)
	}
	// explicit Register of a basic type is a same-type no-op
	if err := dec.Register(int64(0), string("")); err != nil {
		t.Fatalf("Register basics no-op: %v", err)
	}
}

func TestBasicsBudgetsAlive(t *testing.T) {
	b, err := gbon.Marshal([]any{make([]byte, 8192)})
	if err != nil {
		t.Fatal(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(b))
	dec.SetLimits(gbon.Limits{MaxBytes: 64})
	var got []any
	if err := dec.Decode(&got); !errors.Is(err, gbon.ErrBudget) {
		t.Fatalf("oversized basic: got %v, want ErrBudget", err)
	}
}

// rem-c1/F1: the RegisterAs branch validates through the coder-leaf hook
// (descOfRegister), so types with time.Time fields register on the
// Decoder the same way the Register branch always has.
func TestRegisterAsCoderLeafValidation(t *testing.T) {
	type W struct {
		Built time.Time
		Name  string
	}
	r := bytes.NewReader(nil)
	dec := gbon.NewDecoder(r)
	if err := dec.RegisterAs("w", W{}); err != nil {
		t.Fatalf("dec.RegisterAs(W): %v", err)
	}
	dec2 := gbon.NewDecoder(r)
	if err := dec2.Register(W{}); err != nil {
		t.Fatalf("dec2.Register(W): %v", err)
	}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterAs("w", W{}); err != nil {
		t.Fatalf("enc.RegisterAs(W): %v", err)
	}

	want := W{Built: time.Unix(1700000000, 42).UTC(), Name: "w"}
	if err := enc.Encode(want); err != nil {
		t.Fatal(err)
	}
	dec3 := gbon.NewDecoder(&buf)
	if err := dec3.RegisterAs("w", W{}); err != nil {
		t.Fatal(err)
	}
	var got W
	if err := dec3.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Built.Equal(want.Built) || got.Name != want.Name {
		t.Fatalf("round-trip drift: %#v vs %#v", got, want)
	}
}
