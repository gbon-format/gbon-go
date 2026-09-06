package gbon_test

import (
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// TestEncodeScalingProbe gates the encode work growth class: N
// independent small backings, each encoded twice (fresh record, then
// representable repeat) through one stream, must encode in time that
// grows near-linearly — t(2N)/t(N) inside [1.5, 3.0]. The repeat slots
// make the emitted set grow to N while the output stays one small
// record per backing, isolating the per-slot host search from the
// record-density work. The band is wide at the bottom (Docker timing
// jitter) and at the top (N·log N doubling sits at ~2.1-2.2, a linear
// emitted scan at ~4). Each size takes the median of three single
// passes; one out-of-band result earns a single full retry before
// failing.
func TestEncodeScalingProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if raceEnabled {
		t.Skip("race build: wall-clock timings distorted")
	}
	encodeChain := func(n int) time.Duration {
		var passes []time.Duration
		for range 3 {
			backings := make([][]int64, n)
			for k := range backings {
				backings[k] = []int64{1, 0, 0, 0}
			}
			e := gbon.NewEncoder(io.Discard)
			start := time.Now()
			for k := range n {
				if err := e.Encode(backings[k]); err != nil {
					t.Fatalf("encode %d: %v", k, err)
				}
				if err := e.Encode(backings[k]); err != nil {
					t.Fatalf("re-encode %d: %v", k, err)
				}
				if err := e.Encode(backings[k]); err != nil {
					t.Fatalf("cached re-encode %d: %v", k, err)
				}
			}
			passes = append(passes, time.Since(start))
		}
		slices.Sort(passes)
		return passes[1]
	}
	probe := func() (float64, time.Duration, time.Duration) {
		t1 := encodeChain(1024)
		t2 := encodeChain(2048)
		return float64(t2) / float64(t1), t1, t2
	}
	ratio, t1, t2 := probe()
	if ratio < 1.5 || ratio > 3.0 {
		ratio, t1, t2 = probe()
		if ratio < 1.5 || ratio > 3.0 {
			t.Fatalf("scaling probe: t(2N)/t(N) = %.3f outside [1.5, 3.0] (t1=%v t2=%v)", ratio, t1, t2)
		}
	}
	t.Logf("scaling probe: t(N=1024)=%v t(2N=2048)=%v ratio=%.3f", t1, t2, ratio)
}
