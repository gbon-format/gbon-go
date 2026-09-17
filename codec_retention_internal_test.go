package gbon

import (
	"bytes"
	"math/rand"
	"reflect"
	"runtime"
	"testing"
	"unsafe"
)

// retentionNode is the identity subject: a small pointer-rich node.
type retentionNode struct {
	Val  int
	Name string
	Next []*retentionNode
}

// retentionRefToken returns the wire encoding of a REF token for an
// intern-space id: the class nibble (0xC) in the first byte plus the
// minimal unsigned ARG form of the id, big-endian payload.
func retentionRefToken(id uint64) []byte {
	switch {
	case id <= 11:
		return []byte{0xC<<4 | byte(id)}
	case id <= 0xFF:
		return []byte{0xC<<4 | 0x0C, byte(id)}
	case id <= 0xFFFF:
		return []byte{0xC<<4 | 0x0D, byte(id >> 8), byte(id)}
	case id <= 0xFFFFFFFF:
		return []byte{0xC<<4 | 0x0E, byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{0xC<<4 | 0x0F,
			byte(id >> 56), byte(id >> 48), byte(id >> 40), byte(id >> 32),
			byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// retentionCountRef counts REF tokens for id in stream bytes.
func retentionCountRef(b []byte, id uint64) int {
	return bytes.Count(b, retentionRefToken(id))
}

// retentionDrop encodes one pointer root, drops the object and
// forces collection; the address names the dead entry. Pointer root:
// slice members stay arena-pinned and cannot die mid-stream.
func retentionDrop(t *testing.T, e *codecEncoder) uintptr {
	t.Helper()
	dead := func() uintptr {
		p := &retentionNode{Val: 42, Name: "dead"}
		if err := e.Encode(p); err != nil {
			t.Fatalf("Encode dead-subject: %v", err)
		}
		a := uintptr(unsafe.Pointer(p))
		p = nil
		return a
	}()
	runtime.GC()
	runtime.GC()
	return dead
}

// TestRetentionRepeatsGetSameRef: a live object met again resolves to
// the id of its first encounter — one REF token per extra meeting, in
// both the pointer and the map table.
func TestRetentionRepeatsGetSameRef(t *testing.T) {
	e := newCodecEncoder()
	shared := &retentionNode{Val: 7, Name: "shared"}
	if err := e.Encode([]*retentionNode{shared, shared, shared}); err != nil {
		t.Fatalf("Encode pointers: %v", err)
	}
	rec, hit := e.grains[uintptr(unsafe.Pointer(shared))]
	if !hit || rec.wp.Value() == nil {
		t.Fatalf("shared node not interned live")
	}
	if n := retentionCountRef(e.w.Bytes(), rec.id); n != 2 {
		t.Fatalf("repeat meetings: %d REF tokens, want 2", n)
	}

	e2 := newCodecEncoder()
	m := map[string]string{"k": "v"}
	if err := e2.Encode([]any{m, m}); err != nil {
		t.Fatalf("Encode maps: %v", err)
	}
	ent2, hit2 := e2.maps[uintptr(reflect.ValueOf(m).UnsafePointer())]
	if !hit2 || ent2.wp.Value() == nil {
		t.Fatalf("shared map not interned live")
	}
	if n := retentionCountRef(e2.w.Bytes(), ent2.id); n != 1 {
		t.Fatalf("map repeat: %d REF tokens, want 1", n)
	}
}

// TestRetentionDeadEntryIsMiss: a hit whose weak handle has cleared
// behaves as a miss — the next encounter re-interns under a fresh id,
// and no REF token for the dead id is ever emitted.
func TestRetentionDeadEntryIsMiss(t *testing.T) {
	e := newCodecEncoder()
	dead := retentionDrop(t, e)
	rec, hit := e.grains[dead]
	if !hit {
		t.Fatalf("dead entry missing from table")
	}
	if rec.wp.Value() != nil {
		t.Fatalf("weak handle still live after drop+GC")
	}
	deadID := rec.id

	// New object on a recycled address (when the allocator reuses it)
	// or a fresh address: either way a fresh id, never the dead one.
	q := &retentionNode{Val: 99, Name: "new"}
	if err := e.Encode([]*retentionNode{q}); err != nil {
		t.Fatalf("Encode newcomer: %v", err)
	}
	recQ, hitQ := e.grains[uintptr(unsafe.Pointer(q))]
	if !hitQ || recQ.wp.Value() == nil {
		t.Fatalf("newcomer not interned live")
	}
	if recQ.id == deadID {
		t.Fatalf("newcomer claimed the dead id %d", deadID)
	}
	if addrQ := uintptr(unsafe.Pointer(q)); addrQ == dead {
		if e.grains[dead].id == deadID {
			t.Fatalf("dead entry not overwritten on address reuse")
		}
	}
	if n := retentionCountRef(e.w.Bytes(), deadID); n != 0 {
		t.Fatalf("dead id emitted %d REF tokens, want 0", n)
	}
}

// TestRetentionZeroSizeNotTracked: zero-size pointees stay outside the
// intern tables (repeated meetings re-encode the body), while a
// one-byte pointee tracks normally.
func TestRetentionZeroSizeNotTracked(t *testing.T) {
	e := newCodecEncoder()
	z := &struct{ X struct{} }{}
	if err := e.Encode([]*struct{ X struct{} }{z, z}); err != nil {
		t.Fatalf("Encode zero-size: %v", err)
	}
	if _, hit := e.grains[uintptr(unsafe.Pointer(z))]; hit {
		t.Fatalf("zero-size pointee tracked in grains")
	}
	if len(e.grains) != 0 {
		t.Fatalf("ptrs table not empty: %d entries", len(e.grains))
	}

	e2 := newCodecEncoder()
	b1, b2 := new(byte), new(byte)
	if err := e2.Encode([]*byte{b1, b2}); err != nil {
		t.Fatalf("Encode byte pointers: %v", err)
	}
	if len(e2.grains) != 2 {
		t.Fatalf("one-byte pointees: %d table entries, want 2", len(e2.grains))
	}
}

// TestRetentionSweepHygiene: the sweep removes exactly the entries
// whose weak handle cleared; live entries and their ids survive, and
// bytes encoded after a sweep are unaffected.
func TestRetentionSweepHygiene(t *testing.T) {
	e := newCodecEncoder()
	live := &retentionNode{Val: 1, Name: "live"}
	if err := e.Encode([]*retentionNode{live}); err != nil {
		t.Fatalf("Encode live: %v", err)
	}
	liveAddr := uintptr(unsafe.Pointer(live))
	beforeID := e.grains[liveAddr].id
	dead := retentionDrop(t, e)
	liveBefore, deadBefore := len(e.grains), true

	e.sweepDeadInterns()

	if _, hit := e.grains[dead]; hit {
		t.Fatalf("dead entry survived sweep")
	}
	if len(e.grains) != liveBefore-1 {
		t.Fatalf("sweep removed %d entries, want exactly 1", liveBefore-len(e.grains))
	}
	rec, hit := e.grains[liveAddr]
	if !hit || rec.id != beforeID || rec.wp.Value() == nil {
		t.Fatalf("live entry damaged by sweep: hit=%v id=%d->%d", hit, beforeID, rec.id)
	}
	_ = deadBefore

	// Encoding after a sweep still REFs the live object by the same id.
	if err := e.Encode([]*retentionNode{live}); err != nil {
		t.Fatalf("Encode after sweep: %v", err)
	}
	if n := retentionCountRef(e.w.Bytes(), beforeID); n != 1 {
		t.Fatalf("post-sweep repeat: %d REF tokens, want 1", n)
	}
}

// TestRetentionSweepThreshold: after a second full wave crosses the
// sweep triggers, no dead entry from the first wave remains.
func TestRetentionSweepThreshold(t *testing.T) {
	e := newCodecEncoder()
	// One wave of live objects (a fixed array root: slice roots are
	// arena-pinned for the stream), then their deaths.
	var wave [sweepMaxInserts]*retentionNode
	var addrs [sweepMaxInserts]uintptr
	for i := range wave {
		wave[i] = &retentionNode{Val: i}
		addrs[i] = uintptr(unsafe.Pointer(wave[i]))
	}
	if err := e.Encode(wave); err != nil {
		t.Fatalf("Encode wave: %v", err)
	}
	if len(e.grains) < sweepMaxInserts {
		t.Fatalf("wave interned %d entries, want >= %d", len(e.grains), sweepMaxInserts)
	}
	for i := range wave {
		wave[i] = nil
	}
	runtime.GC()
	runtime.GC()

	// A second full wave crosses the dead-fraction probe and/or the
	// insertion-run count; both trigger the automatic sweep.
	var wave2 [sweepMaxInserts]*retentionNode
	for i := range wave2 {
		wave2[i] = &retentionNode{Val: -i}
	}
	if err := e.Encode(wave2); err != nil {
		t.Fatalf("Encode wave2: %v", err)
	}
	dead := 0
	for _, rec := range e.grains {
		if rec.wp.Value() == nil {
			dead++
		}
	}
	if dead != 0 {
		t.Fatalf("auto-sweep left %d dead entries in the table", dead)
	}
	for _, a := range addrs {
		if rec, h := e.grains[a]; h && rec.wp.Value() == nil {
			t.Fatalf("wave-1 dead entry survived at %x", a)
		}
	}
	if len(e.grains) < sweepMaxInserts {
		t.Fatalf("wave2 lost live entries: %d", len(e.grains))
	}
	runtime.KeepAlive(wave2)
}

// retentionGCProbe carries a coder that forces a collection in the
// middle of the encode walk.
type retentionGCProbe struct{ N int }

type retentionGCProbeCoder struct{}

func (retentionGCProbeCoder) EncodeValue(en *Encoder, v reflect.Value) error {
	runtime.GC()
	runtime.GC()
	return en.Encode(v.Interface().(retentionGCProbe).N)
}

func (retentionGCProbeCoder) DecodeValue(d *Decoder, v reflect.Value) error {
	var n int
	if err := d.Decode(&n); err != nil {
		return err
	}
	v.FieldByName("N").SetInt(int64(n))
	return nil
}

// TestRetentionMidEncodeGCStability: a forced GC mid-Encode cannot
// clear weak handles of root-reachable values; ids stay stable.
func TestRetentionMidEncodeGCStability(t *testing.T) {
	type midGraph struct {
		A *retentionNode
		P retentionGCProbe
		B *retentionNode
	}
	shared := &retentionNode{Val: 5, Name: "mid"}
	var buf bytes.Buffer
	fe := NewEncoder(&buf)
	if err := fe.RegisterCoder(retentionGCProbe{}, retentionGCProbeCoder{}); err != nil {
		t.Fatalf("RegisterCoder: %v", err)
	}
	if err := fe.Encode(midGraph{A: shared, P: retentionGCProbe{N: 9}, B: shared}); err != nil {
		t.Fatalf("Encode mid-GC: %v", err)
	}
	id := fe.enc.grains[uintptr(unsafe.Pointer(shared))].id
	if n := retentionCountRef(buf.Bytes(), id); n != 1 {
		t.Fatalf("mid-GC graph: %d REF tokens, want 1", n)
	}

	var out midGraph
	dec := NewDecoder(bytes.NewReader(buf.Bytes()))
	if err := dec.RegisterCoder(retentionGCProbe{}, retentionGCProbeCoder{}); err != nil {
		t.Fatalf("RegisterCoder decode: %v", err)
	}
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("Decode mid-GC stream: %v", err)
	}
	if out.A != out.B {
		t.Fatalf("sharing lost: decode returned distinct objects")
	}
	if out.A.Val != 5 || out.P.N != 9 {
		t.Fatalf("fidelity broken: A.Val=%d P.N=%d", out.A.Val, out.P.N)
	}
}

// TestRetentionInterValueGCWindow: objects dying between two Encode
// calls of one stream (the flush-point window) leave the stream valid:
// the next value re-interns fresh ids and never REFs the dead ones.
func TestRetentionInterValueGCWindow(t *testing.T) {
	e := newCodecEncoder()
	if err := e.Encode([]*retentionNode{{Val: 0}}); err != nil {
		t.Fatalf("Encode v1: %v", err)
	}
	deadID := func() uint64 {
		p := &retentionNode{Val: 1, Name: "inter"}
		if err := e.Encode(p); err != nil {
			t.Fatalf("Encode v2: %v", err)
		}
		id := e.grains[uintptr(unsafe.Pointer(p))].id
		p = nil
		return id
	}()
	runtime.GC()
	runtime.GC()

	if err := e.Encode([]*retentionNode{{Val: 2}}); err != nil {
		t.Fatalf("Encode v3: %v", err)
	}
	if n := retentionCountRef(e.w.Bytes(), deadID); n != 0 {
		t.Fatalf("inter-value window: dead id got %d REF tokens, want 0", n)
	}
}

// TestRetentionPBTDagSharing: for random DAGs with repeated live
// references, every extra meeting of a node emits exactly one REF to
// the id of its first meeting.
func TestRetentionPBTDagSharing(t *testing.T) {
	rng := rand.New(rand.NewSource(20260912))
	for round := range 25 {
		nodes := make([]*retentionNode, 3+rng.Intn(40))
		for i := range nodes {
			nodes[i] = &retentionNode{Val: i, Name: "n"}
		}
		for _, n := range nodes {
			for k := rng.Intn(4); k > 0; k-- {
				n.Next = append(n.Next, nodes[rng.Intn(len(nodes))])
			}
		}
		meetings := map[*retentionNode]int{}
		countMeetings(nodes[0], meetings)
		e := newCodecEncoder()
		if err := e.Encode(nodes[0]); err != nil {
			t.Fatalf("round %d Encode: %v", round, err)
		}
		for n, m := range meetings {
			if m < 2 {
				continue
			}
			recR, ok := e.grains[uintptr(unsafe.Pointer(n))]
			if !ok {
				t.Fatalf("round %d: referenced node not interned", round)
			}
			if got := retentionCountRef(e.w.Bytes(), recR.id); got != m-1 {
				t.Fatalf("round %d: node met %d times emitted %d REFs, want %d", round, m, got, m-1)
			}
		}
	}
}

// TestRetentionWindowScratchCleared: after reset the window scratch
// tail beyond [:0] holds zero windows — no member pin (and no window
// geometry) survives in the retained capacity across streams.
func TestRetentionWindowScratchCleared(t *testing.T) {
	e := newCodecEncoder()
	base := make([]int64, 16)
	if err := e.Encode([][]int64{base[0:8:8], base[4:16:16]}); err != nil {
		t.Fatalf("Encode windows: %v", err)
	}
	if len(e.arena.win) == 0 || len(e.arena.winT) == 0 {
		t.Fatal("window scratch empty before reset — probe vacuous")
	}
	e.arena.reset()
	if len(e.arena.win) != 0 || len(e.arena.winT) != 0 {
		t.Fatalf("window scratch lengths after reset: win=%d winT=%d", len(e.arena.win), len(e.arena.winT))
	}
	for i, w := range e.arena.win[:cap(e.arena.win)] {
		if w != (memWin{}) {
			t.Fatalf("retained window at %d: %+v", i, w)
		}
	}
	for i, et := range e.arena.winT[:cap(e.arena.winT)] {
		if et != nil {
			t.Fatalf("retained window type at %d: %v", i, et)
		}
	}
	for i, p := range e.arena.sPin[:cap(e.arena.sPin)] {
		if p != nil {
			t.Fatalf("retained slot pin at %d", i)
		}
	}
}

func countMeetings(root *retentionNode, meetings map[*retentionNode]int) {
	seen := map[*retentionNode]bool{}
	var walk func(*retentionNode)
	walk = func(n *retentionNode) {
		if n == nil || seen[n] {
			meetings[n]++
			return
		}
		seen[n] = true
		meetings[n] = 1
		for _, nx := range n.Next {
			walk(nx)
		}
	}
	walk(root)
}

// TestRetentionPBTNoDeadRefReuse: across death/realloc cycles, no
// fresh object ever claims a dead identity — no REF token for any id
// interned by a dropped wave appears in later output.
func TestRetentionPBTNoDeadRefReuse(t *testing.T) {
	rng := rand.New(rand.NewSource(987654321))
	e := newCodecEncoder()
	var deadIDs []uint64
	for round := range 12 {
		var wave [64]*retentionNode
		var addrs [64]uintptr
		for i := range wave {
			wave[i] = &retentionNode{Val: round*1000 + i + rng.Intn(10), Name: "aba"}
			addrs[i] = uintptr(unsafe.Pointer(wave[i]))
		}
		if err := e.Encode(wave); err != nil {
			t.Fatalf("round %d Encode: %v", round, err)
		}
		before := len(e.w.Bytes())
		for i := range wave {
			deadIDs = append(deadIDs, e.grains[addrs[i]].id)
			wave[i] = nil
		}
		runtime.GC()
		runtime.GC()
		deadNow := 0
		for _, a := range addrs {
			if rec, h := e.grains[a]; h && rec.wp.Value() == nil {
				deadNow++
			}
		}
		if deadNow == 0 {
			t.Fatalf("round %d: no wave entry died after drop+GC — probe vacuous", round)
		}
		fresh := &retentionNode{Val: -round, Name: "fresh"}
		if err := e.Encode(fresh); err != nil {
			t.Fatalf("round %d fresh Encode: %v", round, err)
		}
		tail := e.w.Bytes()[before:]
		for _, id := range deadIDs {
			if bytes.Contains(tail, retentionRefToken(id)) {
				t.Fatalf("round %d: fresh output REFs dead id %d", round, id)
			}
		}
	}
}
