package gbon_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// topoGen builds random value graphs over all reference kinds — map, slice,
// and interface positions — with pool reuse (fan-in sharing) and cycle
// injection (map-self, mutual maps, slice-through-any). Depth, fan-out, and
// the reuse/cycle probabilities are parameters, not constants: the
// dimensional axes are caller-owned.
type topoGen struct {
	r        *rand.Rand
	maxDepth int
	maxFan   int // children per container
	reusePct int // pool-pick probability over a fresh container
	cyclePct int // ancestor back-edge probability over a fresh child
	maps     []any
	slices   []any
	frames   []any // construction stack: cycle-injection targets
}

func (g *topoGen) leaf() any {
	switch g.r.Intn(4) {
	case 0:
		return int64(g.r.Int63())
	case 1:
		return fmt.Sprintf("s%d", g.r.Intn(1000))
	case 2:
		return g.r.Intn(2) == 0
	default:
		return nil
	}
}

// value builds one node at depth; children see this node through frames for
// back-edge injection.
func (g *topoGen) value(depth int) any {
	if depth >= g.maxDepth {
		return g.leaf()
	}
	roll := g.r.Intn(100)
	switch {
	case roll < g.cyclePct && len(g.frames) > 0:
		return g.frames[g.r.Intn(len(g.frames))]
	case roll < g.cyclePct+g.reusePct && len(g.maps)+len(g.slices) > 0:
		if len(g.maps) > 0 && (len(g.slices) == 0 || g.r.Intn(2) == 0) {
			return g.maps[g.r.Intn(len(g.maps))]
		}
		return g.slices[g.r.Intn(len(g.slices))]
	}
	n := 1 + g.r.Intn(g.maxFan)
	if g.r.Intn(2) == 0 {
		m := map[string]any{}
		g.maps = append(g.maps, m)
		g.frames = append(g.frames, m)
		for i := range n {
			m[fmt.Sprintf("k%d", i)] = g.value(depth + 1)
		}
		g.frames = g.frames[:len(g.frames)-1]
		return m
	}
	s := make([]any, n)
	g.slices = append(g.slices, s)
	g.frames = append(g.frames, s)
	for i := range s {
		s[i] = g.value(depth + 1)
	}
	g.frames = g.frames[:len(g.frames)-1]
	return s
}

// topoKey is the observable header identity of a reference node: the map
// pointer, or the slice (data, len, cap) triple.
type topoKey struct {
	a, b, c uintptr
}

func topoKeyOf(v reflect.Value) topoKey {
	if v.Kind() == reflect.Map {
		return topoKey{a: v.Pointer()}
	}
	return topoKey{a: v.Pointer(), b: uintptr(v.Len()), c: uintptr(v.Cap())}
}

// pathStep is one navigation move from a root: a map key or a slice index.
type pathStep struct {
	key   string
	idx   int
	isKey bool
}

func keyStep(k string) pathStep { return pathStep{key: k, isKey: true} }
func idxStep(i int) pathStep    { return pathStep{idx: i} }

// topoPaths walks the graph recording, for every reference node, the path of
// its first encounter and the duplicate paths of repeated encounters
// (sharing), plus the set of all node paths (cycle-carrying graphs repeat
// paths structurally, so duplicates close cycles too).
func topoPaths(root any) (first map[topoKey][]pathStep, dups [][2][]pathStep) {
	first = map[topoKey][]pathStep{}
	var walk func(v reflect.Value, path []pathStep)
	walk = func(v reflect.Value, path []pathStep) {
		switch v.Kind() {
		case reflect.Map:
			k := topoKeyOf(v)
			if p, seen := first[k]; seen {
				dups = append(dups, [2][]pathStep{p, append([]pathStep(nil), path...)})
				return
			}
			first[k] = append([]pathStep(nil), path...)
			iter := v.MapRange()
			for iter.Next() {
				kk := iter.Key().String()
				walk(iter.Value().Elem(), append(path, keyStep(kk)))
			}
		case reflect.Slice:
			k := topoKeyOf(v)
			if p, seen := first[k]; seen {
				dups = append(dups, [2][]pathStep{p, append([]pathStep(nil), path...)})
				return
			}
			first[k] = append([]pathStep(nil), path...)
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i).Elem(), append(path, idxStep(i)))
			}
		}
	}
	walk(reflect.ValueOf(root), nil)
	return first, dups
}

// resolvePath navigates a decoded copy along a recorded path and returns the
// node (as any) or nil when the path does not resolve.
func resolvePath(root any, path []pathStep) any {
	cur := root
	for _, st := range path {
		v := reflect.ValueOf(cur)
		switch {
		case st.isKey:
			if v.Kind() != reflect.Map {
				return nil
			}
			cur = v.MapIndex(reflect.ValueOf(st.key)).Interface()
		default:
			if v.Kind() != reflect.Slice || st.idx >= v.Len() {
				return nil
			}
			cur = v.Index(st.idx).Interface()
		}
	}
	return cur
}

// marshalGuarded runs Marshal under a wall-clock guard: a non-terminating
// walk (cycle without a visited set) fails the test instead of hanging.
func marshalGuarded(t *testing.T, v any) ([]byte, error) {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		b, err := gbon.Marshal(v)
		done <- res{b, err}
	}()
	select {
	case r := <-done:
		return r.b, r.err
	case <-time.After(30 * time.Second):
		t.Fatal("Marshal did not terminate within 30s (termination defect)")
		return nil, nil
	}
}

// The generator produces sharing and cycles under the
// corresponding axes, terminates, and honors the dimensional parameters
// (depth bounds the walk, fan-out bounds children).
func TestTopoGenShapes(t *testing.T) {
	sawDup, sawCycle := false, false
	for seed := int64(1); seed <= 30 && !(sawDup && sawCycle); seed++ {
		g := &topoGen{r: rand.New(rand.NewSource(seed)), maxDepth: 4, maxFan: 3,
			reusePct: 20, cyclePct: 20}
		v := g.value(0)
		if v == nil {
			continue
		}
		_, dups := topoPaths(v)
		for _, p := range dups {
			sawDup = true
			// A cycle is a repeated encounter below its first encounter:
			// the first path is a strict prefix of the second.
			if len(p[1]) > len(p[0]) {
				prefix := true
				for i := range p[0] {
					if p[0][i] != p[1][i] {
						prefix = false
						break
					}
				}
				sawCycle = sawCycle || prefix
			}
		}
	}
	if !sawDup {
		t.Fatal("generator produced no sharing in 30 seeds (fan-in axis broken)")
	}
	if !sawCycle {
		t.Fatal("generator produced no back-edge closure in 30 seeds (cycle axis broken)")
	}
}

// The dimensional axis: deeper and wider graphs terminate and produce
// proportionally more first-encounter nodes (no fixed ceiling inside the
// generator). Sharing axes are off: every level adds fresh containers.
func TestTopoGenDimensions(t *testing.T) {
	for _, tc := range []struct{ depth, fan int }{
		{2, 2}, {4, 3}, {8, 2}, {14, 1}, {20, 1}, {3, 8},
	} {
		g := &topoGen{r: rand.New(rand.NewSource(int64(tc.depth*100 + tc.fan))),
			maxDepth: tc.depth, maxFan: tc.fan}
		v := g.value(0)
		first, _ := topoPaths(v)
		// A tree-shaped lower bound: at least depth nodes when fan ≥ 1
		// (each level contributes at least one fresh container).
		if len(first) < tc.depth {
			t.Fatalf("depth=%d fan=%d: only %d first-encounter nodes — dimensional axis collapsed",
				tc.depth, tc.fan, len(first))
		}
	}
}

// Mutual axis: a directed construction of mutual
// container pairs — A["b"]=B, B["a"]=A — not a random back-edge. The
// pair count is a table parameter; RT preserves the mutual identity.
func TestTopoMutualAxis(t *testing.T) {
	for _, pairs := range []int{1, 2, 3, 5} {
		root := make([]any, pairs)
		for i := range pairs {
			a, b := map[string]any{}, map[string]any{}
			a["b"] = b
			b["a"] = a
			a["n"] = int64(i)
			root[i] = a
		}
		data, err := marshalGuarded(t, root)
		if err != nil {
			t.Fatalf("pairs=%d: Marshal: %v", pairs, err)
		}
		dec := gbon.NewDecoder(bytes.NewReader(data))
		if err := dec.Register([]any{}, map[string]any{}, int64(0)); err != nil {
			t.Fatal(err)
		}
		var out any
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("pairs=%d: Decode: %v", pairs, err)
		}
		got := out.([]any)
		for i := range pairs {
			a := got[i].(map[string]any)
			b := a["b"].(map[string]any)
			if back, ok := b["a"].(map[string]any); !ok || back["b"] == nil {
				t.Fatalf("pairs=%d i=%d: mutual cycle broken", pairs, i)
			}
			b["witness"] = int64(i)
			if a["b"].(map[string]any)["witness"] != int64(i) {
				t.Fatalf("pairs=%d i=%d: mutual identity broken", pairs, i)
			}
		}
	}
}

// fanTree builds a complete map fan of depth d and branching f: every
// internal node is a fresh map with f children (sharing off).
func fanTree(d, f int, depth int) any {
	if depth >= d {
		return int64(depth)
	}
	m := map[string]any{}
	for i := range f {
		m[fmt.Sprintf("k%d", i)] = fanTree(d, f, depth+1)
	}
	return m
}

// Fan-out axis: node count grows at least as fan^depth
// — the fan is a table parameter, not a constant; Marshal terminates.
// Container levels 0..depth give (fan^(depth+1)−1)/(fan−1) ≥ fan^depth
// reference nodes (the leaf level below carries values, not records).
func TestTopoFanOutAxis(t *testing.T) {
	for _, tc := range []struct{ fan, depth int }{
		{3, 3}, {4, 3}, {8, 2}, {5, 2},
	} {
		v := fanTree(tc.depth+1, tc.fan, 0)
		first, _ := topoPaths(v)
		lower := 1
		for i := 0; i < tc.depth; i++ {
			lower *= tc.fan
		}
		if len(first) < lower {
			t.Fatalf("fan=%d depth=%d: %d nodes, want ≥ fan^depth = %d",
				tc.fan, tc.depth, len(first), lower)
		}
		if _, err := marshalGuarded(t, v); err != nil {
			t.Fatalf("fan=%d depth=%d: Marshal: %v", tc.fan, tc.depth, err)
		}
	}
}

// Sharing topologies over backing-window geometry (T0–T3), the scan-
// partition constraint set: T0 disjoint windows (one record per window,
// kafka-style), T1 exact duplicates (same origin+len at several
// positions), T2 chained overlapping windows over one backing (fan),
// T3 mixed (chain + seam + duplicate + blob pair + zerobase). The
// builders mirror the internal property fixtures; this side checks the
// public contract end to end: marshal, decode, and window geometry.
func topoT0Value(r *rand.Rand) any {
	n := 2 + r.Intn(6)
	vs := make([][]int64, n)
	for i := range vs {
		vs[i] = make([]int64, 1+r.Intn(5))
		for j := range vs[i] {
			vs[i][j] = int64(r.Int63())
		}
	}
	return vs
}

func topoT1Value(r *rand.Rand) any {
	base := make([]int64, 6+r.Intn(6))
	for j := range base {
		base[j] = int64(r.Int63())
	}
	v := base[1:3:5]
	n := 2 + r.Intn(4)
	vs := make([][]int64, n)
	for i := range vs {
		vs[i] = v
	}
	return vs
}

func topoT2Value(r *rand.Rand) any {
	n := 3 + r.Intn(6)
	w := 4 + r.Intn(5)
	step := 1 + r.Intn(w)
	base := make([]int64, (n-1)*step+w)
	for j := range base {
		base[j] = int64(r.Int63())
	}
	vs := make([][]int64, n)
	for i := range vs {
		off := i * step
		vs[i] = base[off : off+w : off+w]
	}
	return vs
}

type topoMix struct {
	A [][]int64
	B [][]byte
}

func topoT3Value(r *rand.Rand) any {
	a := make([]int64, 12)
	for j := range a {
		a[j] = int64(r.Int63())
	}
	b := []byte{0xAB, 0, 0xCD, 0, 0xEF}
	return topoMix{
		A: [][]int64{a[0:5:5], a[3:8:8], a[8:12:12], a[3:8:8], {int64(r.Int63())}},
		B: [][]byte{b[0:4:4], b[2:5:5], {}},
	}
}

// The sharing topologies round-trip with their window geometry intact:
// element data, lengths, and capacities of every member window survive
// the record/view encoding of the grouping pre-pass.
func TestTopoSharingTopologies(t *testing.T) {
	for _, topo := range []struct {
		name  string
		build func(r *rand.Rand) any
	}{
		{"T0", topoT0Value}, {"T1", topoT1Value}, {"T2", topoT2Value}, {"T3", topoT3Value},
	} {
		for seed := int64(1); seed <= 10; seed++ {
			v := topo.build(rand.New(rand.NewSource(seed)))
			data, err := marshalGuarded(t, v)
			if err != nil {
				t.Fatalf("%s seed=%d: Marshal: %v", topo.name, seed, err)
			}
			dec := gbon.NewDecoder(bytes.NewReader(data))
			if err := dec.Register([][]int64{}, []byte{}, topoMix{}); err != nil {
				t.Fatal(err)
			}
			var out any
			if err := dec.Decode(&out); err != nil {
				t.Fatalf("%s seed=%d: Decode: %v", topo.name, seed, err)
			}
			assertWindowsEqual(t, v, out, topo.name, seed)
		}
	}
}

// assertWindowsEqual compares two window trees element-wise, requiring
// equal lengths and capacities on every slice leaf (the observable
// geometry of the view tokens).
func assertWindowsEqual(t *testing.T, want, got any, name string, seed int64) {
	t.Helper()
	w, g := reflect.ValueOf(want), reflect.ValueOf(got)
	if w.Type() != g.Type() {
		t.Fatalf("%s seed=%d: type %v, want %v", name, seed, g.Type(), w.Type())
	}
	switch w.Kind() {
	case reflect.Slice:
		if w.Len() != g.Len() || w.Cap() != g.Cap() {
			t.Fatalf("%s seed=%d: window len/cap (%d/%d), want (%d/%d)",
				name, seed, g.Len(), g.Cap(), w.Len(), w.Cap())
		}
		for i := 0; i < w.Len(); i++ {
			assertWindowsEqual(t, w.Index(i).Interface(), g.Index(i).Interface(), name, seed)
		}
	case reflect.Struct:
		for i := 0; i < w.NumField(); i++ {
			assertWindowsEqual(t, w.Field(i).Interface(), g.Field(i).Interface(), name, seed)
		}
	default:
		if !reflect.DeepEqual(w.Interface(), g.Interface()) {
			t.Fatalf("%s seed=%d: leaf %v, want %v", name, seed, g.Interface(), w.Interface())
		}
	}
}

// S-curve axis: the window count s over one backing
// is the parameter; output bytes are monotone in s and linear in the
// number of unique nodes (constant per-node bound, mirroring the DAG
// linearity carrier).
func TestTopoSCurveAxis(t *testing.T) {
	const cPerNode = 40
	const overhead = 128
	prev := -1
	for _, s := range []int{2, 4, 6, 8, 12, 16, 20, 24} {
		base := make([]int64, 2*s+2)
		for i := range base {
			base[i] = int64(i)
		}
		wins := make([][]int64, s)
		for i := range wins {
			wins[i] = base[2*i : 2*i+4 : 2*i+4]
		}
		data, err := gbon.Marshal(fanWin{W: wins})
		if err != nil {
			t.Fatalf("s=%d: Marshal: %v", s, err)
		}
		if len(data) <= prev {
			t.Fatalf("s=%d: %d bytes not monotone (prev %d)", s, len(data), prev)
		}
		if len(data) > overhead+cPerNode*s {
			t.Fatalf("s=%d: %d bytes exceed linearity bound %d", s, len(data), overhead+cPerNode*s)
		}
		var got fanWin
		if err := gbon.Unmarshal(data, &got); err != nil {
			t.Fatalf("s=%d: Unmarshal: %v", s, err)
		}
		if len(got.W) != s || len(got.W[s-1]) != 4 || cap(got.W[s-1]) != 4 {
			t.Fatalf("s=%d: window geometry lost", s)
		}
		prev = len(data)
	}
}
