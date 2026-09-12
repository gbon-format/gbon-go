package gbon_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// The polar-representative scenario grid for the timing side of the
// measurement campaign. Mirror of the table in
// codec_campaign_internal_test.go (one copy per test package; both are
// campaign files born together). Values are built by the topoGen
// generator of topo_gen_test.go with pinned seeds.
type campaignBenchAxes struct {
	name             string
	depth, fan       int
	reusePct, cycPct int
}

var campaignBenchGrid = []campaignBenchAxes{
	{"control-noshare", 6, 4, 0, 0},
	{"depth-lo", 3, 4, 20, 10},
	{"depth-hi", 12, 2, 20, 10},
	{"fan-lo", 6, 1, 20, 10},
	{"fan-hi", 3, 8, 20, 10},
	{"reuse-lo", 6, 4, 0, 10},
	{"reuse-hi", 6, 4, 60, 10},
	{"cycle-lo", 6, 4, 20, 0},
	{"cycle-hi", 6, 4, 20, 30},
	{"share-hh", 6, 4, 60, 30},
	{"share-hl", 6, 4, 60, 0},
	{"share-lh", 6, 4, 0, 30},
	{"share-mm", 6, 4, 30, 15},
	{"deep-sharing", 10, 2, 40, 20},
	{"wide-flat", 3, 6, 20, 10},
	{"mono-deep", 14, 1, 0, 0},
	{"dense-sharing", 5, 5, 50, 25},
	{"sparse", 8, 3, 10, 5},
	{"ring-heavy", 6, 4, 30, 40},
	{"pure-reuse", 4, 4, 60, 0},
}

var campaignBenchSeeds = []int64{1, 2, 3}

func campaignBenchValue(a campaignBenchAxes, seed int64) any {
	g := &topoGen{
		r:        rand.New(rand.NewSource(seed)),
		maxDepth: a.depth,
		maxFan:   a.fan,
		reusePct: a.reusePct,
		cyclePct: a.cycPct,
	}
	return g.value(0)
}

// Encode-side timing over the full polar grid: one benchmark per grid
// cell (configuration x pinned seed). Encode is the isolated leg (the
// asymmetry marker); decode timing lives in the phase benchmarks below.
func BenchmarkCampaignGrid(b *testing.B) {
	for _, a := range campaignBenchGrid {
		for _, seed := range campaignBenchSeeds {
			b.Run(fmt.Sprintf("%s/seed=%d", a.name, seed), func(b *testing.B) {
				v := campaignBenchValue(a, seed)
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
			})
		}
	}
}

// Phase scenarios: three structurally distinct workloads with isolated
// encode and decode legs, used both for A/B timing and as the profiling
// targets (one scenario per leg per profile).
var campaignPhaseScenarios = []struct {
	name   string
	build  func() any
	decode func([]byte) error
}{
	{
		"sharing-heavy",
		func() any { return campaignBenchValue(campaignBenchAxes{"share-hh", 6, 4, 60, 30}, 1) },
		func(data []byte) error {
			dec := gbon.NewDecoder(bytes.NewReader(data))
			if err := dec.Register([]any{}, map[string]any{}, int64(0)); err != nil {
				return err
			}
			var out any
			return dec.Decode(&out)
		},
	},
	{
		"map-heavy",
		func() any { return benchMap() },
		func(data []byte) error {
			var out map[string]int64
			return gbon.Unmarshal(data, &out)
		},
	},
	{
		"mono-vector",
		func() any { return benchMonoVec128() },
		func(data []byte) error {
			var out []benchPoint
			return gbon.Unmarshal(data, &out)
		},
	},
}

func BenchmarkCampaignPhase(b *testing.B) {
	for _, s := range campaignPhaseScenarios {
		v := s.build()
		data, err := gbon.Marshal(v)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(s.name+"/encode", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := gbon.Marshal(v); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(s.name+"/decode", func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.decode(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// campaignG2Value is the shared payload of the parallel-stream
// benchmarks: the sharing-heavy scenario at a pinned seed.
func campaignG2Value() any {
	return campaignBenchValue(campaignBenchAxes{"share-hh", 6, 4, 60, 30}, 1)
}

// campaignG2Stream runs one full independent stream over v: a fresh
// Encoder into its own buffer, then a fresh Decoder over those bytes.
func campaignG2Stream(v any) {
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err := dec.Register([]any{}, map[string]any{}, int64(0)); err != nil {
		panic(err)
	}
	var out any
	if err := dec.Decode(&out); err != nil {
		panic(err)
	}
}

// Parallel-stream throughput: one iteration is a full wave of n
// concurrent independent streams (encode+decode each), one goroutine per
// stream. The goroutine count per wave is exactly n regardless of
// GOMAXPROCS; ns/op is the wave time, streams/sec is n divided by it.
func BenchmarkCampaignG2Throughput(b *testing.B) {
	for _, n := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("streams=%d", n), func(b *testing.B) {
			v := campaignG2Value()
			data, err := gbon.Marshal(v)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(n * len(data)))
			b.ReportAllocs()
			var wg sync.WaitGroup
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for range n {
					wg.Go(func() {
						campaignG2Stream(v)
					})
				}
				wg.Wait()
			}
		})
	}
}

// Parallel-stream latency: n workers run independent streams in
// parallel until b.N streams total complete; ns/op is the mean
// per-stream time under n-way concurrency.
func BenchmarkCampaignG2Latency(b *testing.B) {
	for _, n := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("streams=%d", n), func(b *testing.B) {
			v := campaignG2Value()
			var ops atomic.Int64
			var done sync.WaitGroup
			for range n {
				done.Go(func() {
					for {
						if ops.Add(1) > int64(b.N) {
							return
						}
						campaignG2Stream(v)
					}
				})
			}
			b.ResetTimer()
			done.Wait()
		})
	}
}
