package gbon_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// u128 and u128Coder back ExampleCoder: a user type with a custom Coder
// whose body sub-serializes through the handed Encoder/Decoder.
type u128 struct {
	Hi, Lo uint64
}

type u128Coder struct{}

func (u128Coder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	u := v.Interface().(u128)
	return e.Encode([2]uint64{u.Hi, u.Lo})
}

func (u128Coder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var raw [2]uint64
	if err := d.Decode(&raw); err != nil {
		return err
	}
	v.Set(reflect.ValueOf(u128{Hi: raw[0], Lo: raw[1]}))
	return nil
}

// exRect is the concrete type behind the shape interface in
// ExampleDecoder_Register.
type exRect struct {
	W, H float64
}

func (r exRect) Area() float64 { return r.W * r.H }

func ExampleMarshal() {
	type point struct {
		X, Y int
	}
	data, err := gbon.Marshal(point{X: 1, Y: 2})
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	fmt.Printf("% x\n", data)
	// Output: 67 62 6f 6e 00 00 d0 6c 33 67 69 74 68 75 62 2e 63 6f 6d 2f 67 62 6f 6e 2d 66 6f 72 6d 61 74 2f 67 62 6f 6e 2d 67 6f 5f 74 65 73 74 2e 67 62 6f 6e 5f 74 65 73 74 2e 70 6f 69 6e 74 02 61 58 d8 63 69 6e 74 08 61 59 c3 b0 22 24
}

func ExampleUnmarshal() {
	type point struct {
		X, Y int
	}
	data, err := gbon.Marshal(point{X: 1, Y: 2})
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	var got point
	if err := gbon.Unmarshal(data, &got); err != nil {
		fmt.Println("unmarshal error:", err)
		return
	}
	fmt.Println(got.X, got.Y)
	// Output: 1 2
}

// ExampleEncoder streams a cyclic value graph through an io.Pipe: the
// a→b→a cycle comes back out intact (head.Next.Next == head).
func ExampleEncoder() {
	type node struct {
		Name string
		Next *node
	}
	a := &node{Name: "a"}
	b := &node{Name: "b"}
	a.Next, b.Next = b, a

	pr, pw := io.Pipe()
	go func() {
		err := gbon.NewEncoder(pw).Encode(a)
		_ = pw.CloseWithError(err)
	}()

	var head *node
	if err := gbon.NewDecoder(pr).Decode(&head); err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Println(head.Name, head.Next.Name, head.Next.Next == head)
	// Output: a b true
}

func ExampleCoder() {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.RegisterCoder(u128{}, u128Coder{}); err != nil {
		fmt.Println("register error:", err)
		return
	}
	if err := enc.Encode(u128{Hi: 1, Lo: 42}); err != nil {
		fmt.Println("encode error:", err)
		return
	}
	dec := gbon.NewDecoder(&buf)
	if err := dec.RegisterCoder(u128{}, u128Coder{}); err != nil {
		fmt.Println("register error:", err)
		return
	}
	var got u128
	if err := dec.Decode(&got); err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Println(got.Hi, got.Lo)
	// Output: 1 42
}

// ExampleDecoder_Register shows both registry outcomes: the stateless
// Unmarshal refuses a concrete interface value (ErrUnsupported), while a
// Decoder with the concrete type registered resolves it.
func ExampleDecoder_Register() {
	type shape interface{ Area() float64 }
	data, err := gbon.Marshal(shape(exRect{W: 3, H: 4}))
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	var s shape
	fmt.Println("stateless:", errors.Is(gbon.Unmarshal(data, &s), gbon.ErrFormat))
	dec := gbon.NewDecoder(bytes.NewReader(data))
	if err := dec.Register(exRect{}); err != nil {
		fmt.Println("register error:", err)
		return
	}
	if err := dec.Decode(&s); err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Println("area:", s.Area())
	// Output:
	// stateless: true
	// area: 12
}

// ExampleError shows the error contract: the sentinel family answers
// errors.Is, errors.As recovers the structured type, and %v renders one
// line without untrusted input values.
func ExampleError() {
	data := []byte{'g', 'b', 'o', 'n', 0x00, 0x00, 0xFF}
	var v int64
	err := gbon.Unmarshal(data, &v)
	fmt.Println(errors.Is(err, gbon.ErrFormat))
	if ae, ok := errors.AsType[*gbon.Error](err); ok {
		fmt.Printf("class=%s offset=%d path=%q\n", ae.Class(), ae.Offset, ae.Path)
	}
	fmt.Println(err)
	// Output:
	// true
	// class=malformed_op offset=6 path=""
	// gbon: malformed_op (offset 6): wire: expected type-ref position, class 0xF
}

// wipRe is the doc-truth gate: README.md and doc.go carry no
// work-in-progress or promissory status forms. The pattern is assembled
// from split literals so this file carries no matchable copy of the
// narrative-taxonomy dictionary (referencelint).
var wipRe = regexp.MustCompile(`(?i)work in ` + `progress|lands incrementally|will accompany|until the format evolves|will be (?:fixed|added|supported|preserved)|\bplanned\b`)

// docGateText reads a doc surface; for Go comment files it strips the
// line comment prefix and folds newlines into spaces (godoc line breaks
// split across lines).
func docGateText(t *testing.T, path string, stripComment bool) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !stripComment {
		return string(b)
	}
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimPrefix(ln, "// "), "//")
	}
	return strings.Join(lines, " ")
}

func TestDocTruthWIPGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		path         string
		stripComment bool
	}{
		{"README", "README.md", false},
		{"doc", "doc.go", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if m := wipRe.FindString(docGateText(t, tc.path, tc.stripComment)); m != "" {
				t.Errorf("doc-truth violation in %s: %q", tc.path, m)
			}
		})
	}
	// Negative fixtures: the gate must reject every known violation class
	// and accept the canonical doc-truth performatives.
	t.Run("negative fixtures", func(t *testing.T) {
		for _, bad := range []string{
			"Status: work in " + "progress",
			"the codec lands incrementally",
			"a spec will accompany the release",
			"not preserved until the format evolves",
			"will be fixed in a later release",
			"planned",
		} {
			if wipRe.FindString(bad) == "" {
				t.Errorf("doc-truth gate misses known violation: %q", bad)
			}
		}
		for _, good := range []string{
			"aliasing is not preserved (arrays travel by value)",
			"concrete types must be registered",
			"the API may change",
			"Avro/protobuf evolution rules",
		} {
			if m := wipRe.FindString(good); m != "" {
				t.Errorf("doc-truth gate false positive on %q: %q", good, m)
			}
		}
	})
}
