package gbon

import (
	"testing"
)

// Chain model for the work-counter bound: a persistent snapshot chain
// over one shared backing. Snapshot k is the window backing[0:k+1:k+2]
// (len and cap both grow to the right), the single non-zero element
// sits at index 0, and every 8th step a fixed reslice window over a
// separate far zone of the same backing repeats (exact slotKey misses,
// cache hits after the first visit). Snapshots are never representable
// against earlier snapshots (the window always grows past every frozen
// end), so each emits a fresh component: the emitted set grows with k
// while output nodes stay one per snapshot.
func runChainModel(t *testing.T, K int) *codecEncoder {
	t.Helper()
	backing := make([]int64, K+16)
	backing[0] = 1
	e := newCodecEncoder()
	if err := e.w.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	e.started = true
	e.streamStart = len(e.w.Bytes())
	for k := range K {
		w := backing[0 : k+1 : k+2]
		if err := e.Encode(w); err != nil {
			t.Fatalf("encode snapshot %d: %v", k, err)
		}
		if k%8 == 7 {
			r := backing[K+8 : K+12 : K+12]
			if err := e.Encode(r); err != nil {
				t.Fatalf("encode reslice %d: %v", k, err)
			}
		}
	}
	return e
}

// The per-stream search work (structural slot probes plus representable
// element iterations) is bounded by a constant multiple of stream size:
// W <= 64*(nodes + slots), and doubling the chain scales W by at most
// 2.5x.
func TestEncodeWorkCounterChainBound(t *testing.T) {
	for _, K := range []int{2048, 4096} {
		e := runChainModel(t, K)
		W := e.workProbes + e.workElems
		units := e.nodes + len(e.arena.sVal)
		t.Logf("K=%d W=%d (probes=%d elems=%d) nodes=%d slots=%d units=%d W/units=%.1f hostCache: hits=%d misses=%d rate=%.1f%%",
			K, W, e.workProbes, e.workElems, e.nodes, len(e.arena.sVal), units, float64(W)/float64(units),
			e.hostHits, e.hostMisses, 100*float64(e.hostHits)/float64(e.hostHits+e.hostMisses))
		if W > 64*int64(units) {
			t.Errorf("K=%d: work %d exceeds 64*(nodes+slots)=%d", K, W, 64*int64(units))
		}
	}
	e1 := runChainModel(t, 2048)
	e2 := runChainModel(t, 4096)
	w1 := e1.workProbes + e1.workElems
	w2 := e2.workProbes + e2.workElems
	ratio := float64(w2) / float64(w1)
	t.Logf("work doubling ratio W(2K)/W(K) = %.3f", ratio)
	if ratio > 2.5 {
		t.Errorf("work doubling ratio %.3f exceeds 2.5", ratio)
	}
}
