package canongrain

// Differential runner over the grammar-census declaration of the
// conformance corpus (vectors/census.json): byte equality across
// independently generated isomorphic pairs, map-iteration seeds, and
// the declared encode modes, per the family matrix the declaration
// carries as data.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	gbon "github.com/gbon-format/gbon-go"
)

type censusFamily struct {
	Name     string   `json:"name"`
	MapSeeds int      `json:"map_seeds"`
	Modes    []string `json:"modes"`
	Asserts  string   `json:"asserts"`
}

type censusDeclaration struct {
	Category    string `json:"category"`
	Declaration struct {
		Grammar    string         `json:"grammar"`
		CensusSize int            `json:"census_size"`
		Families   []censusFamily `json:"families"`
	} `json:"declaration"`
	Vectors []json.RawMessage `json:"vectors"`
}

// censusRoot locates the mounted conformance corpus.
func censusRoot(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("GBON_SPEC_VECTORS"); d != "" {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			t.Fatalf("GBON_SPEC_VECTORS=%s is not a readable directory: %v", d, err)
		}
		return d
	}
	if fi, err := os.Stat("/spec"); err == nil && fi.IsDir() {
		return "/spec"
	}
	return ""
}

// loadCensusDeclaration reads the fixture-class declaration; the
// materialization step of the adapter for the census class.
func loadCensusDeclaration(t *testing.T) *censusDeclaration {
	t.Helper()
	root := censusRoot(t)
	if root == "" {
		t.Skip("conformance corpus not mounted (set GBON_SPEC_VECTORS or mount /spec)")
	}
	pb, err := os.ReadFile(filepath.Join(root, "vectors", "census.json"))
	if err != nil {
		t.Fatalf("census declaration: %v", err)
	}
	var d censusDeclaration
	if err := json.Unmarshal(pb, &d); err != nil {
		t.Fatalf("census declaration parse: %v", err)
	}
	return &d
}

// censusModesHas reports a declared mode; the matrix is data.
func censusModesHas(modes []string, want string) bool {
	return slices.Contains(modes, want)
}

// censusAssertModes asserts the declared mode matrix over one value:
// both encodings agree byte for byte, the stable mode accepting.
func censusAssertModes(t *testing.T, modes []string, name string, v any, want []byte) {
	t.Helper()
	if !censusModesHas(modes, "marshal-stable") {
		return
	}
	sb, err := gbon.MarshalStable(v)
	if err != nil {
		t.Fatalf("%s: stable mode rejected an in-class value: %v", name, err)
	}
	if want == nil {
		want, err = gbon.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
	}
	if !bytes.Equal(sb, want) {
		t.Fatalf("%s: stable bytes differ from default bytes", name)
	}
}

// censusMapWrap carries one census graph under a string-keyed map with
// a permuted insertion order: construction-order independence observed
// as byte equality.
func censusMapWrap(seed int64, g *Graph) map[string]*Node {
	m := make(map[string]*Node, len(g.Nodes)+1)
	rng := rand.New(rand.NewSource(seed))
	perm := rng.Perm(len(g.Nodes))
	m["root"] = g.Root
	for _, j := range perm {
		m[fmt.Sprintf("n%d", j)] = g.Nodes[j]
	}
	return m
}

// runCensusFamilyPair drives the census-pair family: independently
// generated isomorphic graphs byte-agree, and the map-wrapped carry
// agrees across the declared seed count.
func runCensusFamilyPair(t *testing.T, f censusFamily, size int) {
	t.Helper()
	for i := range size {
		g1 := Gen(int64(i))
		g2 := Gen(int64(i))
		b1, err := gbon.Marshal(g1.Root)
		if err != nil {
			t.Fatalf("census %d: marshal: %v", i, err)
		}
		b2, err := gbon.Marshal(g2.Root)
		if err != nil {
			t.Fatalf("census %d: marshal: %v", i, err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("census %d: isomorphic pair stream diverges", i)
		}
		censusAssertModes(t, f.Modes, fmt.Sprintf("census %d", i), g1.Root, b1)
	}
	var wrapFirst []byte
	for seed := range f.MapSeeds {
		v := censusMapWrap(int64(seed), Gen(0))
		b, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("map-wrap seed %d: marshal: %v", seed, err)
		}
		if wrapFirst == nil {
			wrapFirst = b
			censusAssertModes(t, f.Modes, "map-wrap", v, b)
			continue
		}
		if !bytes.Equal(b, wrapFirst) {
			t.Fatalf("map-wrap seed %d: stream diverges from seed 0", seed)
		}
	}
}

// censusAliased carries multiple slice windows over one backing plus
// an array-pointer view into it and an append-chain window.
type censusAliased struct {
	A []int64
	B []int64
	C []int64
	P *[3]int64
	D []int64
}

// buildCensusAliased builds one independently allocated aliased value;
// every call allocates fresh memory and permutes the map insertion.
func buildCensusAliased(seed int64) map[string]censusAliased {
	m := make(map[string]censusAliased, 4)
	rng := rand.New(rand.NewSource(seed))
	for _, j := range rng.Perm(4) {
		backing := make([]int64, 12)
		for i := range backing {
			backing[i] = int64(i + j)
		}
		d := make([]int64, 3, 4)
		d[0] = backing[1]
		d[1] = 99
		d[2] = 100
		m[string(rune('a'+j))] = censusAliased{
			A: backing[0:5],
			B: backing[3:10],
			C: backing[8:12:12],
			P: (*[3]int64)(backing[4:7]),
			D: d,
		}
	}
	return m
}

// runCensusFamilyAliased drives the aliased-backing family: fresh
// allocations under permuted map insertion byte-agree across the
// declared seed count.
func runCensusFamilyAliased(t *testing.T, f censusFamily) {
	t.Helper()
	var first []byte
	for seed := range f.MapSeeds {
		v := buildCensusAliased(int64(seed))
		b, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("aliased seed %d: marshal: %v", seed, err)
		}
		if first == nil {
			first = b
			censusAssertModes(t, f.Modes, "aliased", v, b)
			continue
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("aliased seed %d: stream diverges from seed 0", seed)
		}
	}
}

// censusGrain pairs a slice window with an array-grain window over one
// shared backing.
type censusGrain struct {
	W []int64
	P *[3]int64
}

// buildCensusGrain builds one independently allocated multi-grain
// value over fresh backings with seeded contents.
func buildCensusGrain(seed int64) map[string]censusGrain {
	m := make(map[string]censusGrain, 2)
	rng := rand.New(rand.NewSource(seed))
	for _, j := range rng.Perm(2) {
		backing := make([]int64, 6)
		for i := range backing {
			backing[i] = int64(7 + i + j)
		}
		m[string(rune('g'+j))] = censusGrain{
			W: backing[0:4],
			P: (*[3]int64)(backing[2:5]),
		}
	}
	return m
}

// runCensusFamilyGrain drives the multi-grain-address family: window
// and array-grain addresses over shared backings encode independently
// of allocation and insertion order.
func runCensusFamilyGrain(t *testing.T, f censusFamily) {
	t.Helper()
	var first []byte
	seeds := f.MapSeeds
	if seeds == 0 {
		seeds = 4
	}
	for seed := range seeds {
		v := buildCensusGrain(int64(seed))
		b, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("grain seed %d: marshal: %v", seed, err)
		}
		if first == nil {
			first = b
			censusAssertModes(t, f.Modes, "grain", v, b)
			continue
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("grain seed %d: stream diverges from seed 0", seed)
		}
	}
}

// runCensusDeclaration drives the whole declared family matrix; the
// census class materializes through the declaration read, the pair
// construction, and the byte comparison.
func runCensusDeclaration(t *testing.T, d *censusDeclaration) {
	t.Helper()
	if d.Declaration.Grammar != "canongrain.Graphs" {
		t.Fatalf("census declaration: unknown grammar %q", d.Declaration.Grammar)
	}
	size := d.Declaration.CensusSize
	if size <= 0 {
		t.Fatalf("census declaration: census size %d", size)
	}
	seen := map[string]bool{}
	for _, f := range d.Declaration.Families {
		if seen[f.Name] {
			t.Fatalf("census declaration: family %q declared twice", f.Name)
		}
		seen[f.Name] = true
		switch f.Name {
		case "census-pair":
			runCensusFamilyPair(t, f, size)
		case "aliased-backing":
			runCensusFamilyAliased(t, f)
		case "multi-grain-address":
			runCensusFamilyGrain(t, f)
		default:
			t.Fatalf("census declaration: unknown fixture family %q", f.Name)
		}
	}
}

// TestSpecCensusDifferential: the declared census holds — isomorphic
// pairs, map seeds, and both encode modes byte-agree over every
// declared family.
func TestSpecCensusDifferential(t *testing.T) {
	runCensusDeclaration(t, loadCensusDeclaration(t))
}
