package gbon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gbon-format/gbon-go/internal/wire"
)

// pathNode is one structural segment of a value path: a field name or an
// element index. Frames chain through parent pointers; the path string
// materializes only in error branches — successful encode/decode passes
// carry no path strings (lazy diagnostics). Map keys are addressed by the
// bounded key rendering in errors.go (boundedKey), not by pair indices.
type pathNode struct {
	parent *pathNode
	name   string // field segment (without the dot); empty when idx >= 0
	idx    int    // element index; -1 for a field segment
}

// String materializes the full path in one pass. Error branches only:
// the walk is iterative, so deep paths cannot grow the native stack, and
// no parameter-derived pointer is stored — the buffer fills from the
// leaf backwards, keeping call frames stack-allocated at the call sites.
// The root frame renders as "$" (the grammar's root marker), so a field
// path reads "$.a" and an element path "$[0]".
func (p *pathNode) String() string {
	if p == nil {
		return ""
	}
	var dbuf [24]byte
	n := 0
	for q := p; q != nil; q = q.parent {
		if q.idx >= 0 {
			n += 2 + len(strconv.AppendInt(dbuf[:0], int64(q.idx), 10))
		} else if q.parent == nil {
			n += 1
		} else {
			n += 1 + len(q.name)
		}
	}
	buf := make([]byte, n)
	i := n
	for q := p; q != nil; q = q.parent {
		if q.idx >= 0 {
			i--
			buf[i] = ']'
			d := strconv.AppendInt(dbuf[:0], int64(q.idx), 10)
			i -= len(d)
			copy(buf[i:], d)
			i--
			buf[i] = '['
		} else if q.parent == nil {
			i--
			buf[i] = '$'
		} else {
			i -= len(q.name)
			copy(buf[i:], q.name)
			i--
			buf[i] = '.'
		}
	}
	return string(buf)
}

// typeEntry tracks per-stream descriptor emission state for one reflect.Type
// (the identity key is the type, not the name).
type typeEntry struct {
	d  *wire.Desc
	id uint64
	ok bool
}

// pinnedID is one stream-lifetime identity intern: the intern-space id
// plus the keyed object itself. A bare-address key is an identity token
// valid only while the object stays alive; retaining the value pins the
// address against allocator reuse for as long as the id is resolvable,
// so a subsequent object landing on a freed slot can never claim an earlier
// record's identity.
type pinnedID struct {
	id uint64
	v  reflect.Value
}

// Default encode budgets: the input side is trusted (the value already
// exists in memory), so only the derived resources — stack frames, output
// nodes, output bytes — are capped, at looser limits than the decoder's
// conservative defaults.
const (
	encodeDefaultDepth = 1000000
	encodeDefaultNodes = 10000000
	encodeDefaultBytes = 1000000000
)

// effEncodeLimits resolves the zero fields of l onto the encode defaults.
// MaxMapPairs and MaxSliceLen do not apply on the encode side: a live
// value is not a consumable input resource. A negative MaxBytes is the
// no-cap switch (trusted-producer trust decision) — materialized as the
// maximum int so the guards stay untouched.
func effEncodeLimits(l Limits) Limits {
	if l.MaxDepth == 0 {
		l.MaxDepth = encodeDefaultDepth
	}
	if l.MaxNodes == 0 {
		l.MaxNodes = encodeDefaultNodes
	}
	switch {
	case l.MaxBytes == 0:
		l.MaxBytes = encodeDefaultBytes
	case l.MaxBytes < 0:
		l.MaxBytes = math.MaxInt
	}
	return l
}

// codecEncoder is the value encoder over the wire Writer: reflect walk,
// per-type descriptor interning, pointer-target and map identity interning,
// and slice-view backing grouping.
type codecEncoder struct {
	w           *wire.Writer
	types       map[reflect.Type]typeEntry
	ptrs        map[uintptr]pinnedID
	maps        map[uintptr]pinnedID
	started     bool
	broken      error
	coders      map[reflect.Type]Coder  // RegisterCoder scope
	coderTags   map[reflect.Type]uint64 // per-stream encounter-order tags
	coderCache  map[reflect.Type]*typeCoder
	structDescs map[reflect.Type]*wire.Desc // struct walk memo: one build per type per stream
	asName      map[reflect.Type]string     // RegisterAs bindings: type → wire name
	asType      map[string]reflect.Type     // RegisterAs bindings: wire name → type
	activeCodrs map[reflect.Type]bool
	fac         *Encoder
	inCoder     int                        // >0 inside a coder body: Encode skips the flush
	lim         Limits                     // effective encode budgets (depth/nodes/bytes)
	depth       int                        // current encode recursion frames (scan and encodeBody passes)
	nodes       int                        // output node count (encodeBody calls) this stream
	flushed     int                        // stream output bytes already flushed to the sink
	streamStart int                        // buffer index where the value stream begins (after the header)
	gen         uint64                     // slot generation: bumped per encodeRoot value, keys emission resolution
	classIx     map[groupClass]*classIndex // per-class envelope index over components
	slotIx      map[slotKey]int32          // slot window key → owning component (arena index)
	hostCache   map[hostKey]int32          // slot window → minimal-seq containing emitted component
	hostHits    int64                      // host-cache resolutions that skipped the emitted search
	hostMisses  int64                      // full emitted-search passes (cache absent or stale)
	gseq        int32                      // component creation sequence: first-fit order
	workProbes  int64                      // slot-search work: structural comparisons per slot probe
	workElems   int64                      // representable zero-tail element-loop iterations
	arena       scanArena                  // intrusive grouping arena: slots + envelope components
	hdrVisits   map[hdrKey]uint64          // scan visited headers: key → epoch of its last visit (pooled working memory)
	hdrEp       uint64                     // header-table epoch: bumped per scanValue, one per scan
	skel        *skeletonScratch           // reused canonical-key render state (map ordering)
	refFree     map[reflect.Type]bool      // scan cache: types with no reference component
}

// refFreeType reports whether t contains no reference kind (pointer,
// slice, map, interface, channel, function) at any depth, memoized per
// stream for the scan pass.
func (e *codecEncoder) refFreeType(t reflect.Type) bool {
	if hit, ok := e.refFree[t]; ok {
		return hit
	}
	res := refFreeWalk(t, nil)
	if e.refFree == nil {
		e.refFree = make(map[reflect.Type]bool)
	}
	e.refFree[t] = res
	return res
}

func refFreeWalk(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return true
	}
	if len(seen) > 64 {
		return false
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return false
	case reflect.Struct:
		seen2 := make(map[reflect.Type]bool, len(seen)+1)
		for k := range seen {
			seen2[k] = true
		}
		seen2[t] = true
		for i := 0; i < t.NumField(); i++ {
			if !refFreeWalk(t.Field(i).Type, seen2) {
				return false
			}
		}
	case reflect.Array:
		seen2 := make(map[reflect.Type]bool, len(seen)+1)
		for k := range seen {
			seen2[k] = true
		}
		seen2[t] = true
		return refFreeWalk(t.Elem(), seen2)
	}
	return true
}

// skelScratch returns the stream's skeleton render scratch, built on
// first use.
func (e *codecEncoder) skelScratch() *skeletonScratch {
	if e.skel == nil {
		e.skel = newSkeletonScratch()
	}
	return e.skel
}

func newCodecEncoder() *codecEncoder {
	e := &codecEncoder{
		w:           wire.NewWriter(),
		types:       make(map[reflect.Type]typeEntry),
		ptrs:        make(map[uintptr]pinnedID),
		maps:        make(map[uintptr]pinnedID),
		coderTags:   make(map[reflect.Type]uint64),
		coderCache:  make(map[reflect.Type]*typeCoder),
		structDescs: make(map[reflect.Type]*wire.Desc),
		classIx:     make(map[groupClass]*classIndex),
		slotIx:      make(map[slotKey]int32),
		hostCache:   make(map[hostKey]int32),
	}
	e.lim = effEncodeLimits(Limits{})
	return e
}

// groupClass partitions backing groups by element size and blob-ness:
// windows of different classes never share a region.
type groupClass struct {
	es   uintptr
	blob bool
}

// classIndex locates the components of one class for slot joining: live
// (fresh) components as one array ordered by the immutable creation key
// with O(1) tail append and O(log k) search, emitted components in
// emission order. Removal from the live array is a tombstone mark in
// place — dead entries keep the array order and are dropped by
// amortized compaction — so emission never shifts the array.
type classIndex struct {
	envs      []intEntry // live-component entries, ordered by creation key
	emitted   []int32    // emitted component indices in emission order
	deadCount int
	emit      emitSkip // origin-ordered index over the emitted set
}

// intEntry is one component entry in the class live array. The ordering
// key is the component's creation origin — immutable for the component's
// lifetime: bridge merges extend a live component's bounds
// leftward past other entries and closed (emitted) regions break
// end-monotonicity; live bounds are always read from the component
// itself. A dead entry is a tombstone: the scans skip it, compaction
// drops it.
type intEntry struct {
	ci   int32
	dead bool
}

// skipMaxLevel bounds the skiplist tower height: component counts stay
// far below 2^20, so a taller tower never materializes.
const skipMaxLevel = 20

// skipNode is one emitted component in the origin-ordered skiplist.
// Each level l carries the forward link and the segment aggregate
// maxEnd[l]: the maximum end over the nodes of [self, fwd[l]) — the
// whole segment, so a stabbing query discards candidate-free segments
// in one comparison. Emitted spans may overlap (representable-overlap
// leftovers emit fresh records over frozen ones), so ends are not
// monotone and the aggregate is the only sound early-out.
type skipNode struct {
	ci     int32
	origin uintptr
	end    uintptr
	fwd    []int32
	maxEnd []uintptr
}

// emitSkip is the per-class index over emitted components keyed by
// origin. Node 0 is the head sentinel (origin 0, end 0, all levels);
// real nodes are appended in emission order. Tower heights come from a
// deterministic splitmix hash of the arena index — no runtime
// randomness, equal insert sequences build equal structures.
type emitSkip struct {
	nodes []skipNode
	top   int
}

func skipLevel(ci int32) int {
	h := uint64(ci) + 0x9E3779B97F4A7C15
	h ^= h >> 30
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 27
	h *= 0x94D049BB133111EB
	h ^= h >> 31
	lvl := 1
	for h&1 == 0 && lvl < skipMaxLevel {
		lvl++
		h >>= 1
	}
	return lvl
}

// insert adds one frozen component to the index, rewiring forward links
// bottom-up and recomputing the touched segment aggregates.
func (s *emitSkip) insert(a *scanArena, ci int32) {
	if len(s.nodes) == 0 {
		s.nodes = make([]skipNode, 1, 16)
		s.nodes[0].fwd = []int32{-1}
		s.nodes[0].maxEnd = []uintptr{0}
		s.top = 1
	}
	c := &a.comps[ci]
	lvl := skipLevel(ci)
	if lvl > s.top {
		nf := make([]int32, lvl)
		copy(nf, s.nodes[0].fwd)
		for i := s.top; i < lvl; i++ {
			nf[i] = -1
		}
		nm := make([]uintptr, lvl)
		copy(nm, s.nodes[0].maxEnd)
		s.nodes[0].fwd = nf
		s.nodes[0].maxEnd = nm
		s.top = lvl
	}
	var update [skipMaxLevel]int32
	cur := int32(0)
	for l := s.top - 1; l >= 0; l-- {
		for {
			nx := s.nodes[cur].fwd[l]
			if nx < 0 || s.nodes[nx].origin > c.origin {
				break
			}
			cur = nx
		}
		update[l] = cur
	}
	idx := int32(len(s.nodes))
	s.nodes = append(s.nodes, skipNode{
		ci: ci, origin: c.origin, end: c.end,
		fwd: make([]int32, lvl), maxEnd: make([]uintptr, lvl),
	})
	for l := range lvl {
		old := s.nodes[update[l]].fwd[l]
		s.nodes[idx].fwd[l] = old
		s.nodes[update[l]].fwd[l] = idx
		var m uintptr
		if l == 0 {
			m = c.end
		} else {
			m = s.nodes[idx].maxEnd[l-1]
			for y := s.nodes[idx].fwd[l-1]; y >= 0 && y != old; y = s.nodes[y].fwd[l-1] {
				if v := s.nodes[y].maxEnd[l-1]; v > m {
					m = v
				}
			}
		}
		s.nodes[idx].maxEnd[l] = m
		var m2 uintptr
		if l == 0 {
			m2 = s.nodes[update[0]].end
		} else {
			for y := update[l]; y != idx; y = s.nodes[y].fwd[l-1] {
				if v := s.nodes[y].maxEnd[l-1]; v > m2 {
					m2 = v
				}
			}
		}
		s.nodes[update[l]].maxEnd[l] = m2
	}
	for l := lvl; l < s.top; l++ {
		if c.end > s.nodes[update[l]].maxEnd[l] {
			s.nodes[update[l]].maxEnd[l] = c.end
		}
	}
}

// segReport appends every node of the level-l segment starting at x
// whose end covers send. The segment lies entirely inside the query
// range (all its nodes have origin <= p); segments whose aggregate
// maxEnd < send are discarded wholesale.
func (s *emitSkip) segReport(e *codecEncoder, x int32, lvl int, send uintptr, out *[]int32) {
	e.workProbes++
	if lvl == 0 {
		if s.nodes[x].end >= send {
			*out = append(*out, s.nodes[x].ci)
		}
		return
	}
	if s.nodes[x].maxEnd[lvl] < send {
		return
	}
	for y := x; y != s.nodes[x].fwd[lvl]; y = s.nodes[y].fwd[lvl-1] {
		s.segReport(e, y, lvl-1, send, out)
	}
}

// emitQuery appends the arena indices of every emitted component whose
// frozen span contains the window [p, send): origin <= p and end >=
// send. The walk decomposes the origin prefix [first, stop) into whole
// level segments — candidate-free ones cost one probe — and drops a
// level at the range tail, so the probe count is logarithmic in the
// emitted size plus the candidate count.
func (s *emitSkip) emitQuery(e *codecEncoder, p, send uintptr, out *[]int32) {
	if len(s.nodes) <= 1 {
		return
	}
	// Shrink empty top levels: towers only grow on insert, and a high
	// early tower leaves dead levels that would cost a probe each.
	for s.top > 1 && s.nodes[0].fwd[s.top-1] < 0 {
		s.top--
	}
	stop := int32(math.MaxInt32)
	cur := int32(0)
	for lvl := s.top - 1; lvl >= 0; lvl-- {
		for {
			nx := s.nodes[cur].fwd[lvl]
			if nx < 0 {
				break
			}
			e.workProbes++
			if s.nodes[nx].origin > p {
				stop = nx
				break
			}
			cur = nx
		}
	}
	x := s.nodes[0].fwd[0]
	for x != stop {
		// Greedy maximal jump: the highest level whose whole segment
		// [x, fwd) stays inside the range; candidate-free segments are
		// skipped by their maxEnd aggregate in one probe.
		lvl := len(s.nodes[x].fwd) - 1
		if lvl >= s.top {
			lvl = s.top - 1
		}
		for lvl > 0 {
			nx := s.nodes[x].fwd[lvl]
			e.workProbes++
			if nx >= 0 && s.nodes[nx].origin <= p {
				break
			}
			lvl--
		}
		nx := s.nodes[x].fwd[lvl]
		e.workProbes++
		if lvl == 0 && (nx < 0 || s.nodes[nx].origin > p) {
			// The node itself is the last one inside the range.
			if s.nodes[x].end >= send {
				*out = append(*out, s.nodes[x].ci)
			}
			return
		}
		if lvl > 0 && s.nodes[x].maxEnd[lvl] < send {
			x = nx
			continue
		}
		if lvl == 0 {
			if s.nodes[x].end >= send {
				*out = append(*out, s.nodes[x].ci)
			}
			x = nx
			continue
		}
		s.segReport(e, x, lvl, send, out)
		x = nx
	}
}

// hostKey is the cross-generation identity of one slot window: the
// exact (pointer, len, cap) geometry within its class. Cached host
// resolutions are keyed by the full window; narrower keys lose the
// first-fit-by-sequence join outcome over overlapping frozen spans of
// different widths.
type hostKey struct {
	es   uintptr
	ptr  uintptr
	n, c int
	blob bool
}

// envComp is one envelope component: the union span [origin, end) of its
// member-slot chain. An emitted component is frozen — its record bytes
// are on the wire, so origin, end, id, and elided never change again;
// subsequent slots join it only through the representability guard, without
// chain growth. Bridge merges never involve an emitted component.
type envComp struct {
	origin, end uintptr
	es          uintptr
	blob        bool
	head, tail  int32 // member chain bounds (arena slot indices)
	seq         int32 // creation sequence: the first-fit order of slot joining
	emitted     bool
	absorbed    bool // merged into another component: tombstoned in every index
	id          uint64
	elided      uint64  // E snapshot at emission time: the record's dense prefix
	sortKey     uintptr // creation origin: immutable live-array order key
}

// dead reports whether the live-array entry of the component is a
// tombstone (emitted or absorbed).
func (c *envComp) dead() bool { return c.emitted || c.absorbed }

// envLowerSortKey returns the first live-array position whose component
// creation key is >= k (the array is ordered by the immutable key).
func (ix *classIndex) envLowerSortKey(a *scanArena, k uintptr) int {
	return sort.Search(len(ix.envs), func(i int) bool { return a.comps[ix.envs[i].ci].sortKey >= k })
}

// envLowerCounted is the scan-path binary search over the live array,
// charging one work unit per probe comparison.
func (e *codecEncoder) envLowerCounted(ix *classIndex, a *scanArena, k uintptr) int {
	lo, hi := 0, len(ix.envs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		e.workProbes++
		if a.comps[ix.envs[mid].ci].sortKey < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// envNoteDead tombstones the live-array entry of ci and runs amortized
// compaction once dead entries outnumber live ones.
func (ix *classIndex) envNoteDead(a *scanArena, ci int32) {
	for i := ix.envLowerSortKey(a, a.comps[ci].sortKey); i < len(ix.envs); i++ {
		e := &ix.envs[i]
		if a.comps[e.ci].sortKey != a.comps[ci].sortKey {
			break
		}
		if e.ci == ci && !e.dead {
			e.dead = true
			ix.deadCount++
			break
		}
	}
	if ix.deadCount*2 > len(ix.envs) {
		ix.compact(a)
	}
}

// compact filters tombstoned entries out in place, preserving the order;
// live entries stay more than half the array.
func (ix *classIndex) compact(a *scanArena) {
	out := ix.envs[:0]
	for _, e := range ix.envs {
		if !a.comps[e.ci].dead() {
			out = append(out, e)
		}
	}
	clear(ix.envs[len(out):])
	ix.envs = out
	ix.deadCount = 0
}

// scanArena is the intrusive grouping arena: slot occurrences and
// envelope components in flat arrays reused across marshals (epoch reset
// in resetForPool; capacity bounded by the watermark). The scan phase
// allocates nothing per slot.
type scanArena struct {
	sVal      []reflect.Value // member slot values (zeroed on reset: no object retention)
	sPtr      []uintptr
	sGen      []uint64
	sNext     []int32 // intrusive member chain link; -1 terminates
	comps     []envComp
	cand      []int32  // per-insert candidate scratch
	emitProbe []int32  // emitted-search candidate scratch
	mem       []int32  // emission-time member chain scratch
	win       []memWin // emission-time member window scratch
}

// arenaSlotWatermark and arenaCompWatermark bound the arena capacity
// retained across marshals: capacity above the peak working set of a
// large ordinary encode is released on reset, so a rare giant encode
// cannot pin its arena forever. The bounds sit above the slot/component
// peaks of the sharing-heavy workloads (a lower bound would force the
// arena to regrow from scratch in every such marshal — measured on the
// tree-sharing benchmark line). These are internal codec constants,
// not Limits fields: the arena is working memory, never charged to
// MaxBytes (the output budget).
const (
	arenaSlotWatermark = 1 << 16
	arenaCompWatermark = 1 << 14
)

// reset returns the arena to fresh-stream state: member values released
// (no object-graph retention between marshals), lengths zeroed,
// capacity kept up to the watermark.
func (a *scanArena) reset() {
	full := a.sVal[:cap(a.sVal)]
	for i := range full {
		full[i] = reflect.Value{}
	}
	a.sVal = a.sVal[:0]
	if cap(a.sVal) > arenaSlotWatermark {
		a.sVal = make([]reflect.Value, 0, arenaSlotWatermark)
	}
	a.sPtr = a.sPtr[:0]
	if cap(a.sPtr) > arenaSlotWatermark {
		a.sPtr = make([]uintptr, 0, arenaSlotWatermark)
	}
	a.sGen = a.sGen[:0]
	if cap(a.sGen) > arenaSlotWatermark {
		a.sGen = make([]uint64, 0, arenaSlotWatermark)
	}
	a.sNext = a.sNext[:0]
	if cap(a.sNext) > arenaSlotWatermark {
		a.sNext = make([]int32, 0, arenaSlotWatermark)
	}
	a.comps = a.comps[:0]
	if cap(a.comps) > arenaCompWatermark {
		a.comps = make([]envComp, 0, arenaCompWatermark)
	}
	a.cand = a.cand[:0]
	a.emitProbe = a.emitProbe[:0]
	a.win = a.win[:0]
}

// slotKey is the emission-resolution key of one slot occurrence: the
// generation-scoped observable window identity (groupOf).
type slotKey struct {
	gen  uint64
	ptr  uintptr
	n, c int
	blob bool
}

// classIndexOf returns (creating on first use) the class index for a class.
func (e *codecEncoder) classIndexOf(c groupClass) *classIndex {
	ix, ok := e.classIx[c]
	if !ok {
		ix = &classIndex{}
		e.classIx[c] = ix
	}
	return ix
}

// setLimits replaces the effective encode budgets wholesale.
func (e *codecEncoder) setLimits(l Limits) { e.lim = effEncodeLimits(l) }

// bytesOut is the stream's output byte count since the stream start
// (header excluded): flushed bytes plus the buffered tail.
func (e *codecEncoder) bytesOut() int {
	return e.flushed + len(e.w.Bytes()) - e.streamStart
}

// coderFor resolves the coder precedence for t: built-in time.Time and
// *big.Int > RegisterCoder > Binary adapter > Text adapter; nil = derived
// path. The built-in BIGINT kind outranks the automatic text adapter of
// math/big (encode precedence): the canonical
// encoding of a big integer is the BIGINT kind, never the adapter STRING.
func (e *codecEncoder) coderFor(t reflect.Type) *typeCoder {
	if tc, ok := e.coderCache[t]; ok {
		return tc
	}
	var tc *typeCoder
	switch {
	case t == timeType:
		tc = &typeCoder{ck: ckTime}
	case isBigintType(t):
		tc = &typeCoder{ck: ckBigint}
	case e.coders[t] != nil:
		tc = &typeCoder{ck: ckCustom, c: e.coders[t]}
	default:
		if ak := adapterOf(t); ak != ckNone {
			tc = &typeCoder{ck: ak}
		}
	}
	e.coderCache[t] = tc
	return tc
}

// coderTag assigns the per-stream coder tag in first-dispatch order:
// bytes depend on the value, not on RegisterCoder call order.
func (e *codecEncoder) coderTag(t reflect.Type) uint64 {
	if tag, ok := e.coderTags[t]; ok {
		return tag
	}
	tag := uint64(len(e.coderTags))
	e.coderTags[t] = tag
	return tag
}

func (e *codecEncoder) activeCoder(t reflect.Type) bool { return e.activeCodrs[t] }

func (e *codecEncoder) setActiveCoder(t reflect.Type, on bool) {
	if e.activeCodrs == nil {
		e.activeCodrs = make(map[reflect.Type]bool)
	}
	if on {
		e.activeCodrs[t] = true
	} else {
		delete(e.activeCodrs, t)
	}
}

// facade returns the public Encoder handed to Coder callbacks; lazily built
// for the stateless Marshal path, where custom coders are unreachable.
func (e *codecEncoder) facade() *Encoder {
	if e.fac == nil {
		e.fac = &Encoder{enc: e}
	}
	return e.fac
}

// encodeSub is the coder-callback Encode path: merges the sub-value's scan
// into the live groups (topology tracking) and never resets stream
// state; the nil-any root keeps its interface-descriptor form.
func (e *codecEncoder) encodeSub(v any) error {
	if v == nil {
		if err := e.writeDescOf(anyType, pathNode{idx: -1}); err != nil {
			return err
		}
		return e.w.WriteNil(wire.NilInterface)
	}
	if err := e.scanValue(reflect.ValueOf(v)); err != nil {
		return err
	}
	return e.encodeValue(reflect.ValueOf(v), pathNode{idx: -1})
}

var anyType = reflect.TypeFor[any]()

// codecMarshal encodes one self-contained stream: header + value.
func codecMarshal(v any) ([]byte, error) {
	return codecMarshalScoped(v, nil)
}

// encoderPool recycles stateless-Marshal encoders (intern tables,
// scratch, output buffer) across calls; the streaming Encoder facade is
// stream-lifetime state and never enters the pool.
var encoderPool = sync.Pool{New: func() any { return newCodecEncoder() }}

// codecMarshalScoped encodes one self-contained stream under the coder
// scope and effective budgets of src (nil: a fresh default encoder). The
// map tie-break sub-marshal must fail only where the parent encode would
// fail — budget windows and coder reach mirror the parent stream. The
// encoder comes from the pool and returns to it on every exit; the
// result bytes are detached (owned by the caller), so the pooled buffer
// can be reused by the next call.
func codecMarshalScoped(v any, src *codecEncoder) ([]byte, error) {
	e := encoderPool.Get().(*codecEncoder)
	e.resetForPool(src)
	defer encoderPool.Put(e)
	if err := e.w.WriteHeader(); err != nil {
		return nil, err
	}
	e.streamStart = len(e.w.Bytes())
	e.growForRoot(v)
	if err := e.encodeRoot(v); err != nil {
		return nil, err
	}
	b := e.w.Bytes()
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// resetForPool returns the encoder to fresh-stream state: intern tables
// cleared (buckets retained), counters zeroed, buffer kept up to the
// Writer cap bound, coder scope and budgets inherited from src when
// present.
func (e *codecEncoder) resetForPool(src *codecEncoder) {
	if src != nil {
		e.coders = src.coders
		e.lim = src.lim
	} else {
		e.coders = nil
		e.lim = effEncodeLimits(Limits{})
	}
	clear(e.types)
	clear(e.ptrs)
	clear(e.maps)
	clear(e.coderTags)
	clear(e.coderCache)
	clear(e.structDescs)
	clear(e.classIx)
	clear(e.slotIx)
	clear(e.hostCache)
	e.hostHits = 0
	e.hostMisses = 0
	clear(e.refFree)
	e.arena.reset()
	e.started = false
	e.broken = nil
	e.inCoder = 0
	e.depth = 0
	e.nodes = 0
	e.flushed = 0
	e.streamStart = 0
	e.gen = 0
	e.gseq = 0
	e.workProbes = 0
	e.workElems = 0
	if len(e.hdrVisits) > hdrVisitWatermark {
		e.hdrVisits = nil
	} else {
		clear(e.hdrVisits)
	}
	e.hdrEp = 0
	e.activeCodrs = nil
	e.fac = nil
	e.asName = nil
	e.asType = nil
	e.w.Reset()
}

// encodeRoot handles the top-level position: a nil any encodes as the
// interface descriptor plus the nil-interface token. The slice grouping
// pre-pass is stream-lifetime state: group tables bind live memory
// geometry across every value of the stream (per-stream intern space,
// intern-space), and each encoded value opens a fresh slot
// generation for emission resolution.
func (e *codecEncoder) encodeRoot(v any) error {
	if v == nil {
		if err := e.writeDescOf(anyType, pathNode{idx: -1}); err != nil {
			return err
		}
		return e.w.WriteNil(wire.NilInterface)
	}
	e.gen++
	if err := e.scanValue(reflect.ValueOf(v)); err != nil {
		return err
	}
	return e.encodeValue(reflect.ValueOf(v), pathNode{idx: -1})
}

// liftEncodeWire raises an encode-path wire-layer error (writer-side
// validation, e.g. a descriptor name rebound) into the core; errors
// already in the core pass through unchanged.
func liftEncodeWire(err error, path string) error {
	if err == nil {
		return nil
	}
	if ae, ok := errors.AsType[*Error](err); ok {
		return ae
	}
	if we, ok := errors.AsType[*wire.Error](err); ok {
		if classSentinels[we.Kind] == ErrUnsupported {
			return errUnsupported(we.Kind, path, we.Got, we.Want, errDetail(we.Msg))
		}
		return errFormat(we.Kind, -1, path, we.Got, we.Want, errDetail(we.Msg))
	}
	return err
}

// Encode appends one value to a multi-value stream (Encoder facade). After
// a first error the encoder is invalid: every subsequent call fails without
// writing (sticky-error).
func (e *codecEncoder) Encode(v any) error {
	if e.broken != nil {
		return e.broken
	}
	if e.inCoder > 0 {
		if err := e.encodeSub(v); err != nil {
			err = liftEncodeWire(err, "")
			e.broken = err
			return err
		}
		return nil
	}
	if !e.started {
		if err := e.w.WriteHeader(); err != nil {
			return err
		}
		e.started = true
		e.streamStart = len(e.w.Bytes())
	}
	e.growForRoot(v)
	if err := e.encodeRoot(v); err != nil {
		err = liftEncodeWire(err, "")
		e.broken = err
		return err
	}
	return nil
}

// FlushTo drains the buffered stream bytes into w, preserving intern state.
func (e *codecEncoder) FlushTo(w io.Writer) error {
	if e.broken != nil {
		return e.broken
	}
	e.flushed += len(e.w.Bytes()) - e.streamStart
	e.streamStart = 0
	return e.w.FlushTo(w)
}

// encodeValue writes the type-ref position followed by the value body.
func (e *codecEncoder) encodeValue(v reflect.Value, p pathNode) error {
	if err := e.writeDescOf(v.Type(), p); err != nil {
		return err
	}
	return e.encodeBody(v, p)
}

// writeDescOf emits DESC literal once per reflect.Type per stream, REF on
// repeat. The emitted id is learned by peeking Writer.NextID before
// the literal write (WriteDesc allocates its id first).
func (e *codecEncoder) writeDescOf(t reflect.Type, p pathNode) error {
	if ent, hit := e.types[t]; hit {
		if ent.ok {
			return e.w.WriteRef(ent.id)
		}
		return e.w.WriteDesc(ent.d)
	}
	d, err := e.coderBuildDesc(t, p.String(), make(map[reflect.Type]*wire.Desc))
	if err != nil {
		return err
	}
	id := e.w.NextID()
	if err := e.w.WriteDesc(d); err != nil {
		// the wire layer rejects a name rebound to a structurally
		// different type (ambiguous type names) — an encode-time
		// unsupported, never a self-inconsistent stream
		return liftEncodeWire(err, p.String())
	}
	ent := typeEntry{d: d}
	if e.w.NextID() > id {
		ent.id, ent.ok = id, true
	}
	e.types[t] = ent
	return nil
}

// wireNaming exposes the RegisterAs bindings of this encoder scope to the
// descriptor walk and coder leaves; nil when no bindings exist, so the
// default nameOf derivation stays untouched.
func (e *codecEncoder) wireNaming() nameOverride {
	return namingOf(e.asName)
}

// structFor returns the struct field descriptor for t, built once per
// stream per type through the coder-leaf dispatch (coderBuildDesc), so
// coder-covered field types become CODER leaves matching the bodies
// encodeBody emits. Errors are not cached: a failed walk is
// re-attempted per occurrence, so each reports its own path. The cache
// carries no synchronization — per-stream state is single-goroutine
// (see Encoder).
func (e *codecEncoder) structFor(t reflect.Type, p pathNode) (*wire.Desc, error) {
	if d, ok := e.structDescs[t]; ok {
		return d, nil
	}
	d, err := e.coderBuildDesc(t, p.String(), make(map[reflect.Type]*wire.Desc))
	if err != nil {
		return nil, err
	}
	e.structDescs[t] = d
	return d, nil
}

// encodeBody guards the output budgets around the value-body write: one
// output node per call, one recursion frame per call, and the stream's
// output byte count re-checked after the write. A budget breach is an
// ErrBudget error and breaks the stream like any encoder error (sticky
// via broken; the partial buffer is never flushed).
func (e *codecEncoder) encodeBody(v reflect.Value, p pathNode) error {
	e.nodes++
	if e.nodes > e.lim.MaxNodes {
		return errBudget(classBudgetNodes, -1, p.String(), nil, e.lim.MaxNodes, errDetail(fmt.Sprintf("output nodes exceed MaxNodes budget %d", e.lim.MaxNodes)))
	}
	e.depth++
	if e.depth > e.lim.MaxDepth {
		e.depth--
		return errBudget(classBudgetDepth, -1, p.String(), nil, e.lim.MaxDepth, errDetail(fmt.Sprintf("output depth exceeds MaxDepth budget %d", e.lim.MaxDepth)))
	}
	err := e.encodeBodyInner(v, p)
	e.depth--
	if err != nil {
		return err
	}
	if e.bytesOut() > e.lim.MaxBytes {
		return errBudget(classBudgetBytes, -1, p.String(), nil, e.lim.MaxBytes, errDetail(fmt.Sprintf("output bytes exceed MaxBytes budget %d", e.lim.MaxBytes)))
	}
	return nil
}

// encodeBodyInner writes the value-body tokens for v (opcode table). Descriptor
// emission already happened at the enclosing type-ref position.
func (e *codecEncoder) encodeBodyInner(v reflect.Value, p pathNode) error {
	t := v.Type()
	if tc := e.coderFor(t); tc != nil {
		return tc.encode(e, v, p)
	}
	switch t.Kind() {
	case reflect.Bool:
		return e.w.WriteBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return e.w.WriteInt(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return e.w.WriteUint(v.Uint())
	case reflect.Float32:
		return e.w.WriteFloat32(float32(v.Float()))
	case reflect.Float64:
		return e.w.WriteFloat64(v.Float())
	case reflect.Complex64:
		return e.w.WriteComplex64(complex64(v.Complex()))
	case reflect.Complex128:
		return e.w.WriteComplex128(v.Complex())
	case reflect.String:
		return e.w.WriteString(v.String())
	case reflect.Slice:
		if isByteSliceBase(t) {
			return e.encodeBlob(v, p)
		}
		return e.encodeSlice(v, p)
	case reflect.Array:
		return e.encodeArray(v, p)
	case reflect.Map:
		return e.encodeMap(v, p)
	case reflect.Struct:
		if err := e.w.WriteStructHeader(); err != nil {
			return err
		}
		d, err := e.structFor(t, p)
		if err != nil {
			return err
		}
		for _, f := range d.Fields {
			fn := pathNode{parent: &p, name: f.Name, idx: -1}
			if err := e.encodeBody(v.Field(f.Idx), fn); err != nil {
				return err
			}
		}
		return nil
	case reflect.Pointer:
		return e.encodePointer(v, p)
	case reflect.Interface:
		if v.IsNil() {
			return e.w.WriteNil(wire.NilInterface)
		}
		dv := v.Elem()
		if err := e.writeDescOf(dv.Type(), p); err != nil {
			return err
		}
		return e.encodeBody(dv, p)
	default:
		return unsupportedAt("kind "+t.Kind().String(), p.String())
	}
}

// denseBytes returns the dense prefix E: last non-zero byte + 1.
func denseBytes(b []byte) uint64 {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return uint64(n)
}

// isBitZero reports whether v is bitwise zero: the elision predicate.
// Unlike reflect IsZero, a negative zero float — and any
// aggregate whose only content is negative zeros — is NOT elision fodder:
// −0.0 materializes as an element (bit-for-bit promise).
// BIGINT projections are wire-zero when the integer is nil or zero — the two
// encode to the same body byte, so both elide (bit-level).
func isBitZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Float32:
		return math.Float32bits(float32(v.Float())) == 0
	case reflect.Float64:
		return math.Float64bits(v.Float()) == 0
	case reflect.Complex64:
		c := v.Complex()
		return math.Float32bits(float32(real(c))) == 0 && math.Float32bits(float32(imag(c))) == 0
	case reflect.Complex128:
		c := v.Complex()
		return math.Float64bits(real(c)) == 0 && math.Float64bits(imag(c)) == 0
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !isBitZero(v.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if !isBitZero(v.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Pointer:
		// *big.Int: nil and zero encode to the same one-byte body
		// so both are elision fodder; a non-nil pointer to
		// zero is invisible to reflect IsZero.
		if v.Type() == bigIntPtrType {
			b, _ := v.Interface().(*big.Int)
			return b == nil || b.Sign() == 0
		}
		return v.IsZero()
	default:
		return v.IsZero()
	}
}

// densePrefix returns the dense prefix E over the slice elements.
func densePrefix(v reflect.Value) uint64 {
	n := v.Len()
	for n > 0 && isBitZero(v.Index(n-1)) {
		n--
	}
	return uint64(n)
}

// scanValue walks the value graph collecting slice/blob slots (the scan
// stage of the grouping pre-pass). Only elements within len are traversed as values; capacity tails are
// read as raw memory via reslice-to-cap. visited mirrors the
// reference-kind encounter rule so cycles terminate. The header half of
// the visited state is the pooled epoch table on the encoder: each scan
// opens a fresh epoch, so no header entry survives into this scan's
// decisions.
func (e *codecEncoder) scanValue(v reflect.Value) error {
	e.hdrEp++
	return e.scan(v, newVisitedSet())
}

// visitedSet is the scan-visited state for pointer and map addresses:
// they hash a scalar address (8 bytes) and are kept scan-local on this
// struct. The kind separation is structural — a pointee address may
// equal a slice data pointer (p := &s[0]), so pointers and maps never
// share a key space with slice headers (the header half lives on the
// encoder as an epoch table), while pointers and maps do (two live heap
// objects — pointee and map header — cannot share an address). The set
// is created per scanValue call, never stored beyond it, and holds no
// object references (uintptr keys are identity tokens valid only while
// the caller's value graph keeps every node reachable). The set starts
// as a single inline slot and materializes its map only when a second
// distinct key arrives: a scan meeting one pointer (or map) allocates
// no map at all.
type visitedSet struct {
	scalar     map[uintptr]struct{}
	scalar0    uintptr
	scalar0Set bool
}

// hdrKey is the observable identity of a slice header: two headers with
// an equal (data, len, cap) triple are observably indistinguishable.
type hdrKey struct {
	ptr      uintptr
	len, cap int
}

func newVisitedSet() *visitedSet {
	return &visitedSet{}
}

// visitScalar records a pointer/map address and reports whether it was
// already visited.
func (vs *visitedSet) visitScalar(a uintptr) bool {
	if vs.scalar == nil {
		if !vs.scalar0Set {
			vs.scalar0 = a
			vs.scalar0Set = true
			return false
		}
		if vs.scalar0 == a {
			return true
		}
		vs.scalar = make(map[uintptr]struct{}, 2)
		vs.scalar[vs.scalar0] = struct{}{}
		vs.scalar0Set = false
	}
	if _, ok := vs.scalar[a]; ok {
		return true
	}
	vs.scalar[a] = struct{}{}
	return false
}

// hdrVisitWatermark bounds the header table retained across marshals
// (the header analogue of the arena watermarks): a table grown past it
// by a rare giant scan is dropped on pool reset instead of cleared, so
// it cannot pin working memory disproportionate to the peak of an
// ordinary encode. Internal codec constant, not a Limits field: the
// table is scan working memory, never charged to MaxBytes.
const hdrVisitWatermark = 1 << 16

// visitHdr records a slice header triple against the current scan epoch
// and reports whether it was already visited in this scan. The header
// table is pooled working memory on the encoder: entries persist across
// marshals tagged with the epoch of their last visit, so a key from an
// earlier scan — or an earlier pool life — compares unequal to the
// current epoch and is indistinguishable from an absent one. The table
// is neither rebuilt nor reallocated per marshal; keys are value triples
// with no references, so the retained capacity pins no object graph.
func (e *codecEncoder) visitHdr(k hdrKey) bool {
	if e.hdrVisits[k] == e.hdrEp {
		return true
	}
	if e.hdrVisits == nil {
		e.hdrVisits = make(map[hdrKey]uint64, 1)
	}
	e.hdrVisits[k] = e.hdrEp
	return false
}

func (e *codecEncoder) scan(v reflect.Value, visited *visitedSet) error {
	e.depth++
	if e.depth > e.lim.MaxDepth {
		e.depth--
		return errBudget(classBudgetDepth, -1, "", nil, e.lim.MaxDepth, errDetail(fmt.Sprintf("output depth exceeds MaxDepth budget %d", e.lim.MaxDepth)))
	}
	err := e.scanInner(v, visited)
	e.depth--
	return err
}

// scanElemKind reports whether an element kind can hold scan-relevant
// references (slice/blob slots, pointers, maps, interfaces). Elements of
// other kinds — numerics, bool, string — carry no reference graph, so the
// element recursion of the scan pass skips them wholesale.
func scanElemKind(k reflect.Kind) bool {
	switch k {
	case reflect.Slice, reflect.Array, reflect.Struct, reflect.Map,
		reflect.Pointer, reflect.Interface:
		return true
	}
	return false
}

func (e *codecEncoder) scanInner(v reflect.Value, visited *visitedSet) error {
	if e.coderFor(v.Type()) != nil {
		return nil // coder values are opaque leaves
	}
	switch v.Kind() {
	case reflect.Slice:
		if !v.IsNil() {
			if err := e.scanAddSlot(v, isByteSliceBase(v.Type())); err != nil {
				return err
			}
			if e.visitHdr(hdrKey{ptr: v.Pointer(), len: v.Len(), cap: v.Cap()}) {
				return nil
			}
			if scanElemKind(v.Type().Elem().Kind()) {
				for i := 0; i < v.Len(); i++ {
					if err := e.scan(v.Index(i), visited); err != nil {
						return err
					}
				}
			}
		}
	case reflect.Array:
		if scanElemKind(v.Type().Elem().Kind()) {
			for i := 0; i < v.Len(); i++ {
				if err := e.scan(v.Index(i), visited); err != nil {
					return err
				}
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := e.scan(v.Field(i), visited); err != nil {
				return err
			}
		}
	case reflect.Map:
		if visited.visitScalar(v.Pointer()) {
			return nil
		}
		// Reference-free pairs (e.g. map[string]string) hold nothing the
		// scan tracks — no backing slots, no pointer/map identities — so
		// the per-entry iteration copies are pure overhead.
		if e.refFreeType(v.Type().Key()) && e.refFreeType(v.Type().Elem()) {
			return nil
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := e.scan(iter.Key(), visited); err != nil {
				return err
			}
			if err := e.scan(iter.Value(), visited); err != nil {
				return err
			}
		}
	case reflect.Pointer:
		if !v.IsNil() && v.Type().Elem().Size() != 0 {
			if visited.visitScalar(v.Pointer()) {
				return nil
			}
			if err := e.scan(v.Elem(), visited); err != nil {
				return err
			}
		}
	case reflect.Interface:
		if !v.IsNil() {
			if err := e.scan(v.Elem(), visited); err != nil {
				return err
			}
		}
	}
	return nil
}

// representable is the guard-on-join test against an emitted component:
// the slot's whole cap-window [ptr, ptr+cap·es) must lie
// inside the frozen record region [origin, origin+L·es], and every element
// of its len-window at record index ≥ elided (the record's implicit zero
// tail [E, L)) must be bitwise zero in the live memory — otherwise the
// current window sees data the closed record cannot supply, and the slot
// falls back to a fresh record (the sole body-duplication carve-out).
// The element loop starts past the record's dense prefix: elements at
// record index < elided are inside the recorded prefix and are never
// inspected.
func (e *codecEncoder) representable(c *envComp, v reflect.Value, p uintptr) bool {
	send := p + uintptr(v.Cap())*c.es
	e.workProbes++
	if p < c.origin || send > c.end {
		return false
	}
	off := uint64((p - c.origin) / c.es)
	start := uint64(0)
	if c.elided > off {
		start = c.elided - off
	}
	for i, n := start, uint64(v.Len()); i < n; i++ {
		e.workElems++
		if !isBitZero(v.Index(int(i))) {
			return false
		}
	}
	return true
}

// scanAddSlot records one slot and joins it into a component when the
// windows share one allocation region (one region per component). The
// overlap test is symmetric interval intersection — a window pointing
// below the component origin still shares memory with a back-looking
// member window — with equal pointers as the empty-window special case.
// A slot bridging several components merges them (chained windows form
// one region); components of different element size or blob-ness never
// merge. Zero-size elements are never registered: every non-nil
// []Z(zero-size) shares the zerobase address, and component length
// arithmetic divides by es — such slots stay fresh-owner without
// grouping. An emitted (closed) component is frozen: a slot joins it
// only through the representability guard, in encounter order (first
// fit over creation sequence); bridge merges never involve an emitted
// component, so a representable-overlap leftover stays a separate fresh
// record. Stream-lifetime components make all non-nil empty slices (cap=0, ptr=zerobase)
// of one element size collapse into one zerobase component per stream —
// unobservable, since cap=0 excludes mutation without reallocation.
//
// Group location goes through the per-class envelope index: live
// components form one origin-sorted disjoint envelope array (O(1) tail
// append for monotonically advancing inserts, O(log k) binary search
// otherwise), emitted components stay in emission order. The join
// outcome is identical to the linear first-fit: candidates order by
// creation sequence, the first becomes the join target, subsequent fresh
// candidates bridge into it; each merge absorbs a component at most
// once, so run merges total ≤ n−1 for the whole stream.
func (e *codecEncoder) scanAddSlot(v reflect.Value, blob bool) error {
	es := v.Type().Elem().Size()
	if es == 0 {
		return nil
	}
	p := v.Pointer()
	send := p + uintptr(v.Cap())*es
	ix := e.classIndexOf(groupClass{es: es, blob: blob})
	// Live-candidate search over the creation-key-ordered array:
	// tombstones are skipped in place; live spans live on the component.
	// Covering candidates (origin < p < end) sit left of the key split
	// point and are collected by a backward scan that stops at the first
	// live entry ending at or before p — live intervals are ordered, so
	// nothing further left can cover the window. Entries at or after the
	// split are scanned forward with the baseline overlap filter (the
	// p == origin arm is the equal-origin join of the linear first-fit,
	// covering empty zero-cap reslice envelopes (w[:0:0] keeps the data
	// pointer of w; a fresh x[k:k:k] expression may have its pointer
	// sanitized by the compiler) invisible to strict intersection).
	// The tail probe keeps the monotone insert shape (windows advancing
	// past the last live envelope) at O(1) with no search at all; a last
	// envelope that is empty with p exactly at its origin falls through
	// to the full search — the equal-origin arm must see it.
	a := &e.arena
	cand := a.cand[:0]
	// Emitted-host resolution. The host cache keys the exact window
	// geometry across generations and stores the minimal-sequence
	// containing component — a frozen fact (spans never change, subsequent
	// components only get higher sequence numbers), so a hit is stable
	// as long as the representable check still passes; a mutated zero
	// tail fails that check and falls through to the full search.
	hkey := hostKey{es: es, ptr: p, n: v.Len(), c: v.Cap(), blob: blob}
	emitCi := int32(-1)
	if ci, ok := e.hostCache[hkey]; ok {
		if e.representable(&a.comps[ci], v, p) {
			emitCi = ci
			e.hostHits++
		}
	}
	if emitCi < 0 {
		e.hostMisses++
		probe := a.emitProbe[:0]
		ix.emit.emitQuery(e, p, send, &probe)
		if len(probe) > 0 {
			slices.SortFunc(probe, func(x, y int32) int {
				switch {
				case a.comps[x].seq < a.comps[y].seq:
					return -1
				case a.comps[x].seq > a.comps[y].seq:
					return 1
				}
				return 0
			})
			e.hostCache[hkey] = probe[0]
			for _, ci := range probe {
				if e.representable(&a.comps[ci], v, p) {
					emitCi = ci
					break
				}
			}
		} else {
			delete(e.hostCache, hkey)
		}
		a.emitProbe = probe
	}
	if emitCi >= 0 {
		cand = append(cand, emitCi)
	}
	lo := 0
	if n := len(ix.envs); n > 0 {
		last := &a.comps[ix.envs[n-1].ci]
		e.workProbes++ // tail probe
		if !last.dead() && p >= last.end && p > last.origin {
			lo = n
		} else {
			lo = e.envLowerCounted(ix, a, p)
			for i := lo - 1; i >= 0; i-- {
				c := &a.comps[ix.envs[i].ci]
				e.workProbes++ // backward neighbor probe
				if ix.envs[i].dead || c.dead() {
					continue
				}
				if p >= c.end {
					break
				}
				cand = append(cand, ix.envs[i].ci)
			}
			for i := lo; i < n; i++ {
				c := &a.comps[ix.envs[i].ci]
				e.workProbes++ // forward neighbor probe
				if ix.envs[i].dead || c.dead() {
					continue
				}
				if c.origin >= send {
					break
				}
				if p == c.origin || p < c.end {
					cand = append(cand, ix.envs[i].ci)
				}
			}
		}
	}
	a.cand = cand
	var into int32 = -1
	if len(cand) > 0 {
		if len(cand) > 1 {
			slices.SortFunc(cand, func(x, y int32) int {
				switch {
				case a.comps[x].seq < a.comps[y].seq:
					return -1
				case a.comps[x].seq > a.comps[y].seq:
					return 1
				}
				return 0
			})
		}
		into = cand[0]
		for _, ci := range cand[1:] {
			if a.comps[into].emitted || a.comps[ci].emitted {
				continue
			}
			e.mergeInto(into, ci)
		}
	}
	key := slotKey{gen: e.gen, ptr: p, n: v.Len(), c: v.Cap(), blob: blob}
	if into >= 0 {
		c := &a.comps[into]
		if c.emitted {
			// Representable join of a closed component: the record is
			// frozen, the member chain never grows — only the slot
			// resolution is observable (a view token at emission).
			e.slotIx[key] = into
			return nil
		}
		if p < c.origin {
			c.origin = p
		}
		if send > c.end {
			c.end = send
		}
		si := e.arenaAppendSlot(v, p)
		a.sNext[c.tail] = si
		c.tail = si
		e.slotIx[key] = into
		return nil
	}
	si := e.arenaAppendSlot(v, p)
	ci := int32(len(a.comps))
	a.comps = append(a.comps, envComp{
		origin: p, end: send, es: es, blob: blob,
		head: si, tail: si, seq: e.gseq,
		sortKey: p,
	})
	e.gseq++
	e.slotIx[key] = ci
	// Insert at the tail position when appending past the last live
	// envelope (the fast path already computed it), otherwise at the
	// creation-key split point — the key order holds either way.
	if lo == len(ix.envs) {
		ix.envs = append(ix.envs, intEntry{ci: ci})
		return nil
	}
	ix.envs = append(ix.envs, intEntry{})
	copy(ix.envs[lo+1:], ix.envs[lo:])
	ix.envs[lo] = intEntry{ci: ci}
	return nil
}

// arenaAppendSlot stores one member slot in the arena and returns its
// index; the intrusive chain link starts terminated.
func (e *codecEncoder) arenaAppendSlot(v reflect.Value, p uintptr) int32 {
	a := &e.arena
	si := int32(len(a.sVal))
	a.sVal = append(a.sVal, v)
	a.sPtr = append(a.sPtr, p)
	a.sGen = append(a.sGen, e.gen)
	a.sNext = append(a.sNext, -1)
	return si
}

// mergeInto absorbs component g into component into (bridge merge, both
// fresh): the member chain of g appends at into's tail keeping the
// encounter order, the envelope grows to the union span, and every
// absorbed member's slot resolution re-points to into. The absorbed
// component's live-array entry becomes a tombstone (amortized
// compaction, no array shift).
func (e *codecEncoder) mergeInto(into, g int32) {
	a := &e.arena
	ci, cg := &a.comps[into], &a.comps[g]
	a.sNext[ci.tail] = cg.head
	ci.tail = cg.tail
	if cg.origin < ci.origin {
		ci.origin = cg.origin
	}
	if cg.end > ci.end {
		ci.end = cg.end
	}
	cg.absorbed = true
	ix := e.classIndexOf(groupClass{es: cg.es, blob: cg.blob})
	ix.envNoteDead(a, g)
	for m := cg.head; m >= 0; m = a.sNext[m] {
		e.slotIx[slotKey{gen: a.sGen[m], ptr: a.sPtr[m], n: a.sVal[m].Len(), c: a.sVal[m].Cap(), blob: cg.blob}] = into
	}
}

// markEmitted tombstones the component's live-array entry and moves it
// to the emitted list at the moment its record bytes are committed to
// the stream; the live array itself never shifts — dead entries drop by
// amortized compaction.
func (e *codecEncoder) markEmitted(ci int32) {
	a := &e.arena
	c := &a.comps[ci]
	ix := e.classIndexOf(groupClass{es: c.es, blob: c.blob})
	ix.envNoteDead(a, ci)
	ix.emitted = append(ix.emitted, ci)
	ix.emit.insert(a, ci)
}

// groupOf returns the component the slot of v actually joined on the
// scan pass of the current value: slot identity is the (generation,
// ptr, len, cap) window key. A pointer match against a foreign slot of
// a closed component is NOT a resolution — a guard-rejected window over
// the same memory must resolve to its own fresh component, never to a
// view over the stale record (the elided-tail mutation trap).
func (e *codecEncoder) groupOf(v reflect.Value, blob bool) int32 {
	ci, ok := e.slotIx[slotKey{gen: e.gen, ptr: v.Pointer(), n: v.Len(), c: v.Cap(), blob: blob}]
	if !ok {
		return -1
	}
	return ci
}

// compMembers collects the component's member chain into buf (encounter
// order) and returns it.
func (a *scanArena) compMembers(ci int32, buf []int32) []int32 {
	buf = buf[:0]
	for m := a.comps[ci].head; m >= 0; m = a.sNext[m] {
		buf = append(buf, m)
	}
	return buf
}

// covering returns the member whose window contains index i, so element i of
// the union can be read through it (reslice within cap).
func (e *codecEncoder) covering(c *envComp, i uint64) int32 {
	a := &e.arena
	for m := c.head; m >= 0; m = a.sNext[m] {
		off := uint64((a.sPtr[m] - c.origin) / c.es)
		if i >= off && i < off+uint64(a.sVal[m].Cap()) {
			return m
		}
	}
	return -1
}

// elemAt reads union element i through a covering member window.
func (e *codecEncoder) elemAt(c *envComp, i uint64) reflect.Value {
	a := &e.arena
	m := e.covering(c, i)
	off := int(i - uint64((a.sPtr[m]-c.origin)/c.es))
	return a.sVal[m].Slice(off, off+1).Index(0)
}

// length returns L in elements.
func (c *envComp) length() uint64 { return uint64((c.end - c.origin) / c.es) }

// denseE returns E: one past the last non-bitwise-zero element of [0, L)
// (elision predicate).
func (e *codecEncoder) denseE(c *envComp) uint64 {
	for i := c.length(); i > 0; i-- {
		if !isBitZero(e.elemAt(c, i-1)) {
			return i
		}
	}
	return 0
}

// writeGroupRecord emits the component ARRAY record with elements
// [0, E); E is snapshotted into elided at emission — subsequent guard checks
// read this snapshot, never the mutated live prefix. Elements of
// primitive kinds go through a batch loop: the kind dispatch happens
// once per record, not per element.
func (e *codecEncoder) writeGroupRecord(ci int32, p pathNode) error {
	a := &e.arena
	c := &a.comps[ci]
	L := c.length()
	E := e.denseE(c)
	c.emitted = true
	e.markEmitted(ci)
	c.id = e.w.NextID()
	c.elided = E
	if err := e.w.WriteArrayHeader(L, E); err != nil {
		return err
	}
	if primBatchKind(a.sVal[c.head].Type().Elem().Kind()) != reflect.Invalid {
		return e.writeGroupElems(ci, E, p)
	}
	// Non-primitive elements go through one resliced window per member:
	// per-element Value.Slice allocates (reflect's GC-visibility header),
	// Index over a fixed window does not. The window list is copied out
	// of the arena scratch: the recursive encodeBody below re-enters
	// emission for nested groups and would clobber it.
	wins := append([]memWin(nil), e.compWindows(ci, E)...)
	for i := range E {
		for len(wins) > 1 && i >= wins[0].off+wins[0].n {
			wins = wins[1:]
		}
		en := pathNode{parent: &p, idx: int(i)}
		if err := e.encodeBody(wins[0].val.Index(int(i-wins[0].off)), en); err != nil {
			return err
		}
	}
	return nil
}

// writeGroupElems writes E primitive elements through the member windows
// in index order. Element sources never overlap observably: windows over
// the same memory read the same bytes regardless of the covering member.
func (e *codecEncoder) writeGroupElems(ci int32, E uint64, p pathNode) error {
	if E == 0 {
		return nil
	}
	if e.depth+1 > e.lim.MaxDepth {
		en := pathNode{parent: &p, idx: 0}
		return errBudget(classBudgetDepth, -1, en.String(), nil, e.lim.MaxDepth, errDetail(fmt.Sprintf("output depth exceeds MaxDepth budget %d", e.lim.MaxDepth)))
	}
	a := &e.arena
	wins := e.compWindows(ci, E)
	k := primBatchKind(a.sVal[a.comps[ci].head].Type().Elem().Kind())
	w := e.w
	for i := range E {
		for len(wins) > 1 && i >= wins[0].off+wins[0].n {
			wins = wins[1:]
		}
		v := wins[0].val.Index(int(i - wins[0].off))
		e.nodes++
		if e.nodes > e.lim.MaxNodes {
			en := pathNode{parent: &p, idx: int(i)}
			return errBudget(classBudgetNodes, -1, en.String(), nil, e.lim.MaxNodes, errDetail(fmt.Sprintf("output nodes exceed MaxNodes budget %d", e.lim.MaxNodes)))
		}
		if err := writePrim(w, k, v); err != nil {
			return err
		}
		if e.bytesOut() > e.lim.MaxBytes {
			en := pathNode{parent: &p, idx: int(i)}
			return errBudget(classBudgetBytes, -1, en.String(), nil, e.lim.MaxBytes, errDetail(fmt.Sprintf("output bytes exceed MaxBytes budget %d", e.lim.MaxBytes)))
		}
	}
	return nil
}

// memWin is one member window resliced for direct element access: val
// spans [0, n) of the union index space starting at off.
type memWin struct {
	off uint64
	n   uint64
	val reflect.Value
}

// compWindows returns the member windows of component ci covering [0, E),
// resliced to their cap span and ordered by offset, with overlaps
// collapsed to the first window in member order. The window scratch
// lives in the arena: emission allocates nothing per member.
func (e *codecEncoder) compWindows(ci int32, E uint64) []memWin {
	a := &e.arena
	c := &a.comps[ci]
	if c.head == c.tail {
		m := c.head
		n := min(uint64(a.sVal[m].Cap()), E)
		a.win = a.win[:0]
		a.win = append(a.win, memWin{off: 0, n: n, val: a.sVal[m].Slice(0, int(n))})
		return a.win
	}
	members := a.compMembers(ci, a.mem)
	a.win = a.win[:0]
	for _, m := range members {
		off := uint64((a.sPtr[m] - c.origin) / c.es)
		n := uint64(a.sVal[m].Cap())
		if off >= E {
			continue
		}
		if off+n > E {
			n = E - off
		}
		a.win = append(a.win, memWin{off: off, n: n, val: a.sVal[m].Slice(0, int(n))})
	}
	slices.SortFunc(a.win, func(x, y memWin) int {
		switch {
		case x.off < y.off:
			return -1
		case x.off > y.off:
			return 1
		}
		return 0
	})
	out := a.win[:0]
	var covered uint64
	for _, wv := range a.win {
		end := wv.off + wv.n
		if end <= covered {
			continue
		}
		out = append(out, wv)
		covered = end
	}
	return out
}

// primBatchKind maps an element kind to the batch-write dispatch kind;
// reflect.Invalid marks kinds without a batch path.
func primBatchKind(k reflect.Kind) reflect.Kind {
	switch k {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128:
		return k
	}
	return reflect.Invalid
}

// writePrim writes one primitive value body without the encodeBody frame.
func writePrim(w *wire.Writer, k reflect.Kind, v reflect.Value) error {
	switch k {
	case reflect.Bool:
		return w.WriteBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return w.WriteInt(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return w.WriteUint(v.Uint())
	case reflect.Float32:
		return w.WriteFloat32(float32(v.Float()))
	case reflect.Float64:
		return w.WriteFloat64(v.Float())
	case reflect.Complex64:
		return w.WriteComplex64(complex64(v.Complex()))
	case reflect.Complex128:
		return w.WriteComplex128(v.Complex())
	}
	return errUnsupported(classUnsupportedKind, "", nil, nil, errDetail(fmt.Sprintf("no primitive batch path for kind %s", k)))
}

// writeView emits the slot's view token with its exact window geometry,
// minimal form first: form0 whole-array, form1 cap=len, form2 full.
func (e *codecEncoder) writeView(c *envComp, v reflect.Value) error {
	off := uint64((v.Pointer() - c.origin) / c.es)
	length := uint64(v.Len())
	capacity := uint64(v.Cap())
	L := c.length()
	switch {
	case off == 0 && length == capacity && length == L:
		return e.w.WriteViewFull(c.id)
	case capacity == length:
		return e.w.WriteView(c.id, off, length)
	default:
		return e.w.WriteView3(c.id, off, length, capacity)
	}
}

// encodeBlob writes a []byte position: the component BLOB record on
// first encounter, then the view token. es==0 never applies to blobs.
func (e *codecEncoder) encodeBlob(v reflect.Value, p pathNode) error {
	if v.IsNil() {
		return e.w.WriteNil(wire.NilSlice)
	}
	ci := e.groupOf(v, true)
	if ci < 0 {
		return e.encodeFreshBlob(v)
	}
	a := &e.arena
	c := &a.comps[ci]
	if !c.emitted {
		L := c.length()
		buf := make([]byte, L)
		for m := c.head; m >= 0; m = a.sNext[m] {
			off := uint64((a.sPtr[m] - c.origin) / c.es)
			n := uint64(a.sVal[m].Cap())
			if off+n > L {
				n = L - off
			}
			copy(buf[off:], a.sVal[m].Slice(0, int(n)).Bytes())
		}
		E := denseBytes(buf)
		c.emitted = true
		e.markEmitted(ci)
		c.id = e.w.NextID()
		c.elided = E
		if err := e.w.WriteBlobHeader(L, E); err != nil {
			return err
		}
		if err := e.w.WriteRawBytes(buf[:E]); err != nil {
			return err
		}
	}
	return e.writeView(c, v)
}

// encodeFreshBlob is the no-group fallback: single-slot BLOB record.
func (e *codecEncoder) encodeFreshBlob(v reflect.Value) error {
	b := v.Bytes()
	L := uint64(v.Cap())
	E := denseBytes(b)
	id := e.w.NextID()
	if err := e.w.WriteBlobHeader(L, E); err != nil {
		return err
	}
	if err := e.w.WriteRawBytes(b[:E]); err != nil {
		return err
	}
	if uint64(v.Len()) == L {
		return e.w.WriteViewFull(id)
	}
	return e.w.WriteView3(id, 0, uint64(v.Len()), L)
}

// encodeSlice writes a []T position: the group ARRAY record on first
// encounter, then the view token. Zero-size element slices are
// fresh-owner by construction (no observable data to share).
func (e *codecEncoder) encodeSlice(v reflect.Value, p pathNode) error {
	if v.IsNil() {
		return e.w.WriteNil(wire.NilSlice)
	}
	ci := e.groupOf(v, false)
	if ci < 0 {
		return e.encodeFreshSlice(v, p)
	}
	if !e.arena.comps[ci].emitted {
		if err := e.writeGroupRecord(ci, p); err != nil {
			return err
		}
	}
	return e.writeView(&e.arena.comps[ci], v)
}

// writePrimElems writes E elements of a slice/array value of a batchable
// primitive kind: one kind dispatch for the whole run, per-element budget
// semantics identical to the per-element encodeBody frame.
func (e *codecEncoder) writePrimElems(v reflect.Value, E uint64, p pathNode) error {
	if E == 0 {
		return nil
	}
	if e.depth+1 > e.lim.MaxDepth {
		en := pathNode{parent: &p, idx: 0}
		return errBudget(classBudgetDepth, -1, en.String(), nil, e.lim.MaxDepth, errDetail(fmt.Sprintf("output depth exceeds MaxDepth budget %d", e.lim.MaxDepth)))
	}
	k := primBatchKind(v.Type().Elem().Kind())
	w := e.w
	for i := range E {
		el := v.Index(int(i))
		e.nodes++
		if e.nodes > e.lim.MaxNodes {
			en := pathNode{parent: &p, idx: int(i)}
			return errBudget(classBudgetNodes, -1, en.String(), nil, e.lim.MaxNodes, errDetail(fmt.Sprintf("output nodes exceed MaxNodes budget %d", e.lim.MaxNodes)))
		}
		if err := writePrim(w, k, el); err != nil {
			return err
		}
		if e.bytesOut() > e.lim.MaxBytes {
			en := pathNode{parent: &p, idx: int(i)}
			return errBudget(classBudgetBytes, -1, en.String(), nil, e.lim.MaxBytes, errDetail(fmt.Sprintf("output bytes exceed MaxBytes budget %d", e.lim.MaxBytes)))
		}
	}
	return nil
}

// encodeFreshSlice is the no-group fallback (es==0): one ARRAY record of
// length cap with empty dense prefix, plus the exact view.
func (e *codecEncoder) encodeFreshSlice(v reflect.Value, p pathNode) error {
	L := uint64(v.Cap())
	E := densePrefix(v)
	id := e.w.NextID()
	if err := e.w.WriteArrayHeader(L, E); err != nil {
		return err
	}
	if primBatchKind(v.Type().Elem().Kind()) != reflect.Invalid {
		if err := e.writePrimElems(v, E, p); err != nil {
			return err
		}
	} else {
		for i := range E {
			en := pathNode{parent: &p, idx: int(i)}
			if err := e.encodeBody(v.Index(int(i)), en); err != nil {
				return err
			}
		}
	}
	length := uint64(v.Len())
	if length == L {
		return e.w.WriteViewFull(id)
	}
	return e.w.WriteView3(id, 0, length, L)
}

// encodeArray writes an [N]T body: one ARRAY record, no view, value
// semantics.
func (e *codecEncoder) encodeArray(v reflect.Value, p pathNode) error {
	L := uint64(v.Len())
	E := densePrefix(v)
	if err := e.w.WriteArrayHeader(L, E); err != nil {
		return err
	}
	if primBatchKind(v.Type().Elem().Kind()) != reflect.Invalid {
		return e.writePrimElems(v, E, p)
	}
	for i := range E {
		en := pathNode{parent: &p, idx: int(i)}
		if err := e.encodeBody(v.Index(int(i)), en); err != nil {
			return err
		}
	}
	return nil
}

// encodeMap writes a map body: an intern-space record (the id lands at the
// header, DFS preorder like array/blob records; repeated encounters emit a
// REF, preserving map identity through the round trip) whose pairs follow in
// canonical order: primary sort by key skeleton
// (literal canonical bytes), tie-break by pair value bytes, final tie-break
// by the full DFS sequence of pointer-component addresses of the key for a
// total deterministic order. Key
// emission itself is the regular body encoding, so pointer keys intern in
// the unified object space.
func (e *codecEncoder) encodeMap(v reflect.Value, p pathNode) error {
	if v.IsNil() {
		return e.w.WriteNil(wire.NilMap)
	}
	kt := v.Type().Key()
	if !comparableKeyKind(kt.Kind()) {
		return unsupportedAt("map key type "+nameOf(kt), p.String())
	}
	if e.coderFor(kt) != nil && !isBigintType(kt) {
		return unsupportedAt("coder-coded map key type "+nameOf(kt)+" (no canonical key bytes)", p.String())
	}
	if ent, hit := e.maps[v.Pointer()]; hit {
		return e.w.WriteRef(ent.id)
	}
	if kt == stringType && v.Type().Elem() == stringType && e.coderFor(v.Type().Elem()) == nil {
		return e.encodeStringMap(v, p)
	}
	if kt == stringType && v.Type().Elem() == byteSliceType && e.coderFor(v.Type().Elem()) == nil {
		return e.encodeBytesMap(v, p)
	}
	// Deterministic pair order is a three-phase tie-break over the key
	// categories: skeleton bytes, then value bytes, then the pointer
	// sequence. The value-bytes phase runs a full scoped sub-marshal per
	// pair it reaches (only pairs with equal skeletons get compared), so
	// structurally equal pointer keys cost O(n log n·|V|) marshal work —
	// a documented cost of canonical map encoding. A failing sub-marshal
	// is not swallowed: the error (wrapped with the pair path) fails the
	// encode before the map header is written.
	type pair struct {
		skel    []byte
		k       reflect.Value
		val     reflect.Value
		valb    []byte
		valset  bool
		lazy    bool
		ptrseq  []uintptr
		lazySeq bool
		err     error
	}
	valBytes := func(pr *pair) []byte {
		if !pr.lazy {
			pr.lazy = true
			if b, err := codecMarshalScoped(pr.val.Interface(), e); err == nil {
				pr.valb, pr.valset = b, true
			} else {
				pr.err = err
			}
		}
		return pr.valb
	}
	pairs := make([]pair, 0, v.Len())
	i := 0
	for iter := v.MapRange(); iter.Next(); {
		k := iter.Key()
		if k.Kind() == reflect.Interface && !k.IsNil() && e.coderFor(k.Elem().Type()) != nil && !isBigintType(k.Elem().Type()) {
			return unsupportedAt("coder-coded dynamic map key type "+nameOf(k.Elem().Type()), p.String())
		}
		pn := pathNode{parent: &p, idx: i}
		skel, err := skeletonKeyBytes(k, pn, e.skelScratch())
		if err != nil {
			return err
		}
		pairs = append(pairs, pair{skel: skel, k: k, val: iter.Value()})
		i++
	}
	keySeq := func(p *pair) []uintptr {
		if !p.lazySeq {
			p.lazySeq = true
			p.ptrseq = keyPtrSeq(p.k)
		}
		return p.ptrseq
	}
	idx := make([]int32, len(pairs))
	for j := range idx {
		idx[j] = int32(j)
	}
	slices.SortFunc(idx, func(a, b int32) int {
		pa, pb := &pairs[a], &pairs[b]
		if c := bytes.Compare(pa.skel, pb.skel); c != 0 {
			return c
		}
		if c := bytes.Compare(valBytes(pa), valBytes(pb)); c != 0 {
			return c
		}
		if keyPtrSeqLess(keySeq(pa), keySeq(pb)) {
			return -1
		}
		return 1
	})
	for j := range pairs {
		if pairs[j].err != nil {
			return pairs[j].err
		}
	}
	// Intern record: register the id the header will take, so value
	// positions inside the pairs REF this map mid-fill (record-then-fill,
	// record semantics).
	e.maps[v.Pointer()] = pinnedID{id: e.w.NextID(), v: v}
	if err := e.w.WriteMapHeader(uint64(len(pairs))); err != nil {
		return err
	}
	for _, j := range idx {
		pn := pathNode{parent: &p, idx: int(j)}
		if err := e.encodeBody(pairs[j].k, pn); err != nil {
			return err
		}
		if err := e.encodeBody(pairs[j].val, pn); err != nil {
			return err
		}
	}
	return nil
}

// skeletonKeyBytes returns the literal canonical encoding of a map key of
// any comparable category: struct fields in descriptor order
// (blank excluded, KO-6), array elements ascending, pointers by pointee
// skeleton; no interning — byte-equal skeletons mean byte-equal skeletons
// regardless of object identity (pointer-free category detector).
// stringType pins the reflect type of the typed map paths.
var stringType = reflect.TypeFor[string]()

// strPair is one pair of the typed map[string]string path. form and n
// cache the key's ARG form and length at collection time, so the sort
// comparator runs on plain ints (the form is a function of the length,
// so the cached triple orders exactly like cmpLenPrefix).
type strPair struct {
	k, v string
	form byte
	n    int
}

// byteSliceType pins the reflect type of the typed map[string][]byte path.
var byteSliceType = reflect.TypeFor[[]byte]()

// bytesPair is one pair of the typed map[string][]byte path; the sort
// comparator is the shared cmpLenPrefix (the same total order the
// string map path and the generic skeleton render produce).
type bytesPair struct {
	k string
	v []byte
}

// encodeStringMap is the typed map[string]string body path: native range
// collection over the asserted map (no per-pair reflect boxing),
// skeleton-order sort without byte rendering, direct string writes.
// Budget semantics mirror the per-pair encodeBody frames of the generic
// path (two nodes and two MaxBytes re-checks per pair). Equal string
// keys cannot occur in a Go map, so the skeleton order is total and the
// value/pointer tie-break phases are unreachable.
func (e *codecEncoder) encodeStringMap(v reflect.Value, p pathNode) error {
	m := v.Interface().(map[string]string)
	ptr := v.Pointer()
	pairs := make([]strPair, 0, len(m))
	for k, val := range m {
		pairs = append(pairs, strPair{k: k, v: val, form: wire.ArgForm(uint64(len(k))), n: len(k)})
	}
	slices.SortFunc(pairs, func(a, b strPair) int {
		if a.form != b.form {
			if a.form < b.form {
				return -1
			}
			return 1
		}
		if a.n != b.n {
			if a.n < b.n {
				return -1
			}
			return 1
		}
		return strings.Compare(a.k, b.k)
	})
	if e.depth+1 > e.lim.MaxDepth {
		pn := pathNode{parent: &p, idx: 0}
		return errBudget(classBudgetDepth, -1, pn.String(), nil, e.lim.MaxDepth, errDetail(fmt.Sprintf("output depth exceeds MaxDepth budget %d", e.lim.MaxDepth)))
	}
	e.maps[ptr] = pinnedID{id: e.w.NextID(), v: v}
	if err := e.w.WriteMapHeader(uint64(len(pairs))); err != nil {
		return err
	}
	for i := range pairs {
		pn := pathNode{parent: &p, idx: i}
		e.nodes++
		if e.nodes > e.lim.MaxNodes {
			return errBudget(classBudgetNodes, -1, pn.String(), nil, e.lim.MaxNodes, errDetail(fmt.Sprintf("output nodes exceed MaxNodes budget %d", e.lim.MaxNodes)))
		}
		if err := e.w.WriteString(pairs[i].k); err != nil {
			return err
		}
		if e.bytesOut() > e.lim.MaxBytes {
			return errBudget(classBudgetBytes, -1, pn.String(), nil, e.lim.MaxBytes, errDetail(fmt.Sprintf("output bytes exceed MaxBytes budget %d", e.lim.MaxBytes)))
		}
		e.nodes++
		if e.nodes > e.lim.MaxNodes {
			return errBudget(classBudgetNodes, -1, pn.String(), nil, e.lim.MaxNodes, errDetail(fmt.Sprintf("output nodes exceed MaxNodes budget %d", e.lim.MaxNodes)))
		}
		if err := e.w.WriteString(pairs[i].v); err != nil {
			return err
		}
		if e.bytesOut() > e.lim.MaxBytes {
			return errBudget(classBudgetBytes, -1, pn.String(), nil, e.lim.MaxBytes, errDetail(fmt.Sprintf("output bytes exceed MaxBytes budget %d", e.lim.MaxBytes)))
		}
	}
	return nil
}

// encodeBytesMap is the typed map[string][]byte body path: native range
// collection over the asserted map (no per-pair reflect boxing), the same
// skeleton-order sort as encodeStringMap, direct string key writes, and
// values through the common encodeBody blob path (record-then-fill groups,
// REF on repeats, slice-backing sharing — same bytes as the generic path).
// Budget semantics mirror the per-pair frames of encodeStringMap for the
// key slot; value charges come from encodeBody like the generic emission.
// Equal string keys cannot occur in a Go map, so the value/pointer
// tie-break phases are unreachable here as well.
func (e *codecEncoder) encodeBytesMap(v reflect.Value, p pathNode) error {
	m := v.Interface().(map[string][]byte)
	ptr := v.Pointer()
	pairs := make([]bytesPair, 0, len(m))
	for k, val := range m {
		pairs = append(pairs, bytesPair{k: k, v: val})
	}
	slices.SortFunc(pairs, func(a, b bytesPair) int {
		return cmpLenPrefix(a.k, b.k)
	})
	if e.depth+1 > e.lim.MaxDepth {
		pn := pathNode{parent: &p, idx: 0}
		return errBudget(classBudgetDepth, -1, pn.String(), nil, e.lim.MaxDepth, errDetail(fmt.Sprintf("output depth exceeds MaxDepth budget %d", e.lim.MaxDepth)))
	}
	e.maps[ptr] = pinnedID{id: e.w.NextID(), v: v}
	if err := e.w.WriteMapHeader(uint64(len(pairs))); err != nil {
		return err
	}
	for i := range pairs {
		pn := pathNode{parent: &p, idx: i}
		e.nodes++
		if e.nodes > e.lim.MaxNodes {
			return errBudget(classBudgetNodes, -1, pn.String(), nil, e.lim.MaxNodes, errDetail(fmt.Sprintf("output nodes exceed MaxNodes budget %d", e.lim.MaxNodes)))
		}
		if err := e.w.WriteString(pairs[i].k); err != nil {
			return err
		}
		if e.bytesOut() > e.lim.MaxBytes {
			return errBudget(classBudgetBytes, -1, pn.String(), nil, e.lim.MaxBytes, errDetail(fmt.Sprintf("output bytes exceed MaxBytes budget %d", e.lim.MaxBytes)))
		}
		if err := e.encodeBody(reflect.ValueOf(pairs[i].v), pn); err != nil {
			return err
		}
	}
	return nil
}

// cmpLenPrefix orders two strings exactly as their canonical skeleton
// bytes would — the STRING literal token: ARG-form nibble first, then
// the big-endian length payload (numeric within a form), then the raw
// bytes. Byte-equal to comparing the rendered tokens, without rendering.
func cmpLenPrefix(a, b string) int {
	fa, fb := wire.ArgForm(uint64(len(a))), wire.ArgForm(uint64(len(b)))
	if fa != fb {
		if fa < fb {
			return -1
		}
		return 1
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// skeletonScratch is the reused state of the canonical-key render: the
// intern Writer and the pointer visited-set. Reset before each key, so
// the rendered bytes equal a fresh-writer pass.
type skeletonScratch struct {
	w    *wire.Writer
	seen map[uintptr]bool
}

func newSkeletonScratch() *skeletonScratch {
	return &skeletonScratch{w: wire.NewWriter(), seen: make(map[uintptr]bool)}
}

// skeletonKeyBytes renders the literal canonical encoding of a map key; sc
// carries the scratch state (reset per call).
func skeletonKeyBytes(k reflect.Value, p pathNode, sc *skeletonScratch) ([]byte, error) {
	sc.w.Reset()
	clear(sc.seen)
	wl := &skelWalker{sc: sc}
	if err := wl.walk(k, p); err != nil {
		return nil, err
	}
	return append([]byte(nil), sc.w.Bytes()...), nil
}

// skelWalker is the skeleton render pass as a method receiver: static
// dispatch keeps the per-element path frames stack-allocated (a closure
// call would force them to the heap).
type skelWalker struct {
	sc *skeletonScratch
}

func (wl *skelWalker) walk(v reflect.Value, p pathNode) error {
	sw, seen := wl.sc.w, wl.sc.seen
	switch v.Kind() {
	case reflect.Bool:
		return sw.WriteBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return sw.WriteInt(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return sw.WriteUint(v.Uint())
	case reflect.Float32:
		if math.IsNaN(v.Float()) {
			return unsupportedAt("NaN map key component", p.String())
		}
		return sw.WriteFloat32(float32(v.Float()))
	case reflect.Float64:
		if math.IsNaN(v.Float()) {
			return unsupportedAt("NaN map key component", p.String())
		}
		return sw.WriteFloat64(v.Float())
	case reflect.Complex64, reflect.Complex128:
		c := v.Complex()
		if math.IsNaN(real(c)) || math.IsNaN(imag(c)) {
			return unsupportedAt("NaN map key component", p.String())
		}
		if v.Kind() == reflect.Complex64 {
			return sw.WriteComplex64(complex64(c))
		}
		return sw.WriteComplex128(c)
	case reflect.String:
		return sw.WriteStringLit(v.String())
	case reflect.Pointer:
		if v.Type() == bigIntPtrType {
			// BIGINT keys: the skeleton is the literal value body —
			// minimal bare/ext argument of the zigzag image (KO-6
			// over the whole integer domain).
			return sw.WriteBigint(v.Interface().(*big.Int))
		}
		if v.IsNil() {
			return sw.WriteNil(wire.NilPointer)
		}
		if seen[v.Pointer()] {
			// Cyclic key: the repeated address collapses to the nil
			// marker so the skeleton stays finite (sort-only bytes;
			// byte-set categories never contain pointers).
			return sw.WriteNil(wire.NilPointer)
		}
		seen[v.Pointer()] = true
		return wl.walk(v.Elem(), p)
	case reflect.Interface:
		if v.IsNil() {
			return sw.WriteNil(wire.NilInterface)
		}
		dv := v.Elem()
		if !comparableKeyKind(dv.Type().Kind()) {
			return unsupportedAt("unhashable map key dynamic type "+nameOf(dv.Type()), p.String())
		}
		d, err := descOf(dv.Type(), p.String())
		if err != nil {
			return err
		}
		if err := sw.WriteDesc(d); err != nil {
			return err
		}
		return wl.walk(dv, p)
	case reflect.Array:
		E := densePrefix(v)
		if err := sw.WriteArrayHeader(uint64(v.Len()), E); err != nil {
			return err
		}
		for i := range E {
			en := pathNode{parent: &p, idx: int(i)}
			if err := wl.walk(v.Index(int(i)), en); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if err := sw.WriteStructHeader(); err != nil {
			return err
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Name == "_" {
				continue // blank: excluded from ==, KO-6
			}
			if f.PkgPath != "" {
				fn := pathNode{parent: &p, name: f.Name, idx: -1}
				return unsupportedAt("unexported field "+f.Name, fn.String())
			}
			fn := pathNode{parent: &p, name: f.Name, idx: -1}
			if err := wl.walk(v.Field(i), fn); err != nil {
				return err
			}
		}
		return nil
	}
	return unsupportedAt("map key kind "+v.Kind().String(), p.String())
}

// keyPtrSeq returns the DFS sequence of pointer-component addresses of a
// comparable key — the total final tie-break: nil emits
// the canonical slot 0 (distinct from any live address), a repeated address
// (cycle) stops expansion after emitting itself, one slot per pointer DFS
// visit in skeleton order (fields ascending, blank excluded, array elements
// ascending). Distinct map keys with byte-equal skeletons and values always
// diverge in this sequence at their first differing pointer component.
func keyPtrSeq(k reflect.Value) []uintptr {
	var seq []uintptr
	seen := make(map[uintptr]bool)
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				seq = append(seq, 0)
				return
			}
			seq = append(seq, v.Pointer())
			if seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			walk(v.Elem())
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if t.Field(i).Name == "_" {
					continue // blank: excluded from key semantics (KO-6)
				}
				walk(v.Field(i))
			}
		case reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(k)
	return seq
}

// keyPtrSeqLess is the lexicographic order over keyPtrSeq sequences: element
// slots compared by value (nil slot 0 below every address), shorter prefix
// below its extension.
func keyPtrSeqLess(a, b []uintptr) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// encodePointer writes a pointer body: nil selector, REF on a repeated
// target, or a reserved id (registration before children)
// followed by the pointee body. Zero-size targets are not tracked.
func (e *codecEncoder) encodePointer(v reflect.Value, p pathNode) error {
	if v.IsNil() {
		return e.w.WriteNil(wire.NilPointer)
	}
	if v.Type().Elem().Size() != 0 {
		if ent, hit := e.ptrs[v.Pointer()]; hit {
			return e.w.WriteRef(ent.id)
		}
		e.ptrs[v.Pointer()] = pinnedID{id: e.w.ReserveID(), v: v}
	}
	return e.encodeBody(v.Elem(), p)
}

// growForRoot reserves output capacity from the root value's shape: a
// slice root reserves 3·L bytes (blob heading plus body), a string root
// its length. The estimate is a capacity hint bounded by
// growEstimateCap — never an allocation sized from attacker-controlled
// lengths without a budget ceiling.
const growEstimateCap = 1 << 26

func (e *codecEncoder) growForRoot(v any) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return
	}
	var est int
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		es := int(rv.Type().Elem().Size())
		est = rv.Len() * (es + 2)
	case reflect.String:
		est = rv.Len()
	default:
		return
	}
	if est > growEstimateCap {
		est = growEstimateCap
	}
	if est > 0 {
		e.w.Grow(est)
	}
}
