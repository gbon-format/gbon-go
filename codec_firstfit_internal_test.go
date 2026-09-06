package gbon

import (
	"bytes"
	"testing"
	"unsafe"
)

// firstFitOracle replays the pre-index join rule over the live arena
// state: every emitted component is tested with the same containment
// and zero-tail predicate in creation-sequence order; the first hit is
// the join target. Live (non-emitted) components resolve by the same
// candidate scan the scan path performs: overlap below the key split,
// equal-origin or strict overlap above it.
func firstFitOracle(e *codecEncoder, v reflectView) int32 {
	a := &e.arena
	es := v.es
	p := v.ptr
	send := p + uintptr(v.cap)*es
	ix := e.classIndexOf(groupClass{es: es, blob: v.blob})
	var best int32 = -1
	for _, ci := range ix.emitted {
		c := &a.comps[ci]
		if representableOracle(c, v, p) && (best < 0 || c.seq < a.comps[best].seq) {
			best = ci
		}
	}
	liveBest := int32(-1)
	for _, en := range ix.envs {
		if en.dead {
			continue
		}
		c := &a.comps[en.ci]
		if c.dead() {
			continue
		}
		if p == c.origin || (p < c.end && c.origin < send) {
			if liveBest < 0 || c.seq < a.comps[liveBest].seq {
				liveBest = en.ci
			}
		}
	}
	if best < 0 {
		return liveBest
	}
	if liveBest < 0 {
		return best
	}
	if a.comps[liveBest].seq < a.comps[best].seq {
		return liveBest
	}
	return best
}

// representableOracle mirrors representable without touching the work
// counters: containment of the cap-window plus bitwise-zero tail past
// the frozen elided prefix.
func representableOracle(c *envComp, v reflectView, p uintptr) bool {
	send := p + uintptr(v.cap)*v.es
	if p < c.origin || send > c.end {
		return false
	}
	off := uint64((p - c.origin) / v.es)
	for i := uint64(0); i < uint64(v.ln); i++ {
		if off+i >= c.elided && v.mem[v.off+int(i)] != 0 {
			return false
		}
	}
	return true
}

// reflectView carries the window geometry plus the backing memory for
// the oracle's direct byte reads.
type reflectView struct {
	es   uintptr
	blob bool
	ptr  uintptr
	off  int
	ln   int
	cap  int
	mem  []byte
}

// runSteps encodes the window sequence through one stream encoder and
// asserts, after every slot, that the recorded join resolution equals
// the linear first-fit oracle over the same arena state.
func runSteps(t *testing.T, mem []byte, wins [][3]int, muts map[int][2]int) *codecEncoder {
	t.Helper()
	base := uintptr(unsafe.Pointer(&mem[0]))
	e := newCodecEncoder()
	if err := e.w.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	e.started = true
	e.streamStart = len(e.w.Bytes())
	for si, w := range wins {
		off, ln, capw := w[0], w[1], w[2]
		vv := mem[off : off+ln : off+capw]
		if err := e.Encode(vv); err != nil {
			t.Fatalf("step %d: encode: %v", si, err)
		}
		key := slotKey{gen: e.gen, ptr: base + uintptr(off), n: ln, c: capw, blob: true}
		got, ok := e.slotIx[key]
		if !ok {
			t.Fatalf("step %d: slot key not registered", si)
		}
		want := firstFitOracle(e, reflectView{
			es: 1, blob: true, ptr: base + uintptr(off),
			off: off, ln: ln, cap: capw, mem: mem,
		})
		if got != want {
			t.Fatalf("step %d (win off=%d ln=%d cap=%d): joined comp %d, oracle wants %d; comps:\n%s",
				si, off, ln, capw, got, want, dumpComps(e, base))
		}
		if mu, ok := muts[si]; ok {
			mem[mu[0]] = byte(mu[1])
		}
	}
	return e
}

func dumpComps(e *codecEncoder, base uintptr) string {
	var b bytes.Buffer
	for ci := range e.arena.comps {
		c := &e.arena.comps[ci]
		b.WriteString("\n\tcomp ")
		b.WriteString(itoa(int64(ci)))
		b.WriteString(": [")
		b.WriteString(itoa(int64((c.origin - base) / c.es)))
		b.WriteString(",")
		b.WriteString(itoa(int64((c.end - base) / c.es)))
		b.WriteString(") seq=")
		b.WriteString(itoa(int64(c.seq)))
		if c.emitted {
			b.WriteString(" emitted elided=")
			b.WriteString(itoa(int64(c.elided)))
		}
	}
	return b.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Deterministic window sequences covering the join adversarial classes:
// leftover stacks (fresh records emitted over frozen ones), back-looking
// windows below a frozen origin, equal-origin windows of different
// widths, and mutated tails between visits of the same geometry.
func TestEmitIndexFirstFitIdentity(t *testing.T) {
	mem := []byte{0x00, 0x00, 0x4e, 0x00, 0xf7, 0x80, 0x00, 0xbe}
	runSteps(t, mem, [][3]int{
		{5, 3, 3}, {2, 2, 6}, {0, 3, 3}, {4, 3, 4},
	}, map[int][2]int{0: {3, 11}, 2: {2, 32}, 3: {1, 92}})

	mem2 := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	runSteps(t, mem2, [][3]int{
		{0, 4, 4}, {2, 4, 8}, {6, 2, 6}, {0, 6, 12},
		{4, 4, 4}, {0, 12, 12}, {2, 2, 2},
	}, map[int][2]int{1: {7, 0x40}, 3: {1, 0x50}})

	mem3 := []byte{9, 0, 0, 0, 0, 0, 0, 0}
	runSteps(t, mem3, [][3]int{
		{0, 2, 8}, {0, 2, 8}, {0, 4, 8}, {2, 2, 6},
		{0, 8, 8}, {0, 2, 8},
	}, map[int][2]int{2: {2, 0x33}})

	mem4 := []byte{0, 1, 0, 2, 0, 3, 0, 4}
	runSteps(t, mem4, [][3]int{
		{0, 1, 8}, {7, 1, 1}, {3, 1, 5}, {0, 8, 8},
		{1, 6, 7}, {6, 2, 2}, {2, 4, 6}, {0, 4, 8},
	}, nil)
}

// A mutated zero tail between visits of the same window geometry must
// fall back to a fresh record — never a view over the stale host — and
// the host cache must not bypass that fallback (the trap: the cached
// containment-min is re-validated by a full representable check).
func TestHostCacheMutatedTailTrap(t *testing.T) {
	mem := []byte{1, 2, 0, 0, 0}
	base := uintptr(unsafe.Pointer(&mem[0]))
	e := newCodecEncoder()
	if err := e.w.WriteHeader(); err != nil {
		t.Fatal(err)
	}
	e.started = true
	e.streamStart = len(e.w.Bytes())
	enc := func(off, ln, capw int) int32 {
		t.Helper()
		if err := e.Encode(mem[off : off+ln : off+capw]); err != nil {
			t.Fatal(err)
		}
		ci, ok := e.slotIx[slotKey{gen: e.gen, ptr: base + uintptr(off), n: ln, c: capw, blob: true}]
		if !ok {
			t.Fatal("slot key missing")
		}
		return ci
	}
	c0 := enc(0, 2, 5) // frozen host [0,5), elided=2
	if !e.arena.comps[c0].emitted {
		t.Fatal("first window must emit")
	}
	// A wider len-window over the same host: representable (the tail
	// [2,4) is zero), joined by the full search — the first visit
	// always runs it, the cache fills during the scan pass.
	c1 := enc(0, 4, 5)
	if c1 != c0 {
		t.Fatalf("wider window joined %d, want host %d", c1, c0)
	}
	// The repeat resolves through the cache with a full representable
	// re-validation.
	c1b := enc(0, 4, 5)
	if c1b != c0 {
		t.Fatalf("cached repeat joined %d, want host %d", c1b, c0)
	}
	if e.hostHits == 0 {
		t.Fatal("clean repeat must hit the host cache")
	}
	mem[3] = 0xAA // mutate the tail inside the len-window, past elided
	hitsBefore, missesBefore := e.hostHits, e.hostMisses
	c2 := enc(0, 4, 5)
	if c2 == c0 {
		t.Fatalf("mutated-tail window resolved to the stale host %d", c2)
	}
	if e.hostHits != hitsBefore || e.hostMisses != missesBefore+1 {
		t.Fatal("mutated-tail window must fail the cached re-validation and fall through to the full search")
	}
	if !e.arena.comps[c2].emitted {
		t.Fatal("fallback must emit its own record")
	}
	// Mutations past the len-window stay invisible: the narrow repeat
	// still joins the host (first-encounter snapshot semantics).
	c3 := enc(0, 2, 5)
	if c3 != c0 {
		t.Fatalf("narrow repeat joined %d, want host %d", c3, c0)
	}
}
