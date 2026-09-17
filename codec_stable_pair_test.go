package gbon

// Isomorphic-pair differential: two independently allocated congruent
// value graphs encode to byte-equal streams under map-iteration seeds,
// including map-carried aliased backing arrays (multiple slice windows
// over one backing inside map values) — the instrument gap pinned by the
// exhaustiveness spike. All fixtures sit inside the stable class; the
// stable-mode variant asserts acceptance and byte equality with the
// default mode.

import (
	"bytes"
	"math/rand"
	"testing"
)

// pairNode is the shared-sub-structure unit of the basic graph: chains
// rebuilt by separate allocations stay congruent.
type pairNode struct {
	S    string
	N    int64
	Kids []*pairNode
}

// buildPairGraph builds one independently allocated graph: a node fan
// with a cycle back to the root, carried under a string-keyed map whose
// insertion order the caller permutes.
func buildPairGraph(seed int64) map[string]*pairNode {
	root := &pairNode{S: "root", N: 1}
	branches := make([]*pairNode, 6)
	for i := range branches {
		leafA := &pairNode{S: "a", N: int64(i)}
		leafB := &pairNode{S: "b", N: int64(i + 10)}
		branches[i] = &pairNode{S: "br", N: int64(i), Kids: []*pairNode{leafA, leafB}}
	}
	root.Kids = branches
	root.Kids = append(root.Kids, root) // cycle through the root
	m := make(map[string]*pairNode, 7)
	m["root"] = root
	rng := rand.New(rand.NewSource(seed))
	for _, j := range rng.Perm(len(branches)) {
		m[string(rune('a'+j))] = branches[j]
	}
	return m
}

// TestStablePairBasic: independently allocated congruent graphs — shared
// sub-structures rebuilt separately, a root cycle, string-keyed maps
// under permuted insertion orders — encode byte-identically in both
// modes; the stable mode accepts them (in-class).
func TestStablePairBasic(t *testing.T) {
	var first []byte
	for seed := range int64(16) {
		b, err := Marshal(buildPairGraph(seed))
		if err != nil {
			t.Fatalf("seed %d: marshal: %v", seed, err)
		}
		if first == nil {
			first = b
			continue
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("seed %d: isomorphic pair stream diverges", seed)
		}
	}
	for seed := range int64(4) {
		sb, err := MarshalStable(buildPairGraph(seed))
		if err != nil {
			t.Fatalf("seed %d: stable mode rejected an in-class graph: %v", seed, err)
		}
		if !bytes.Equal(sb, first) {
			t.Fatalf("seed %d: stable bytes differ from default bytes", seed)
		}
	}
	// array-keyed map over independently allocated congruent bodies
	var arrFirst []byte
	for seed := range int64(8) {
		m := map[[2]int64]string{}
		rng := rand.New(rand.NewSource(seed))
		for _, j := range rng.Perm(8) {
			m[[2]int64{int64(j), int64(j) * 3}] = "v"
		}
		b, err := Marshal(m)
		if err != nil {
			t.Fatalf("array-key map: %v", err)
		}
		if arrFirst == nil {
			arrFirst = b
			continue
		}
		if !bytes.Equal(b, arrFirst) {
			t.Fatalf("seed %d: array-key map stream diverges", seed)
		}
	}
}

// aliasedVal carries multiple slice windows over one backing array plus
// an array-pointer view into it: the join geometry the backing-join rule
// must decide from live bytes alone, never from scan order.
type aliasedVal struct {
	A []int64
	B []int64
	C []int64
	P *[3]int64
	D []int64
}

// buildAliasedMap builds one independently allocated aliased graph:
// a fresh backing per call, three overlapping windows over it, an array
// pointer into its middle, and a sub-slice chain, all carried as map
// values under a permuted insertion order.
func buildAliasedMap(seed int64) map[string]aliasedVal {
	m := make(map[string]aliasedVal, 9)
	rng := rand.New(rand.NewSource(seed))
	for _, j := range rng.Perm(9) {
		backing := make([]int64, 12)
		for i := range backing {
			backing[i] = int64(i + j)
		}
		m[string(rune('a'+j))] = aliasedVal{
			A: backing[0:5],
			B: backing[3:10],
			C: backing[8:12:12],
			P: (*[3]int64)(backing[4:7]),
			D: append(backing[1:2:2], 99, 100),
		}
	}
	return m
}

// TestStablePairAliasedBackings: map-carried values with aliased backing
// arrays (two slices over one backing inside map values, multi-grain
// addresses via overlapping windows and an array-pointer view), two
// independent allocations by sixteen map seeds — byte equality across
// seeds and pairs, in both modes.
func TestStablePairAliasedBackings(t *testing.T) {
	var first []byte
	for seed := range int64(16) {
		b, err := Marshal(buildAliasedMap(seed))
		if err != nil {
			t.Fatalf("seed %d: marshal: %v", seed, err)
		}
		if first == nil {
			first = b
			continue
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("seed %d: aliased-backing map stream diverges from seed 0", seed)
		}
	}
	for seed := range int64(4) {
		sb, err := MarshalStable(buildAliasedMap(seed))
		if err != nil {
			t.Fatalf("seed %d: stable mode rejected an aliased-backing map: %v", seed, err)
		}
		if !bytes.Equal(sb, first) {
			t.Fatalf("seed %d: stable bytes differ from default bytes", seed)
		}
	}
}
