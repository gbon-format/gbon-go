package canongrain

import (
	"fmt"
	"reflect"
)

// The KG-1 spike grammar: 13 field-type palette entries × 4 container
// shapes (52 forms) + 12 static forms + the ring form = 65, plus the
// CX counterexample positives as dedicated graphs.

// Static named types of the grammar (conversions and hazards need
// static type identity).
type (
	Leaf struct {
		Next *Leaf
		V    int64
	}
	SConvA   struct{ A int64 }
	SConvB   struct{ A int64 }
	ZS       struct{}
	ZeroLead struct {
		A struct{}
		B int64
	}
	ZeroArrLead struct {
		A [0]int64
		B int64
	}
	ZeroEmbedLead struct {
		ZS
		B int64
	}
	OnePast struct {
		F1 int64
		F2 struct{}
	}
	MidField struct {
		H int64
		F *Leaf
	}
	IfaceLead struct {
		I any
		T int64
	}
	ChainLead struct {
		P **Leaf
		T int64
	}
	EmbedLeaf struct {
		Leaf
		Tail int64
	}
	EmbedPtr struct {
		*Leaf
		Tail int64
	}
	LeafArr   [2]Leaf
	LeafSlice []Leaf
	ConvGraph struct {
		PA *SConvA
		PB *SConvB
	}
	InteriorT struct {
		H int64
		L struct{}
		M int64
	}
	InteriorW struct {
		PL *struct{}
		PM *int64
	}
)

var (
	tInt64 = reflect.TypeFor[int64]()
	tVoid  = reflect.TypeFor[struct{}]()
	tIface = reflect.TypeFor[any]()
)

// palette is the 13-entry field-type axis of the grammar.
var palette = []reflect.Type{
	tInt64,
	reflect.TypeFor[*int64](),
	reflect.TypeFor[*Leaf](),
	tIface,
	reflect.TypeFor[[]int64](),
	reflect.TypeFor[[2]int64](),
	reflect.TypeFor[map[string]int64](),
	reflect.TypeFor[string](),
	tVoid,
	reflect.TypeFor[[0]int64](),
	reflect.TypeFor[struct{ X int64 }](),
	reflect.TypeFor[**Leaf](),
	reflect.TypeFor[***int64](),
}

func safeStructOf(fs []reflect.StructField) (t reflect.Type, ok bool) {
	defer func() {
		if recover() != nil {
			t, ok = nil, false
		}
	}()
	return reflect.StructOf(fs), true
}

// RingRoot wraps the self-ring root form: the graph whose root is the
// 1-ring leaf itself.
type RingRoot struct {
	R *Leaf
}

// GrammarForms enumerates the 65 spike forms: palette × shapes, the
// static forms, and the ring root.
func GrammarForms() []reflect.Type {
	var out []reflect.Type
	shapes := []func(pal reflect.Type) []reflect.StructField{
		func(pal reflect.Type) []reflect.StructField {
			return []reflect.StructField{{Name: "L", Type: pal}}
		},
		func(pal reflect.Type) []reflect.StructField {
			return []reflect.StructField{{Name: "L", Type: pal}, {Name: "Tail", Type: tInt64}}
		},
		func(pal reflect.Type) []reflect.StructField {
			return []reflect.StructField{{Name: "Z", Type: tVoid}, {Name: "L", Type: pal}}
		},
		func(pal reflect.Type) []reflect.StructField {
			return []reflect.StructField{{Name: "H", Type: tInt64}, {Name: "L", Type: pal}, {Name: "T", Type: tInt64}}
		},
	}
	for _, p := range palette {
		for _, sh := range shapes {
			if t, ok := safeStructOf(sh(p)); ok && t != nil {
				out = append(out, t)
			}
		}
	}
	out = append(out,
		reflect.TypeFor[EmbedLeaf](),
		reflect.TypeFor[EmbedPtr](),
		reflect.TypeFor[ZeroLead](),
		reflect.TypeFor[ZeroArrLead](),
		reflect.TypeFor[ZeroEmbedLead](),
		reflect.TypeFor[OnePast](),
		reflect.TypeFor[MidField](),
		reflect.TypeFor[IfaceLead](),
		reflect.TypeFor[ChainLead](),
		reflect.TypeFor[LeafArr](),
		reflect.TypeFor[LeafSlice](),
		reflect.TypeFor[**Leaf](),
		reflect.TypeFor[RingRoot](),
	)
	return out
}

// newLeaf builds the grammar's 1-ring leaf.
func newLeaf() *Leaf {
	n := &Leaf{V: 1}
	n.Next = n
	return n
}

// fill populates a value with live data (no nil holes below the depth
// cap); interface slots receive ring leaves, and every fourth one the
// degenerate payload form.
func fill(v reflect.Value, depth int) {
	if !v.IsValid() || depth > 3 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(v.Elem(), depth+1)
	case reflect.Interface:
		if v.IsNil() {
			if depth%4 == 2 {
				v.Set(reflect.ValueOf(degenerateValue(int64(depth))))
			} else {
				v.Set(reflect.ValueOf(newLeaf()))
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range 2 {
			fill(v.Index(i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		val := reflect.New(v.Type().Elem()).Elem()
		fill(val, depth+1)
		m.SetMapIndex(reflect.New(v.Type().Key()).Elem(), val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fill(v.Field(i), depth+1)
		}
	case reflect.Array:
		if v.Len() > 0 {
			fill(v.Index(0), depth+1)
		}
	}
}

// cxGraphs builds the CX counterexample positives: named conversions
// over one address (CX-2), the interior zero-size collision (CX-1b),
// zero-size markers (CX-4), and the zero-size lead (CX-1).
func cxGraphs() []any {
	a := &SConvA{A: 5}
	cg := &ConvGraph{PA: a, PB: (*SConvB)(a)}
	t := &InteriorT{H: 1, M: 2}
	w := &InteriorW{PL: &t.L, PM: &t.M}
	return []any{cg, w, &struct{ P *struct{} }{P: &struct{}{}}, &ZeroLead{B: 3}}
}

// normalize reduces a decoded graph for the RT comparison: nil-merged
// pointer chains and zero-size pointees compare as nil (the declared
// carve-outs); everything else compares deeply.
func normalize(v reflect.Value, depth int) any {
	if !v.IsValid() || depth > 6 {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || v.Type().Elem().Size() == 0 {
			return nil
		}
		inner := normalize(v.Elem(), depth+1)
		if inner == nil {
			return nil
		}
		return inner
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return normalize(v.Elem(), depth+1)
	case reflect.Struct:
		parts := make([]any, 0, v.NumField())
		for i := 0; i < v.NumField(); i++ {
			parts = append(parts, normalize(v.Field(i), depth+1))
		}
		return parts
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		parts := make([]any, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			parts = append(parts, normalize(v.Index(i), depth+1))
		}
		return parts
	case reflect.Array:
		parts := make([]any, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			parts = append(parts, normalize(v.Index(i), depth+1))
		}
		return parts
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		keys := v.MapKeys()
		parts := make([][2]any, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, [2]any{fmt.Sprint(normalize(k, depth+1)), normalize(v.MapIndex(k), depth+1)})
		}
		return parts
	}
	return v.Interface()
}
