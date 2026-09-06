package gbon

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// Component oracle for the scan phase: over the live (non-tombstoned)
// entries of every class array, live spans stay pairwise disjoint and
// ordered by their live origins; tombstones may interleave and keep the
// creation-key array order.
func assertEnvelopesDisjoint(t *testing.T, e *codecEncoder) {
	t.Helper()
	for c, ix := range e.classIx {
		prev := (*envComp)(nil)
		for i := range ix.envs {
			ent := ix.envs[i]
			if ent.dead || e.arena.comps[ent.ci].dead() {
				continue
			}
			cur := &e.arena.comps[ent.ci]
			if prev != nil && (prev.origin > cur.origin || prev.end > cur.origin) {
				t.Fatalf("class es=%d blob=%v: live envelope at %d unordered or overlapping: [%d,%d) before [%d,%d)",
					c.es, c.blob, i, prev.origin, prev.end, cur.origin, cur.end)
			}
			prev = cur
		}
	}
}

// Continuum test for the grouping pre-pass: the member windows of an
// envelope component must join into a hole-free cover of [origin, end) —
// overlap inside one component is valid aliasing — while distinct live
// components never overlap each other. Crafted slot set: a shared backing
// with chained overlapping windows, plus a separate allocation. Windows
// must strictly intersect to join — a mere seam (ptr == member end)
// stays a separate component, so the crafted chain overlaps pairwise.
func TestBackingGroupContinuum(t *testing.T) {
	backing := make([]int64, 16)
	head := backing[0:8:8]   // [0, 64)
	mid := backing[4:12:12]  // [32, 96): overlaps head and tail
	tail := backing[8:16:16] // [64, 128): overlaps mid
	apart := make([]int64, 4)

	enc := NewEncoder(&bytes.Buffer{})
	for _, s := range [][]int64{head, mid, tail, apart} {
		if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
			t.Fatal(err)
		}
	}

	type span struct{ lo, hi uintptr }
	a := &enc.enc.arena
	comps := make([]*envComp, 0, 2)
	for ci := range a.comps {
		if !a.comps[ci].absorbed {
			comps = append(comps, &a.comps[ci])
		}
	}
	if len(comps) != 2 {
		t.Fatalf("crafted set must yield the shared component plus the separate one: got %d", len(comps))
	}
	for gi, c := range comps {
		windows := make([]span, 0, 1)
		for m := c.head; m >= 0; m = a.sNext[m] {
			windows = append(windows, span{a.sPtr[m], a.sPtr[m] + uintptr(a.sVal[m].Cap())*c.es})
		}
		sort.Slice(windows, func(i, j int) bool { return windows[i].lo < windows[j].lo })
		lo, hi := windows[0].lo, windows[0].hi
		for _, w := range windows[1:] {
			if w.lo > hi {
				t.Fatalf("component %d: hole in the window cover at offset %d..%d", gi, hi, w.lo)
			}
			if w.hi > hi {
				hi = w.hi
			}
		}
		if lo != c.origin || hi != c.end {
			t.Fatalf("component %d: cover [%d,%d) must equal the recorded region [origin=%d,end=%d)", gi, lo, hi, c.origin, c.end)
		}
	}
	for i := 0; i < len(comps); i++ {
		for j := i + 1; j < len(comps); j++ {
			x, y := comps[i], comps[j]
			if x.origin < y.end && y.origin < x.end {
				t.Fatalf("live components overlap: [%d,%d) ∩ [%d,%d) ≠ ∅", x.origin, x.end, y.origin, y.end)
			}
		}
	}
	assertEnvelopesDisjoint(t, enc.enc)
}

// Closed-region slot reuse: an emitted (closed) component's region may
// sit between live envelopes — the envelope overlap search must be
// insensitive to it, and a fresh window over the closed region joins
// only its covering live neighbor. Crafted shape (CL-15): live span
// overhanging a closed region on the left, then a slot covered by that
// span.
func TestFreshIndexDeadRegionCandidates(t *testing.T) {
	backing := make([]byte, 256)
	d := backing[64:96:96]    // becomes a closed (emitted) component [64,96)
	x := backing[160:224:224] // live component [160,224)
	enc := NewEncoder(&bytes.Buffer{})
	for _, s := range [][]byte{d, x} {
		if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
			t.Fatal(err)
		}
	}
	enc.enc.markEmitted(enc.enc.groupOf(reflect.ValueOf(d), false))
	e1 := backing[32:128:128] // reuse over the closed region, overhanging left
	if err := enc.enc.scanAddSlot(reflect.ValueOf(e1), false); err != nil {
		t.Fatal(err)
	}
	s := backing[112:120:120] // covered by e1; an end-based search skips it
	if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
		t.Fatal(err)
	}
	want := enc.enc.groupOf(reflect.ValueOf(e1), false)
	if want < 0 {
		t.Fatal("e1 must resolve to a fresh component")
	}
	got := enc.enc.groupOf(reflect.ValueOf(s), false)
	if got != want {
		var o, e uintptr
		if got >= 0 {
			o, e = enc.enc.arena.comps[got].origin, enc.enc.arena.comps[got].end
		}
		t.Fatalf("slot [112,120) must join the covering component [origin=%d,end=%d), got [origin=%d,end=%d)",
			enc.enc.arena.comps[want].origin, enc.enc.arena.comps[want].end, o, e)
	}
	assertEnvelopesDisjoint(t, enc.enc)
}

// winGeom is the cap-window geometry of one slot occurrence: the half-open
// element-address span [ptr, send) over which grouping join is decided.
type winGeom struct {
	ptr, send uintptr
	es        uintptr
	blob      bool
}

// geomOf extracts the window geometry of a non-nil slice value.
func geomOf(v reflect.Value) winGeom {
	es := v.Type().Elem().Size()
	p := v.Pointer()
	return winGeom{ptr: p, send: p + uintptr(v.Cap())*es, es: es, blob: es == 1 && v.Type().Elem().Kind() == reflect.Uint8}
}

// intersects is the pairwise join predicate of the grouping geometry:
// strictly overlapping windows join; a seam (one window starting exactly
// at the other's end) stays separate; equal pointers join through the
// empty-window special case.
func (a winGeom) intersects(b winGeom) bool {
	return a.ptr < b.send && b.ptr < a.send
}

// naiveComponents computes the connectivity components of the windows
// by pairwise geometry alone (O(n²), no codec state): the independent
// oracle for the scan partition. Windows of different classes never
// join. Beside symmetric strict intersection, the directional
// equal-origin rule mirrors the arm of the linear first-fit: an empty
// x[k:k:k] window joins the subsequent non-empty window starting at the
// same origin (strict intersection cannot see the empty span); the
// reverse order does not join.
func naiveComponents(wins []winGeom) [][]winGeom {
	empty := func(w winGeom) bool { return w.send == w.ptr }
	joins := func(i, j int) bool {
		if wins[i].intersects(wins[j]) {
			return true
		}
		return empty(wins[i]) && !empty(wins[j]) && wins[i].ptr == wins[j].ptr && i < j
	}
	used := make([]bool, len(wins))
	var comps [][]winGeom
	for i := range wins {
		if used[i] {
			continue
		}
		idx := []int{i}
		used[i] = true
		for grew := true; grew; {
			grew = false
			for j := range wins {
				if used[j] {
					continue
				}
				if wins[j].es != wins[idx[0]].es || wins[j].blob != wins[idx[0]].blob {
					continue
				}
				for _, m := range idx {
					if joins(m, j) || joins(j, m) {
						idx = append(idx, j)
						used[j] = true
						grew = true
						break
					}
				}
			}
		}
		comp := make([]winGeom, len(idx))
		for k, id := range idx {
			comp[k] = wins[id]
		}
		comps = append(comps, comp)
	}
	return comps
}

// compKey renders one component canonically: the envelope span plus the
// sorted member window multiset, so partitions compare as sets.
func compKey(wins []winGeom) string {
	lo, hi := wins[0].ptr, wins[0].send
	for _, w := range wins[1:] {
		if w.ptr < lo {
			lo = w.ptr
		}
		if w.send > hi {
			hi = w.send
		}
	}
	spans := make([]string, len(wins))
	for i, w := range wins {
		spans[i] = fmt.Sprintf("%x-%x", w.ptr, w.send)
	}
	sort.Strings(spans)
	return fmt.Sprintf("es=%d blob=%v [%x,%x) %v", wins[0].es, wins[0].blob, lo, hi, spans)
}

// groupViewsOf projects the live encoder grouping into component views:
// one view per live (non-absorbed) component with its member window
// geometries. The projection is the mechanic-facing half of the
// partition oracle; the naive componentor is the independent half.
func groupViewsOf(e *codecEncoder) [][]winGeom {
	a := &e.arena
	var out [][]winGeom
	for ci := range a.comps {
		c := &a.comps[ci]
		if c.absorbed {
			continue
		}
		var ws []winGeom
		for m := c.head; m >= 0; m = a.sNext[m] {
			ws = append(ws, winGeom{ptr: a.sPtr[m], send: a.sPtr[m] + uintptr(a.sVal[m].Cap())*c.es, es: c.es, blob: c.blob})
		}
		out = append(out, ws)
	}
	return out
}

// assertScanPartition checks the scan-phase partition property: every
// group of the encoder equals one connectivity component of the window
// geometry, and the component sets are equal.
func assertScanPartition(t *testing.T, e *codecEncoder, wins []winGeom, ctx string) {
	t.Helper()
	if len(wins) == 0 {
		t.Fatalf("%s: topology produced no windows", ctx)
	}
	got := make(map[string]int)
	for _, gv := range groupViewsOf(e) {
		got[compKey(gv)]++
	}
	want := make(map[string]int)
	for _, comp := range naiveComponents(wins) {
		want[compKey(comp)]++
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %d groups, naive components %d", ctx, len(got), len(want))
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("%s: partition mismatch: missing/dup component %s (got %d, want %d)", ctx, k, got[k], n)
		}
	}
}

// topoScan runs the scan stage of one root value on a live encoder and
// returns it for partition inspection.
func topoScan(t *testing.T, v any) *codecEncoder {
	t.Helper()
	enc := NewEncoder(&bytes.Buffer{})
	enc.enc.gen++
	if err := enc.enc.scanValue(reflect.ValueOf(v)); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return enc.enc
}

// TestScanPartitionProperty: over the sharing topologies T0–T3 the scan
// partition (groups with envelopes and members) equals the connectivity
// components of the window geometry computed by the independent naive
// componentor.
func TestScanPartitionProperty(t *testing.T) {
	for _, topo := range []struct {
		name  string
		build func(r *rand.Rand) (any, []winGeom)
	}{
		{"T0", buildTopoT0}, {"T1", buildTopoT1}, {"T2", buildTopoT2}, {"T3", buildTopoT3},
	} {
		for seed := int64(1); seed <= 25; seed++ {
			v, wins := topo.build(rand.New(rand.NewSource(seed)))
			e := topoScan(t, v)
			assertScanPartition(t, e, wins, fmt.Sprintf("%s seed=%d", topo.name, seed))
		}
	}
}

// buildTopoT0: disjoint windows — separate allocations, kafka-style: one
// window per record, zero sharing between members. The root container
// slice is itself a slot of its own class (element-size 24).
func buildTopoT0(r *rand.Rand) (any, []winGeom) {
	n := 2 + r.Intn(6)
	vs := make([][]int64, n)
	wins := make([]winGeom, 0, n+1)
	for i := range vs {
		vs[i] = make([]int64, 1+r.Intn(5))
		for j := range vs[i] {
			vs[i][j] = int64(r.Int63())
		}
		wins = append(wins, geomOf(reflect.ValueOf(vs[i])))
	}
	return vs, append(wins, geomOf(reflect.ValueOf(vs)))
}

// buildTopoT1: exact duplicates — one window value appearing at several
// positions (origin, len, cap all equal).
func buildTopoT1(r *rand.Rand) (any, []winGeom) {
	base := make([]int64, 4+r.Intn(8))
	for j := range base {
		base[j] = int64(r.Int63())
	}
	lo := r.Intn(2)
	mid := lo + 1 + r.Intn(len(base)-lo-2)
	hi := mid + 1 + r.Intn(len(base)-mid)
	v := base[lo:mid:hi]
	n := 2 + r.Intn(4)
	vs := make([][]int64, n)
	wins := make([]winGeom, 0, n+1)
	g := geomOf(reflect.ValueOf(v))
	for i := range vs {
		vs[i] = v
		wins = append(wins, g)
	}
	return vs, append(wins, geomOf(reflect.ValueOf(vs)))
}

// buildTopoT2: chained overlapping windows over one backing, fan-style:
// consecutive windows overlap by construction; the whole chain is one
// component.
func buildTopoT2(r *rand.Rand) (any, []winGeom) {
	n := 3 + r.Intn(6)
	w := 4 + r.Intn(5) // window cap span
	step := 1 + r.Intn(w)
	total := (n-1)*step + w
	base := make([]int64, total)
	for j := range base {
		base[j] = int64(r.Int63())
	}
	vs := make([][]int64, n)
	wins := make([]winGeom, 0, n+1)
	for i := range vs {
		off := i * step
		vs[i] = base[off : off+w : off+w]
		wins = append(wins, geomOf(reflect.ValueOf(vs[i])))
	}
	return vs, append(wins, geomOf(reflect.ValueOf(vs)))
}

// buildTopoT3: mixed — a merge chain with a duplicate window and a seam
// split over one int64 backing, a separate int64 allocation, and a blob
// window pair over one byte backing plus a zerobase window.
func buildTopoT3(r *rand.Rand) (any, []winGeom) {
	type mixT struct {
		A [][]int64
		B [][]byte
	}
	la := 10 + r.Intn(6)
	a := make([]int64, la)
	for j := range a {
		a[j] = int64(r.Int63())
	}
	lb := 6 + r.Intn(5)
	b := make([]byte, lb)
	for j := range b {
		b[j] = byte(r.Intn(256))
	}
	// chain windows overlap pairwise; the seam window starts exactly at
	// the chain's end and stays separate.
	mid := 3 + r.Intn(3)
	c0 := a[0 : mid+2 : mid+2]
	c1 := a[mid : mid+4 : mid+4]
	seamEnd := min(mid+5+r.Intn(3), la-1)
	seam := a[mid+4 : seamEnd : seamEnd]
	solo := []int64{int64(r.Int63())}
	// Isolated equal-origin pair after the seam: the zero-cap reslice
	// hole window (keeps the data pointer, unlike a fresh x[k:k:k]
	// expression) creates an empty envelope, and the following non-empty
	// window at the same origin joins it through the equal-origin arm.
	over := a[seamEnd:la:la]
	hole := over[:0:0]
	w0End := 2 + r.Intn(2)
	w0 := b[0:w0End:w0End]
	w1End := min(3+r.Intn(2), len(b))
	w1 := b[1:w1End:w1End]
	v := mixT{
		A: [][]int64{c0, c1, c0, seam, hole, over, solo},
		B: [][]byte{w0, w1, {}},
	}
	var wins []winGeom
	for _, s := range v.A {
		wins = append(wins, geomOf(reflect.ValueOf(s)))
	}
	for _, s := range v.B {
		wins = append(wins, geomOf(reflect.ValueOf(s)))
	}
	wins = append(wins,
		geomOf(reflect.ValueOf(v.A)),
		geomOf(reflect.ValueOf(v.B)))
	return v, wins
}

// Equal-origin join (the arm of the linear first-fit): a zero-cap
// reslice window (w[:0:0] — keeps the data pointer of w, unlike a
// fresh x[k:k:k] expression whose pointer the compiler may sanitize)
// forms an empty envelope; a subsequent non-empty window starting at the
// same origin joins it — strict intersection alone cannot see the empty
// span. The reverse order does not join: the empty window arriving
// after a non-empty envelope at the same origin stays its own component
// (the forward scan breaks at origin >= send).
func TestEmptyEnvelopeEqualOriginJoin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order func(backing []int64) [][]int64
		want  int
	}{
		{"empty-first joins", func(b []int64) [][]int64 {
			full := b[0:2:2]
			return [][]int64{full[:0:0], full}
		}, 1},
		{"nonempty-first stays apart", func(b []int64) [][]int64 {
			full := b[0:2:2]
			return [][]int64{full, full[:0:0]}
		}, 2},
	} {
		backing := make([]int64, 8)
		wins := tc.order(backing)
		enc := NewEncoder(&bytes.Buffer{})
		for _, w := range wins {
			if err := enc.enc.scanAddSlot(reflect.ValueOf(w), false); err != nil {
				t.Fatal(err)
			}
		}
		live := 0
		for ci := range enc.enc.arena.comps {
			if !enc.enc.arena.comps[ci].absorbed {
				live++
			}
		}
		if live != tc.want {
			t.Fatalf("%s: live components = %d, want %d", tc.name, live, tc.want)
		}
		assertEnvelopesDisjoint(t, enc.enc)
	}
}

// Representability guard pair (carve-out): a slot over
// an emitted component whose len-window reaches the record's implicit
// zero tail [E, L) joins the closed record only when every tail element
// is bitwise zero in live memory; one live nonzero byte forces the fresh
// fallback (the sole body-duplication carve-out).
func TestRepresentableGuardJoin(t *testing.T) {
	backing := make([]byte, 8)
	w := backing[0:8:8]
	enc := NewEncoder(&bytes.Buffer{})
	if err := enc.enc.scanAddSlot(reflect.ValueOf(w), false); err != nil {
		t.Fatal(err)
	}
	ci := enc.enc.groupOf(reflect.ValueOf(w), false)
	if ci < 0 {
		t.Fatal("w must resolve")
	}
	// close the record: L=8, E=4 (tail [4,8) implicit zero)
	c := &enc.enc.arena.comps[ci]
	c.emitted = true
	c.id = 1
	c.elided = 4
	enc.enc.markEmitted(ci)

	// Live nonzero byte inside the zero tail → guard rejects → fresh.
	backing[6] = 0xAA
	mut := backing[4:8:8]
	if err := enc.enc.scanAddSlot(reflect.ValueOf(mut), false); err != nil {
		t.Fatal(err)
	}
	fresh := enc.enc.groupOf(reflect.ValueOf(mut), false)
	if fresh < 0 || fresh == ci {
		t.Fatal("guard-rejected window must fall back to its own fresh component")
	}
	if fc := &enc.enc.arena.comps[fresh]; fc.origin != reflect.ValueOf(mut).Pointer() || fc.end != reflect.ValueOf(mut).Pointer()+4 {
		t.Fatalf("fresh fallback must span the rejected window, got [%d,%d)", fc.origin, fc.end)
	}

	// Pair: tail bitwise zero → representable accept → view over emitted.
	backing[6] = 0
	zero := backing[5:8:8]
	if err := enc.enc.scanAddSlot(reflect.ValueOf(zero), false); err != nil {
		t.Fatal(err)
	}
	if got := enc.enc.groupOf(reflect.ValueOf(zero), false); got != ci {
		t.Fatal("bit-zero tail window must join the emitted component")
	}
	assertEnvelopesDisjoint(t, enc.enc)
}

// Bridge across several envelopes with a merge over an absorbed span: a
// spanning slot coalesces every intersecting component into the lowest-seq
// survivor; subsequent windows over the absorbed region resolve to the survivor.
func TestBridgeMultipleEnvelopes(t *testing.T) {
	backing := make([]byte, 256)
	a := backing[0:32:32]
	b := backing[64:96:96]
	cw := backing[128:160:160]
	enc := NewEncoder(&bytes.Buffer{})
	for _, s := range [][]byte{a, b, cw} {
		if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
			t.Fatal(err)
		}
	}
	s := backing[0:160:160] // bridges a, b, cw in one run
	if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
		t.Fatal(err)
	}
	live := 0
	for ci := range enc.enc.arena.comps {
		if !enc.enc.arena.comps[ci].absorbed {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("bridge run must coalesce three components into one, live=%d", live)
	}
	into := enc.enc.groupOf(reflect.ValueOf(a), false)
	tc := &enc.enc.arena.comps[into]
	wantEnd := reflect.ValueOf(a).Pointer() + 160
	if tc.origin != reflect.ValueOf(a).Pointer() || tc.end != wantEnd {
		t.Fatalf("coalesced component must span [0,160), got [%d,%d)", tc.origin, tc.end)
	}
	// windows over the absorbed spans resolve to the survivor
	for _, probe := range [][]byte{backing[70:90:90], backing[130:150:150]} {
		if err := enc.enc.scanAddSlot(reflect.ValueOf(probe), false); err != nil {
			t.Fatal(err)
		}
		if got := enc.enc.groupOf(reflect.ValueOf(probe), false); got != into {
			t.Fatal("probe over an absorbed span must resolve to the surviving component")
		}
	}
	assertEnvelopesDisjoint(t, enc.enc)
}

// Bridge merge across a closed component: the surviving component's
// origin moves leftward past a closed component's region while the
// envelope order must hold on live origins. Crafted shape (CL-16):
// merge target created first (lowest seq), closed component in the
// reused region between the candidates, bridge slot spanning both.
func TestFreshIndexBridgeOverDead(t *testing.T) {
	backing := make([]byte, 256)
	into := backing[160:200:200] // created first: the merge target by seq
	d2 := backing[100:140:140]   // closed component between g and into
	g := backing[40:70:70]       // left candidate, merged over the closed span
	enc := NewEncoder(&bytes.Buffer{})
	for _, s := range [][]byte{into, d2, g} {
		if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
			t.Fatal(err)
		}
	}
	enc.enc.markEmitted(enc.enc.groupOf(reflect.ValueOf(d2), false))
	s := backing[50:180:180] // bridges g and into across the closed span
	if err := enc.enc.scanAddSlot(reflect.ValueOf(s), false); err != nil {
		t.Fatal(err)
	}
	target := enc.enc.groupOf(reflect.ValueOf(into), false)
	if target < 0 {
		t.Fatal("into must stay resolved after the bridge")
	}
	tc := &enc.enc.arena.comps[target]
	gp := reflect.ValueOf(g).Pointer()
	intoEnd := reflect.ValueOf(into).Pointer() + 40
	if tc.origin != gp || tc.end != intoEnd {
		t.Fatalf("bridged component must span the merged region [origin=%d,end=%d), got [origin=%d,end=%d)", gp, intoEnd, tc.origin, tc.end)
	}
	for _, probe := range [][]byte{backing[80:120:120], backing[44:48:48]} {
		if err := enc.enc.scanAddSlot(reflect.ValueOf(probe), false); err != nil {
			t.Fatal(err)
		}
		if got := enc.enc.groupOf(reflect.ValueOf(probe), false); got != target {
			t.Fatal("probe over the merged span must join the bridged component")
		}
	}
	assertEnvelopesDisjoint(t, enc.enc)
}
