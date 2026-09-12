package gbon

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Frozen mirror of the topoGen builder from topo_gen_test.go: the encoder
// counters are package-private, so the metrics harness runs as an internal
// test, while the generator lives in the external test package. The source
// file is immutable (existing tests are never edited), so the mirror cannot
// drift. Axes and semantics are identical: depth/fan/reuse/cycle are
// caller-owned parameters.
type campaignGen struct {
	r        *rand.Rand
	maxDepth int
	maxFan   int
	reusePct int
	cyclePct int
	maps     []any
	slices   []any
	frames   []any
}

func (g *campaignGen) leaf() any {
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

func (g *campaignGen) value(depth int) any {
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

// campaignAxes is one polar-representative grid configuration: poles of
// each axis around a center point, pole combinations of the sharing axes,
// and a no-sharing control point. Deep and wide poles carry a fan/depth
// clamp so the generated graphs stay in the encoder's linear band. The
// table mirrors codec_campaign_bench_test.go; both are campaign files
// born together, one per test package.
type campaignAxes struct {
	name             string
	depth, fan       int
	reusePct, cycPct int
}

var campaignGrid = []campaignAxes{
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

// campaignSeeds are the pinned grid seeds.
var campaignSeeds = []int64{1, 2, 3}

// campaignScenarioValue builds the value of one grid cell.
func campaignScenarioValue(a campaignAxes, seed int64) any {
	g := &campaignGen{
		r:        rand.New(rand.NewSource(seed)),
		maxDepth: a.depth,
		maxFan:   a.fan,
		reusePct: a.reusePct,
		cyclePct: a.cycPct,
	}
	return g.value(0)
}

// campaignEncode runs one standalone encode of v and returns the encoder
// (the same standalone-stream setup runChainModel uses: header written,
// stream marked, one root value encoded).
func campaignEncode(t *testing.T, v any) *codecEncoder {
	t.Helper()
	e := newCodecEncoder()
	if err := e.w.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	e.started = true
	e.streamStart = len(e.w.Bytes())
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	return e
}

// campaignMetricsLine renders the metric row of one grid cell as one
// deterministic line: identical configuration and environment must yield
// byte-identical lines (the reproducibility probe compares them bitwise).
func campaignMetricsLine(a campaignAxes, seed int64, e *codecEncoder) string {
	W := e.workProbes + e.workElems
	units := e.nodes + len(e.arena.sPin)
	nEmitted := 0
	for _, ix := range e.classIx {
		nEmitted += len(ix.emitted)
	}
	total := e.hostHits + e.hostMisses
	rate := 0.0
	if total > 0 {
		rate = 100 * float64(e.hostHits) / float64(total)
	}
	return fmt.Sprintf("%s\tseed=%d\tW=%d\tunits=%d\tperUnit=%.4f\tprobes=%d\telems=%d\thits=%d\tmisses=%d\thitRate=%.4f\tnodes=%d\tnEmitted=%d\tnClasses=%d\tbytes=%d",
		a.name, seed, W, units, float64(W)/float64(units), e.workProbes, e.workElems,
		e.hostHits, e.hostMisses, rate, e.nodes, nEmitted, len(e.classIx),
		len(e.w.Bytes())-e.streamStart)
}

// campaignStableFields projects a metric line onto its
// scan-order-stable columns: the structural metrics of the
// reproducibility contract (units, elems, hits, misses, hitRate, nodes,
// nEmitted, nClasses, bytes). The work counters (W, perUnit, probes)
// additionally depend on the runtime-randomized iteration order of Go
// maps during the scan pass — W sums probes and elems, and the probes
// term carries the order noise — so their reproducibility probe is the
// stable projection plus the observed work band.
func campaignStableFields(line string) string {
	f := strings.Split(line, "\t")
	return strings.Join([]string{f[0], f[1], f[3], f[6], f[7], f[8], f[9], f[10], f[11], f[12], f[13]}, "\t")
}

// campaignWorkBand returns the width of the observed work-counter band
// (max W minus min W) across repeated encodes of one cell.
func campaignWorkBand(lines []string) int {
	min, max := 0, 0
	for i, l := range lines {
		f := strings.Split(l, "\t")
		w, err := strconv.Atoi(strings.TrimPrefix(f[2], "W="))
		if err != nil {
			return -1
		}
		if i == 0 || w < min {
			min = w
		}
		if i == 0 || w > max {
			max = w
		}
	}
	return max - min
}

// The grid metrics table: every cell of the polar grid is encoded three
// times from freshly generated values (same pinned seed) and the
// scan-order-stable projection of the metric lines must be
// byte-identical; the work counters carry the observed variation band
// instead (an order-noise measurement in itself). The no-sharing control
// point is present, and a zero-probe cell reports a guarded zero hit
// rate instead of NaN. With GBON_CAMPAIGN_OUT set, the table is
// dumped there as a campaign artifact (metrics.tsv with the bandW
// column).
func TestCampaignGridMetrics(t *testing.T) {
	controlSeen := false
	var lines []string
	for _, a := range campaignGrid {
		if a.reusePct == 0 && a.cycPct == 0 {
			controlSeen = true
		}
		for _, seed := range campaignSeeds {
			var ls []string
			for range 3 {
				e := campaignEncode(t, campaignScenarioValue(a, seed))
				ls = append(ls, campaignMetricsLine(a, seed, e))
			}
			stable := campaignStableFields(ls[0])
			for _, l := range ls[1:] {
				if s := campaignStableFields(l); s != stable {
					t.Errorf("%s seed=%d: stable metric fields not reproducible\n first:  %s\n second: %s",
						a.name, seed, stable, s)
				}
			}
			band := campaignWorkBand(ls)
			maxW := 0
			for _, l := range ls {
				f := strings.Split(l, "\t")
				w, _ := strconv.Atoi(strings.TrimPrefix(f[2], "W="))
				if w > maxW {
					maxW = w
				}
			}
			// Work counters carry order noise from Go's randomized map
			// iteration; under the compiled-plan engine a map-entry scan
			// order reaches the shared class indexes, so probe
			// sequences differ per encode and the same-seed work band
			// reaches ~0.4 of the cell's peak work. The
			// reproducibility contract bounds the observed band by half
			// of the peak: map-order noise stays below it, an anomalous
			// noise source (a band comparable to the whole cell's work)
			// crosses it. The ratio applies only where the band is wide
			// enough for integer probe counts to form a meaningful
			// ratio; below the absolute floor, near-zero cells would
			// ratio on granularity alone.
			if maxW > 0 && band > 16 && band*2 > maxW {
				t.Errorf("%s seed=%d: work band %d exceeds half of peak W=%d (noise source beyond map order)",
					a.name, seed, band, maxW)
			}
			line := ls[0] + fmt.Sprintf("\tbandW=%d", band)
			t.Logf("%s", line)
			lines = append(lines, line)
		}
	}
	if !controlSeen {
		t.Fatal("grid lacks the no-sharing control point")
	}
	out := os.Getenv("GBON_CAMPAIGN_OUT")
	if out == "" {
		return
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "metrics.tsv"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
