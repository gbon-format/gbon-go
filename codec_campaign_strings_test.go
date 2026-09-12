package gbon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// String-position census over a value graph, public API only. The walk
// uses first-encounter semantics — repeated reference nodes (map
// headers, slice windows, pointer targets) are entered once, keyed by
// the same identity model as topoPaths (topoKey) — mirroring the
// encoder's identity interning: every string position reached on first
// encounter is one WriteString call, repeats included. The census runs
// twice per scenario, over the original value and over its decoded
// round-trip copy, and the two must agree exactly (round-trip preserves
// the reference graph, so the string-position multiset is invariant).

// campaignCountStrings walks v counting string positions; seen carries
// the first-encounter set of reference nodes.
func campaignCountStrings(v reflect.Value, seen map[topoKey]bool, occ map[string]int) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		campaignCountStrings(v.Elem(), seen, occ)
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		k := topoKey{a: v.Pointer()}
		if seen[k] {
			return
		}
		seen[k] = true
		campaignCountStrings(v.Elem(), seen, occ)
	case reflect.Map:
		if v.IsNil() {
			return
		}
		k := topoKeyOf(v)
		if seen[k] {
			return
		}
		seen[k] = true
		iter := v.MapRange()
		for iter.Next() {
			if iter.Key().Kind() == reflect.String {
				occ[iter.Key().String()]++
			} else {
				campaignCountStrings(iter.Key(), seen, occ)
			}
			campaignCountStrings(iter.Value(), seen, occ)
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		k := topoKeyOf(v)
		if seen[k] {
			return
		}
		seen[k] = true
		for i := 0; i < v.Len(); i++ {
			campaignCountStrings(v.Index(i), seen, occ)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			campaignCountStrings(v.Field(i), seen, occ)
		}
	case reflect.String:
		occ[v.String()]++
	}
}

// campaignCensus is the string-position multiset of one value.
func campaignCensus(v any) map[string]int {
	occ := map[string]int{}
	campaignCountStrings(reflect.ValueOf(v), map[topoKey]bool{}, occ)
	return occ
}

// campaignCensusEqual reports the number of strings whose occurrence
// counts differ between two censuses (zero = exact agreement).
func campaignCensusDiffs(a, b map[string]int) int {
	n := 0
	for k, c := range a {
		if b[k] != c {
			n++
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			n++
		}
	}
	return n
}

// campaignStringsScenario is one census target: the same topoGen grid
// the metrics and timing legs use, plus the frozen bench corpus values.
type campaignStringsScenario struct {
	name   string
	build  func() any
	decode func([]byte) (any, error)
}

func campaignStringsScenarios() []campaignStringsScenario {
	var sc []campaignStringsScenario
	for _, a := range campaignBenchGrid {
		for _, seed := range campaignBenchSeeds {
			sc = append(sc, campaignStringsScenario{
				fmt.Sprintf("grid/%s/seed=%d", a.name, seed),
				func() any { return campaignBenchValue(a, seed) },
				func(data []byte) (any, error) {
					dec := gbon.NewDecoder(bytes.NewReader(data))
					if err := dec.Register([]any{}, map[string]any{}, int64(0)); err != nil {
						return nil, err
					}
					var out any
					if err := dec.Decode(&out); err != nil {
						return nil, err
					}
					return out, nil
				},
			})
		}
	}
	sc = append(sc,
		campaignStringsScenario{
			"corpus/structs128",
			func() any { return benchStruct() },
			func(data []byte) (any, error) {
				var out []benchPoint
				if err := gbon.Unmarshal(data, &out); err != nil {
					return nil, err
				}
				return out, nil
			},
		},
		campaignStringsScenario{
			"corpus/map256",
			func() any { return benchMap() },
			func(data []byte) (any, error) {
				var out map[string]int64
				if err := gbon.Unmarshal(data, &out); err != nil {
					return nil, err
				}
				return out, nil
			},
		},
	)
	return sc
}

// campaignStringsBuckets are the histogram length-bucket edges.
var campaignStringsEdges = []int{2, 4, 8, 16, 32, 64, 1 << 30}

// campaignBucketRow is one length bucket of the census histogram.
type campaignBucketRow struct {
	Lo, Hi         int
	Distinct       int
	Occurrences    int
	RepeatBytes    int // (occ-1)*len summed over the bucket's strings
	OccPerDistinct float64
}

// The string-distribution census: every scenario is marshaled, censused
// on the original value and on the decoded round-trip copy, and the two
// censuses must agree exactly (the round-trip preserves the reference
// graph). The histogram (length x repeat frequency, bytes from repeats
// per bucket) is logged; with GBON_CAMPAIGN_STRINGS_OUT set it is also
// dumped there as a campaign artifact.
func TestCampaignStringsCensus(t *testing.T) {
	type outRow struct {
		Scenario  string              `json:"scenario"`
		Positions int                 `json:"positions"`
		Distinct  int                 `json:"distinct"`
		Buckets   []campaignBucketRow `json:"buckets"`
	}
	var outs []outRow
	for _, sc := range campaignStringsScenarios() {
		v := sc.build()
		data, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("%s: Marshal: %v", sc.name, err)
		}
		back, err := sc.decode(data)
		if err != nil {
			t.Fatalf("%s: decode: %v", sc.name, err)
		}
		orig := campaignCensus(v)
		dec := campaignCensus(back)
		if d := campaignCensusDiffs(orig, dec); d > 0 {
			t.Errorf("%s: census divergence original vs decoded: %d strings differ", sc.name, d)
		}
		positions := 0
		for _, c := range orig {
			positions += c
		}
		row := outRow{Scenario: sc.name, Positions: positions, Distinct: len(orig)}
		lo := 1
		for _, hi := range campaignStringsEdges {
			b := campaignBucketRow{Lo: lo, Hi: hi}
			for s, n := range orig {
				L := len(s)
				if L < lo || L > hi {
					continue
				}
				b.Distinct++
				b.Occurrences += n
				b.RepeatBytes += (n - 1) * L
			}
			if b.Distinct > 0 {
				b.OccPerDistinct = float64(b.Occurrences) / float64(b.Distinct)
			}
			row.Buckets = append(row.Buckets, b)
			lo = hi + 1
		}
		outs = append(outs, row)
		t.Logf("%s: positions=%d distinct=%d", sc.name, row.Positions, row.Distinct)
		for _, b := range row.Buckets {
			if b.Distinct == 0 {
				continue
			}
			t.Logf("  len %d-%d: distinct=%d occurrences=%d occ/dist=%.2f repeatBytes=%d",
				b.Lo, b.Hi, b.Distinct, b.Occurrences, b.OccPerDistinct, b.RepeatBytes)
		}
	}
	out := os.Getenv("GBON_CAMPAIGN_STRINGS_OUT")
	if out == "" {
		return
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(outs, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "strings-census.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
