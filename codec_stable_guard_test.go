package gbon

// Stable-mode guard tests: the dynamic predicate (E5 tie and E4 zero
// float key rejects with deterministic class and path), the static
// conservative predicate over types, sub-marshal flag inheritance,
// nested-violation propagation, and off-by-default behavior.

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// tieBody is a pointer key body: two distinct pointers to congruent
// bodies walk equal rank cells, so the pair order falls through to the
// pointer sequence when the values are equal.
type tieBody struct {
	S string
	N int
}

func tieFixture() map[*tieBody]int {
	a := &tieBody{S: "k", N: 3}
	b := &tieBody{S: "k", N: 3}
	return map[*tieBody]int{a: 7, b: 7}
}

func tieValDiffFixture() map[*tieBody]int {
	a := &tieBody{S: "k", N: 3}
	b := &tieBody{S: "k", N: 3}
	return map[*tieBody]int{a: 1, b: 2}
}

func guardClass(t *testing.T, err error) (class, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a guard rejection, got success")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("guard error does not answer ErrUnsupported: %v", err)
	}
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("guard error not As-recoverable: %v", err)
	}
	return ae.Class(), ae.Path
}

// TestStableGuardE5Tie: equal-valued pointer-tied pairs reject with the
// unstable_tie_break class and a path naming the offending map pair.
func TestStableGuardE5Tie(t *testing.T) {
	class, path := guardClass(t, func() error { _, err := MarshalStable(tieFixture()); return err }())
	if class != "unstable_tie_break" {
		t.Fatalf("class = %q, want unstable_tie_break", class)
	}
	if path == "" {
		t.Fatalf("guard error carries no path: %v", path)
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.SetStable(true)
	if err := enc.Encode(tieFixture()); err == nil {
		t.Fatal("streaming encoder with stable mode accepted a tied map")
	}
}

// TestStableGuardE5ValDiff: pointer keys with byte-equal skeletons and
// differing pair value bytes stay inside the class — the value-bytes
// phase orders them, no identity discriminator applies.
func TestStableGuardE5ValDiff(t *testing.T) {
	v := tieValDiffFixture()
	sb, err := MarshalStable(v)
	if err != nil {
		t.Fatalf("in-class pointer-keyed map rejected: %v", err)
	}
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("default marshal: %v", err)
	}
	if !bytes.Equal(sb, b) {
		t.Fatal("stable-mode bytes differ from default-mode bytes inside the class")
	}
}

// TestStableGuardE5TieBehindValDiff: a tied pair sitting behind a
// value-differing pair inside one rank-equal block still rejects — the
// scan cannot stop at the first rank-equal adjacency.
func TestStableGuardE5TieBehindValDiff(t *testing.T) {
	a := &tieBody{S: "k", N: 1}
	b := &tieBody{S: "k", N: 1}
	c := &tieBody{S: "k", N: 1}
	m := map[*tieBody]int{a: 5, b: 7, c: 7}
	class, path := guardClass(t, func() error { _, err := MarshalStable(m); return err }())
	if class != "unstable_tie_break" {
		t.Fatalf("class = %q, want unstable_tie_break", class)
	}
	if path == "" {
		t.Fatal("guard error carries no path")
	}
}

// TestStableGuardE4Zero: a zero float map key of either sign rejects
// with the unstable_zero_float_key class; nonzero keys, and complex keys
// with nonzero components, stay inside the class.
func TestStableGuardE4Zero(t *testing.T) {
	type fkey struct{ F float64 }
	neg := math.Copysign(0, -1)
	for name, m := range map[string]any{
		"pos-zero-f64":   map[float64]int{0: 1},
		"neg-zero-f64":   map[float64]int{neg: 1},
		"pos-zero-f32":   map[float32]int{0: 1},
		"neg-zero-f32":   map[float32]int{float32(neg): 1},
		"zero-field":     map[fkey]int{{F: neg}: 1},
		"zero-array":     map[[2]float64]int{{0, 1.5}: 1},
		"zero-re-c128":   map[complex128]int{complex(0, 0): 1},
		"zero-im-c128":   map[complex128]int{complex(1.5, 0): 1},
		"zero-part-c64":  map[complex64]int{complex(0, 2.5): 1},
		"neg-zero-c128":  map[complex128]int{complex(neg, 1.5): 1},
		"dyn-zero-f64":   map[any]int{float64(neg): 1},
		"dyn-zero-field": map[any]int{fkey{F: 0}: 1},
	} {
		class, path := guardClass(t, func() error { _, err := MarshalStable(m); return err }())
		if class != "unstable_zero_float_key" {
			t.Fatalf("%s: class = %q, want unstable_zero_float_key", name, class)
		}
		if path == "" {
			t.Fatalf("%s: guard error carries no path", name)
		}
	}
	for name, m := range map[string]any{
		"nonzero-f64":  map[float64]int{1.5: 1, -2.5: 2},
		"nonzero-c128": map[complex128]int{complex(1.5, 2.5): 1, complex(-1.5, 0.5): 2},
		"nonzero-c64":  map[complex64]int{complex(0.5, 0.25): 1},
		"zero-value":   map[string]float64{"a": neg},
	} {
		sb, err := MarshalStable(m)
		if err != nil {
			t.Fatalf("%s: in-class map rejected: %v", name, err)
		}
		b, err := Marshal(m)
		if err != nil {
			t.Fatalf("%s: default marshal: %v", name, err)
		}
		if !bytes.Equal(sb, b) {
			t.Fatalf("%s: stable bytes differ from default bytes", name)
		}
	}
}

// TestStableGuardDeterminism: the rejected class and path are identical
// across map-iteration seeds (sixteen insertion permutations of the same
// slot content) for both rules.
func TestStableGuardDeterminism(t *testing.T) {
	keys := []*tieBody{
		{S: "k", N: 1}, {S: "k", N: 1}, {S: "k", N: 1},
		{S: "j", N: 2}, {S: "j", N: 2},
	}
	var wantClass, wantPath string
	for seed := range int64(16) {
		rng := rand.New(rand.NewSource(seed))
		m := make(map[*tieBody]int, len(keys))
		for _, j := range rng.Perm(len(keys)) {
			m[keys[j]] = 9
		}
		class, path := guardClass(t, func() error { _, err := MarshalStable(m); return err }())
		if seed == 0 {
			wantClass, wantPath = class, path
			continue
		}
		if class != wantClass || path != wantPath {
			t.Fatalf("seed %d: class/path = %q/%q, want %q/%q", seed, class, path, wantClass, wantPath)
		}
	}
	fl := []float64{0, 1.5, -2.5, 3.25, 4.75, -5.5, 6.5, 7.5}
	wantFClass, wantFPath := "", ""
	for seed := range int64(16) {
		rng := rand.New(rand.NewSource(seed))
		m := make(map[float64]int, len(fl))
		for _, j := range rng.Perm(len(fl)) {
			m[fl[j]] = j
		}
		class, path := guardClass(t, func() error { _, err := MarshalStable(m); return err }())
		if class != "unstable_zero_float_key" {
			t.Fatalf("seed %d: class = %q, want unstable_zero_float_key", seed, class)
		}
		if seed == 0 {
			wantFClass, wantFPath = class, path
			continue
		}
		if class != wantFClass || path != wantFPath {
			t.Fatalf("seed %d: class/path = %q/%q, want %q/%q", seed, class, path, wantFClass, wantFPath)
		}
	}
}

// TestStableStaticVerdicts: the plan-level conservative predicate over
// types — interfaces anywhere and map keys carrying pointer, interface,
// float, or complex components force the dynamic path; a stable verdict
// never covers an unstable-capable type, and stable-verdict values
// encode identically in both modes.
func TestStableStaticVerdicts(t *testing.T) {
	type ifaceHolder struct{ I any }
	type floatKey struct{ F float64 }
	type recNode struct {
		N    int
		Next *recNode
	}
	stable := []reflect.Type{
		reflect.TypeFor[int](),
		reflect.TypeFor[string](),
		reflect.TypeFor[[]int](),
		reflect.TypeFor[map[string]int](),
		reflect.TypeFor[map[[2]int64]string](),
		reflect.TypeFor[map[string]*int](),
		reflect.TypeFor[[]map[string]int](),
		reflect.TypeFor[*map[string]int](),
		reflect.TypeFor[[3]map[string]string](),
	}
	unstable := []reflect.Type{
		reflect.TypeFor[map[*int]int](),
		reflect.TypeFor[map[float64]int](),
		reflect.TypeFor[map[complex128]string](),
		reflect.TypeFor[map[any]int](),
		reflect.TypeFor[any](),
		reflect.TypeFor[ifaceHolder](),
		reflect.TypeFor[[]any](),
		reflect.TypeFor[map[string]map[float64]int](),
		reflect.TypeFor[map[[2]float64]int](),
		reflect.TypeFor[map[floatKey]string](),
		reflect.TypeFor[recNode](),
	}
	for _, tt := range stable {
		if pl := buildTypePlan(tt, nil, nil); !pl.stable {
			t.Fatalf("static verdict: %s flagged unstable", tt)
		}
	}
	for _, tt := range unstable {
		if pl := buildTypePlan(tt, nil, nil); pl.stable {
			t.Fatalf("static verdict: %s flagged stable", tt)
		}
	}
	inClass := []any{
		map[string]int{"a": 1, "b": 2, "c": 3},
		map[[2]int64]string{{1, 2}: "x", {3, 4}: "y"},
		map[string]*int{"p": new(int), "q": new(int)},
		[]map[string]int{{"a": 1}, {"b": 2}},
	}
	for _, v := range inClass {
		sb, err := MarshalStable(v)
		if err != nil {
			t.Fatalf("stable-verdict value rejected: %v", err)
		}
		b, err := Marshal(v)
		if err != nil {
			t.Fatalf("default marshal: %v", err)
		}
		if !bytes.Equal(sb, b) {
			t.Fatalf("%T: stable bytes differ from default bytes", v)
		}
	}
}

// TestStableGuardNested: an inner violation reached through a value of
// an outer map rejects the outer call with the inner rule's class; the
// path runs through the value position.
func TestStableGuardNested(t *testing.T) {
	type wrap struct{ M map[*tieBody]int }
	a := &tieBody{S: "k", N: 1}
	b := &tieBody{S: "k", N: 1}
	outer := map[string]wrap{
		"one": {M: map[*tieBody]int{a: 5, b: 5}},
		"two": {M: map[*tieBody]int{}},
	}
	class, _ := guardClass(t, func() error { _, err := MarshalStable(outer); return err }())
	if class != "unstable_tie_break" {
		t.Fatalf("class = %q, want unstable_tie_break", class)
	}
	deep := map[string][]wrap{"k": {{M: map[*tieBody]int{a: 5, b: 5}}}}
	class, _ = guardClass(t, func() error { _, err := MarshalStable(deep); return err }())
	if class != "unstable_tie_break" {
		t.Fatalf("deep: class = %q, want unstable_tie_break", class)
	}
}

// TestStableSubmarshalInherit: the stable flag rides the scoped
// sub-marshal of pair values — an in-class outer pointer-keyed map whose
// values embed zero-float-key maps rejects with the inner E4 class.
func TestStableSubmarshalInherit(t *testing.T) {
	type inner struct{ M map[float64]int }
	type outerVal struct{ X inner }
	a := &tieBody{S: "k", N: 1}
	b := &tieBody{S: "k", N: 1}
	m := map[*tieBody]outerVal{
		a: {X: inner{M: map[float64]int{0: 1}}},
		b: {X: inner{M: map[float64]int{0: 2}}},
	}
	class, _ := guardClass(t, func() error { _, err := MarshalStable(m); return err }())
	if class != "unstable_zero_float_key" {
		t.Fatalf("class = %q, want unstable_zero_float_key", class)
	}
}

// TestStableGuardBothRules: a map violating both rules at once reports
// the first offender in sorted order — the float-zero slot below the
// pointer tie when the tied bodies sort after the float token, and the
// tie below the zero when zero-size pointees sort first.
func TestStableGuardBothRules(t *testing.T) {
	m := map[any]int{
		float64(0):             1,
		&tieBody{S: "k", N: 1}: 2,
		&tieBody{S: "k", N: 1}: 2,
	}
	// Interface keys carry their descriptor cells ahead of the value
	// tokens, so the tied pair sorts ahead of the zero-float slot here;
	// the guard reports the first offender in sorted order (the
	// tie-break choice of the both-rules case, pinned deterministically).
	class, path := guardClass(t, func() error { _, err := MarshalStable(m); return err }())
	if class != "unstable_tie_break" {
		t.Fatalf("both-rules fixture: class = %q, want unstable_tie_break", class)
	}
	if path == "" {
		t.Fatal("both-rules fixture: guard error carries no path")
	}
	for seed := range int64(8) {
		rng := rand.New(rand.NewSource(seed))
		mm := make(map[any]int, 3)
		vals := []any{float64(0), &tieBody{S: "k", N: 1}, &tieBody{S: "k", N: 1}}
		for _, j := range rng.Perm(len(vals)) {
			switch x := vals[j].(type) {
			case float64:
				mm[x] = 1
			case *tieBody:
				mm[x] = 2
			}
		}
		c2, p2 := guardClass(t, func() error { _, err := MarshalStable(mm); return err }())
		if c2 != class || p2 != path {
			t.Fatalf("seed %d: class/path = %q/%q, want %q/%q", seed, c2, p2, class, path)
		}
	}
}

// TestStableOffByDefault: the same fixtures encode successfully without
// the mode — the reject assertions of the guard tests are mode-bound
// (this is the toggled-off variant of every reject above).
func TestStableOffByDefault(t *testing.T) {
	for name, v := range map[string]any{
		"tie":   tieFixture(),
		"zero":  map[float64]int{0: 1},
		"mixed": map[any]int{float64(0): 1, &tieBody{S: "k", N: 1}: 2, &tieBody{S: "k", N: 1}: 2},
	} {
		b1, err := Marshal(v)
		if err != nil {
			t.Fatalf("%s: default mode rejected: %v", name, err)
		}
		b2, err := Marshal(v)
		if err != nil {
			t.Fatalf("%s: default mode re-encode: %v", name, err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("%s: default-mode bytes not deterministic", name)
		}
	}
}

// TestStableGuardRenderProbe logs the quoted one-line and verbose
// renders of the two guard classes (baseline derivation witness for the
// snippet identity corpus).
func TestStableGuardRenderProbe(t *testing.T) {
	for _, m := range []any{
		map[*int]int{new(int): 1, new(int): 1},
		map[float64]int{0: 1},
	} {
		_, err := MarshalStable(m)
		if err == nil {
			t.Fatalf("probe fixture accepted: %T", m)
		}
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("probe error not As-recoverable: %v", err)
		}
		t.Logf("render %q", [2]string{ae.Error(), fmt.Sprintf("%+v", ae)})
	}
}
