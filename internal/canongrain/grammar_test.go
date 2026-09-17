package canongrain

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
	"time"

	gbon "github.com/gbon-format/gbon-go"
)

// The grammar suite runs the full KG-1 spike grammar (65 forms) and
// the CX positives through the three predicates: decode-ok, normalized
// RT identity (declared carve-outs), and byte idempotence.
func TestOracleGrammar(t *testing.T) {
	start := time.Now()
	forms := GrammarForms()
	if len(forms) != 65 {
		t.Fatalf("grammar forms = %d, want 65", len(forms))
	}
	graphs := 0
	for _, ft := range forms {
		v := reflect.New(ft).Elem()
		fill(v, 0)
		if ft == reflect.TypeFor[RingRoot]() {
			v.Field(0).Set(reflect.ValueOf(newLeaf()))
		}
		root := v.Interface()
		b, err := gbon.Marshal(root)
		if err != nil {
			t.Fatalf("form %s encode: %v", ft, err)
		}
		out := reflect.New(ft).Interface()
		if err := grammarDecode(b, out); err != nil {
			t.Fatalf("form %s decode: %v (bytes %x)", ft, err, b)
		}
		graphs++
		got := reflect.ValueOf(out).Elem()
		if !deepEq(normalize(got, 0), normalize(v, 0)) {
			t.Fatalf("form %s RT drift:\n got  %#v\n want %#v", ft, normalize(got, 0), normalize(v, 0))
		}
		b2, err := gbon.Marshal(reflect.ValueOf(out).Elem().Interface())
		if err != nil {
			t.Fatalf("form %s re-encode: %v", ft, err)
		}
		if !bytes.Equal(b, b2) {
			t.Fatalf("form %s byte idempotence:\n first %x\n second %x", ft, b, b2)
		}
	}
	for i, g := range cxGraphs() {
		b, err := gbon.Marshal(g)
		if err != nil {
			t.Fatalf("cx%d encode: %v", i, err)
		}
		out := reflect.New(reflect.TypeOf(g)).Interface()
		if err := grammarDecode(b, out); err != nil {
			t.Fatalf("cx%d decode: %v (bytes %x)", i, err, b)
		}
		graphs++
		got := reflect.ValueOf(out).Elem()
		if !deepEq(normalize(got, 0), normalize(reflect.ValueOf(g), 0)) {
			t.Fatalf("cx%d RT drift: %#v vs %#v", i, normalize(got, 0), normalize(reflect.ValueOf(g), 0))
		}
		b2, _ := gbon.Marshal(reflect.ValueOf(out).Elem().Interface())
		if !bytes.Equal(b, b2) {
			t.Fatalf("cx%d byte idempotence: %x vs %x", i, b, b2)
		}
	}
	t.Logf("grammar suite: %d forms, %d graphs, %s", len(forms), graphs, time.Since(start))
}

// grammarDecode decodes through a decoder whose registry carries the
// grammar's interface payload forms.
func grammarDecode(b []byte, into any) error {
	dec := gbon.NewDecoder(bytes.NewReader(b))
	if err := dec.Register(new(Leaf), degenerateInterface()); err != nil {
		return err
	}
	return dec.Decode(into)
}

// deepEq compares normalized graphs by their structural rendering.
func deepEq(a, b any) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}
