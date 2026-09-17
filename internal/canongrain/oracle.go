// Package canongrain is the bounded-exhaustive oracle of the canonical
// grain rule: a generator of small pointer graphs over a fixed type
// grammar, checked for decode success, round-trip identity (with the
// declared carve-outs), and byte idempotence.
package canongrain

import (
	"fmt"
	"math/rand"
	"reflect"
)

// dynTypes are the interface payload candidates; degenerate carries the
// leading any-field of the declared degenerate form.
var (
	degenerate = reflect.StructOf([]reflect.StructField{{Name: "F", Type: reflect.TypeFor[any]()}})
)

// Node is the graph node of the oracle grammar: a fixed-shape struct
// whose pointer slots carry the grain variety.
type Node struct {
	A    int64
	PInt *int64
	Self *Node
	Peer *Node
	Any  any
	Z    *struct{}
}

// Graph is one generated input: a node ring plus per-node slot choices.
type Graph struct {
	Root  *Node
	Nodes []*Node
}

// grammar enumerates the bounded type grammar the generator draws from.
type grammar struct {
	rnd *rand.Rand
}

func (g *grammar) int64() int64   { return g.rnd.Int63n(1000) }
func (g *grammar) string() string { return fmt.Sprintf("s%d", g.rnd.Intn(97)) }

// Fill populates one node's non-pointer slots.
func (g *grammar) Fill(n *Node, idx int) {
	n.A = g.int64()
	switch g.rnd.Intn(4) {
	case 0:
		n.Any = g.int64()
	case 1:
		n.Any = g.string()
	case 2:
		n.Any = degenerateValue(g.int64())
	case 3:
		n.Any = nil
	}
}

func degenerateValue(v int64) any {
	d := reflect.New(degenerate).Elem()
	d.Field(0).Set(reflect.ValueOf(v))
	return d.Interface()
}

// Gen produces one graph: a node list with a Next-ring over Self/Peer,
// cross-address pointer slots, and interior zero-size aliases.
func Gen(seed int64) *Graph {
	g := &grammar{rnd: rand.New(rand.NewSource(seed))}
	l := 1 + g.rnd.Intn(4)
	gr := &Graph{Nodes: make([]*Node, l)}
	for i := range gr.Nodes {
		gr.Nodes[i] = &Node{}
		g.Fill(gr.Nodes[i], i)
	}
	for i := range gr.Nodes {
		n := gr.Nodes[i]
		n.Self = gr.Nodes[g.rnd.Intn(l)]
		n.Peer = gr.Nodes[(i+1)%l]
		n.PInt = &gr.Nodes[i].A
		if g.rnd.Intn(2) == 0 {
			n.Z = &struct{}{}
		}
	}
	gr.Root = gr.Nodes[0]
	// a two-grain address: a bare int64 view of the first node's field
	if l > 1 && g.rnd.Intn(2) == 0 {
		gr.Root = gr.Nodes[1]
	}
	return gr
}

// Graphs returns the full bounded census for small sizes.
func Graphs(n int) []*Graph {
	out := make([]*Graph, 0, n)
	for i := range n {
		out = append(out, Gen(int64(i)))
	}
	return out
}
