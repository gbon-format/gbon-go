package gbon_test

import (
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Benchmark corpus: four representative value classes, each marshaled
// and unmarshaled. Not a performance gate — bitrot protection only.

type benchPoint struct {
	X, Y   int64
	Name   string
	Labels []string
}

func benchStruct() []benchPoint {
	s := make([]benchPoint, 128)
	for i := range s {
		s[i] = benchPoint{X: int64(i), Y: int64(i) * 2, Name: "pt", Labels: []string{"a", "b"}}
	}
	return s
}

func benchMap() map[string]int64 {
	m := make(map[string]int64, 256)
	for i := range 256 {
		m[string(rune('a'+i%26))+string(rune('a'+i/26))] = int64(i)
	}
	return m
}

// benchGraph builds a cyclic, sharing-heavy graph: 64 nodes in a ring,
// each also pointing at a shared payload.
func benchGraph() []*benchNode {
	shared := &benchPayload{Data: make([]int64, 32)}
	nodes := make([]*benchNode, 64)
	for i := range nodes {
		nodes[i] = &benchNode{ID: int64(i), Payload: shared}
	}
	for i := range nodes {
		nodes[i].Next = nodes[(i+1)%len(nodes)]
	}
	return nodes
}

type benchNode struct {
	ID      int64
	Next    *benchNode
	Payload *benchPayload
}

type benchPayload struct {
	Data []int64
}

// benchViews builds a shared backing with multiple windows (aliasing).
func benchViews() [4][]int64 {
	backing := make([]int64, 512)
	for i := range backing {
		backing[i] = int64(i)
	}
	return [4][]int64{backing[0:128], backing[128:256], backing[256:384], backing[384:512]}
}

func benchRT[T any](b *testing.B, v T) {
	b.Helper()
	data, err := gbon.Marshal(v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			if _, err := gbon.Marshal(v); err != nil {
				b.Fatal(err)
			}
		} else {
			var out T
			if err := gbon.Unmarshal(data, &out); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkStructs128(b *testing.B) { benchRT(b, benchStruct()) }

func BenchmarkMapPrimitiveKeys256(b *testing.B) { benchRT(b, benchMap()) }

func BenchmarkGraphRing64(b *testing.B) { benchRT(b, benchGraph()) }

func BenchmarkSliceViewsShared(b *testing.B) { benchRT(b, benchViews()) }

// Encode-only benchmarks (the encode/decode asymmetry marker): the same
// four classes plus primitive-vector classes, encode side isolated.

func benchEnc[T any](b *testing.B, v T) {
	b.Helper()
	data, err := gbon.Marshal(v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := gbon.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}

func benchInt64Slice512() []int64 {
	s := make([]int64, 512)
	for i := range s {
		s[i] = int64(i) * 1103515245
	}
	return s
}

func benchMonoVec128() []benchPoint {
	s := make([]benchPoint, 128)
	for i := range s {
		s[i] = benchPoint{X: int64(i), Y: int64(i) * 2}
	}
	return s
}

func BenchmarkEncodeStructs128(b *testing.B)  { benchEnc(b, benchStruct()) }
func BenchmarkEncodeMap256(b *testing.B)      { benchEnc(b, benchMap()) }
func BenchmarkEncodeGraphRing64(b *testing.B) { benchEnc(b, benchGraph()) }
func BenchmarkEncodeViews(b *testing.B)       { benchEnc(b, benchViews()) }
func BenchmarkEncodeInt64Slice512(b *testing.B) {
	benchEnc(b, benchInt64Slice512())
}
func BenchmarkEncodeMonoVec128(b *testing.B) { benchEnc(b, benchMonoVec128()) }

// Shared-backing fan (1000 overlapping windows over one byte backing):
// the decode-atomicity staging monitor — temp publication on the target
// path must not degrade shared-backing reconstruction. Mirrors the
// exotic scenario benchmark in the main bench corpus.
func BenchmarkSharedBackingFan1000(b *testing.B) {
	const (
		windows = 1000
		stride  = 4
		wlen    = 16
	)
	backing := make([]byte, (windows-1)*stride+wlen)
	for i := range backing {
		backing[i] = byte(i)
	}
	fan := make([][]byte, windows)
	for i := range fan {
		fan[i] = backing[i*stride : i*stride+wlen]
	}
	benchRT(b, fan)
}
