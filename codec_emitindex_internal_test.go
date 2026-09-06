package gbon

import (
	"math/rand"
	"slices"
	"testing"
)

// bruteForceQuery collects every skiplist node with origin <= p and
// end >= send — the containment set the skiplist must reproduce.
func bruteForceQuery(s *emitSkip, p, send uintptr) []int32 {
	var out []int32
	for i := 1; i < len(s.nodes); i++ {
		n := &s.nodes[i]
		if n.origin <= p && n.end >= send {
			out = append(out, n.ci)
		}
	}
	slices.Sort(out)
	return out
}

func querySorted(s *emitSkip, e *codecEncoder, p, send uintptr) []int32 {
	var out []int32
	s.emitQuery(e, p, send, &out)
	slices.Sort(out)
	return out
}

func mkComp(a *scanArena, origin, end uintptr) int32 {
	ci := int32(len(a.comps))
	a.comps = append(a.comps, envComp{origin: origin, end: end, es: 1, sortKey: origin})
	return ci
}

// Stabbing queries over adversarial span sets: monotone inserts,
// equal-origin spans, leftover stacks (subsequent spans nested over frozen
// ones), back-looking windows, and random shapes — the index must
// return exactly the containment set of each query.
func TestEmitSkipContainment(t *testing.T) {
	e := newCodecEncoder()
	rng := rand.New(rand.NewSource(7))
	for _, tc := range []string{"monotone", "equal-origin", "leftover-stack", "random"} {
		var a scanArena
		var s emitSkip
		switch tc {
		case "monotone":
			for i := range 300 {
				s.insert(&a, mkComp(&a, uintptr(10*i), uintptr(10*i+8)))
			}
		case "equal-origin":
			for i := range 200 {
				s.insert(&a, mkComp(&a, 1000, uintptr(1000+4*i)))
			}
			for i := range 100 {
				s.insert(&a, mkComp(&a, uintptr(5000+10*i), uintptr(5000+10*i+20)))
			}
		case "leftover-stack":
			// Frozen span, then subsequent spans nested inside it at both
			// ends and beyond, mirroring representable-overlap leftovers.
			s.insert(&a, mkComp(&a, 100, 500))
			for i := range 150 {
				s.insert(&a, mkComp(&a, uintptr(100+i), uintptr(500-i)))
			}
			s.insert(&a, mkComp(&a, 50, 600))
			s.insert(&a, mkComp(&a, 90, 510))
		case "random":
			for range 400 {
				o := uintptr(rng.Intn(2000))
				w := 1 + rng.Intn(300)
				s.insert(&a, mkComp(&a, o, o+uintptr(w)))
			}
		}
		for range 500 {
			p := uintptr(rng.Intn(2400))
			send := p + uintptr(rng.Intn(300))
			got := querySorted(&s, e, p, send)
			want := bruteForceQuery(&s, p, send)
			if len(got) != len(want) {
				t.Fatalf("%s: query [%d,%d): got %v want %v", tc, p, send, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s: query [%d,%d): got %v want %v", tc, p, send, got, want)
				}
			}
		}
	}
}

// The segment aggregates must reflect every insert: a query whose send
// separates two stacked spans must return only the covering one.
func TestEmitSkipMaxEndStack(t *testing.T) {
	var a scanArena
	var s emitSkip
	s.insert(&a, mkComp(&a, 0, 100))
	s.insert(&a, mkComp(&a, 0, 40))
	s.insert(&a, mkComp(&a, 10, 30))
	e := newCodecEncoder()
	if got := querySorted(&s, e, 20, 25); len(got) != 3 {
		t.Fatalf("stack containment: got %v want all three", got)
	}
	if got := querySorted(&s, e, 20, 90); len(got) != 1 {
		t.Fatalf("outer-only containment: got %v want the [0,100) span only", got)
	}
	if got := querySorted(&s, e, 200, 300); len(got) != 0 {
		t.Fatalf("empty range: got %v", got)
	}
	if got := querySorted(&s, e, 0, 100); len(got) != 1 {
		t.Fatalf("exact-span containment: got %v want the [0,100) span only", got)
	}
}

// Insert order independence: the same span set inserted in different
// orders answers queries identically (tower heights depend on the arena
// index, the containment set never does).
func TestEmitSkipOrderIndependence(t *testing.T) {
	spans := [][2]uintptr{{0, 100}, {50, 60}, {10, 90}, {90, 200}, {0, 5}, {150, 300}, {95, 105}}
	var orders [][]int32
	orders = append(orders, []int32{0, 1, 2, 3, 4, 5, 6})
	orders = append(orders, []int32{6, 5, 4, 3, 2, 1, 0})
	orders = append(orders, []int32{3, 0, 6, 1, 5, 2, 4})
	rng := rand.New(rand.NewSource(11))
	for range 20 {
		perm := append([]int32(nil), orders[0]...)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		orders = append(orders, perm)
	}
	e := newCodecEncoder()
	for oi, perm := range orders {
		var a scanArena
		var s emitSkip
		for _, idx := range perm {
			s.insert(&a, mkComp(&a, spans[idx][0], spans[idx][1]))
		}
		for p := uintptr(0); p < 320; p += 7 {
			for send := p; send < 320; send += 13 {
				got := querySorted(&s, e, p, send)
				want := bruteForceQuery(&s, p, send)
				if len(got) != len(want) {
					t.Fatalf("order %d query [%d,%d): got %v want %v", oi, p, send, got, want)
				}
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("order %d query [%d,%d): got %v want %v", oi, p, send, got, want)
					}
				}
			}
		}
	}
}
