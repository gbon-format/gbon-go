//go:build gbon_emitprobe

package gbon

import (
	"bytes"
	"math"
	"math/big"
	"math/rand"
	"runtime"
	"testing"
)

// soakValues builds sharing-heavy graphs whose member windows pin
// backing arrays through the arena: overlapping slice windows, blobs,
// primitive batches, string/pointer/big.Int members.
func soakValues(r *rand.Rand) []any {
	backing := make([]int32, 64)
	for i := range backing {
		backing[i] = r.Int31()
	}
	blobBase := make([]byte, 96)
	for i := range blobBase {
		blobBase[i] = byte(r.Intn(256))
	}
	strBase := []string{"alpha", "", "gamma", "", "omega", ""}
	type node struct {
		Val  int
		Name string
		Next []*node
	}
	shared := &node{Val: 7, Name: "shared"}
	other := &node{Val: 8, Name: "other", Next: []*node{shared, shared}}
	var vals []any
	vals = append(vals,
		[][]int32{backing[0:16], backing[8:32], backing[60:64]},
		[][]byte{blobBase[0:40], blobBase[32:96]},
		[]string(strBase),
		[]*node{shared, shared, other, nil},
		[]*big.Int{big.NewInt(0), nil, big.NewInt(1 << 40), new(big.Int)},
		[]float64{0, math.Copysign(0, -1), 1.5, 0, 0},
		[]complex128{0, complex(math.Copysign(0, -1), 0), complex(1, math.Copysign(0, -1))},
		[]any{backing[4:12], blobBase[8:24], "s", 42},
		map[string]any{
			"w": backing[10:20],
			"b": blobBase[0:16],
			"n": shared,
			"m": map[string]string{"x": "y"},
		},
	)
	return vals
}

// TestSoakGCBoundaries: a collection forced at every emission boundary
// (value, batch element, blob member) keeps the stream byte-identical —
// pinned member memory reads live bytes throughout.
func TestSoakGCBoundaries(t *testing.T) {
	r := rand.New(rand.NewSource(20260912))
	for _, v := range soakValues(r) {
		want, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal %T: %v", v, err)
		}
		emitProbeGC = true
		got, err := Marshal(v)
		emitProbeGC = false
		if err != nil {
			t.Fatalf("Marshal %T under forced GC: %v", v, err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("%T: forced-GC stream diverged (%d vs %d bytes)", v, len(want), len(got))
		}
	}
}

// TestSoakGCPoolRepeat: repeated encodes from the encoder pool with
// forced collections between and inside the streams stay 0-diff and keep
// the pool contract (no slot state survives the pool return).
func TestSoakGCPoolRepeat(t *testing.T) {
	r := rand.New(rand.NewSource(4242))
	vals := soakValues(r)
	for round := range 3 {
		for _, v := range vals {
			want, err := Marshal(v)
			if err != nil {
				t.Fatalf("round %d Marshal %T: %v", round, v, err)
			}
			runtime.GC()
			runtime.GC()
			emitProbeGC = true
			got, err := Marshal(v)
			emitProbeGC = false
			if err != nil {
				t.Fatalf("round %d Marshal %T under GC: %v", round, v, err)
			}
			if !bytes.Equal(want, got) {
				t.Fatalf("round %d %T: pool+GC stream diverged", round, v)
			}
		}
	}
}
