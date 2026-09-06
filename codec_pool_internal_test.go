package gbon

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"
)

// TestMarshalPoolReuse: the stateless Marshal path recycles its encoder
// through a pool — consecutive calls with different value shapes must
// produce byte-identical output to independent fresh encoders and leave
// no intern/scope/scratch residue between calls.
func TestMarshalPoolReuse(t *testing.T) {
	type inner struct {
		ID    int64
		Name  string
		Score float64
	}
	type outer struct {
		When  time.Time
		Tags  map[string]string
		Items []inner
		Next  *outer
	}
	vals := []any{
		inner{ID: 1, Name: "a", Score: 1.5},
		map[string]string{"k1": "v1", "k2": "v2"},
		[]int64{1, 2, 3, 1 << 40},
		outer{When: time.Date(2026, 8, 30, 12, 0, 0, 5, time.UTC),
			Tags:  map[string]string{"x": "y"},
			Items: []inner{{ID: 2, Name: "b", Score: math.Copysign(0, -1)}},
			Next:  nil},
		"plain string after struct with shared deps",
		[]byte{1, 2, 3, 0, 0},
	}
	var first [][]byte
	for i, v := range vals {
		b, err := codecMarshal(v)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		first = append(first, b)
	}
	// Second lap over the same values on recycled encoders: bytes stable.
	for i, v := range vals {
		b, err := codecMarshal(v)
		if err != nil {
			t.Fatalf("marshal2 %d: %v", i, err)
		}
		if !bytes.Equal(b, first[i]) {
			t.Fatalf("marshal %d: pooled bytes differ from first pass", i)
		}
		// Round-trip through the stateless decoder still holds.
		out := reflect.New(reflect.TypeOf(v))
		if err := codecUnmarshal(b, out.Interface()); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
	}

	// Cross-marshal isolation of the pooled header-epoch table: a header
	// visited by an earlier marshal on the same recycled encoder must be
	// indistinguishable from an absent one — the same value marshaled
	// right after a sharing-heavy form (its headers already visited) and
	// after an unrelated form produces identical bytes.
	shared := [][]int64{{1, 2, 3}, {4, 5}}
	if _, err := codecMarshal([]any{shared, shared}); err != nil {
		t.Fatal(err)
	}
	variantA, err := codecMarshal(shared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codecMarshal("unrelated form between shared marshals"); err != nil {
		t.Fatal(err)
	}
	if _, err := codecMarshal([]any{shared, shared}); err != nil {
		t.Fatal(err)
	}
	variantB, err := codecMarshal(shared)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(variantA, variantB) {
		t.Fatalf("shared-slice marshal differs by pool history: %x vs %x", variantA, variantB)
	}
}

// Arena hygiene probes (WF scan-prepass pool contract): (i) after
// resetForPool the arena holds no slot values and no live length —
// nothing of the encoded value survives the pool boundary; (ii) a
// marshal whose slot peak exceeded the watermark leaves the arena
// truncated back to it — a rare giant encode cannot pin its arena;
// (iii) once warmed, append+reset cycles over the retained capacity
// allocate nothing (bounded inter-call memory).
func TestArenaPoolHygiene(t *testing.T) {
	// (i) retention probe: after a shared-heavy marshal, the pooled
	// encoder (taken back — resetForPool runs on Get) carries no slot
	// values and no component length.
	base := make([]int64, 64)
	val := make([][]int64, 0, 32)
	for i := range 32 {
		val = append(val, base[i:i+2:i+2])
	}
	if _, err := codecMarshal(val); err != nil {
		t.Fatal(err)
	}
	// The pool hands the encoder out raw; the next marshal's resetForPool
	// (which codecMarshalScoped runs after Get) is the reset point.
	e := encoderPool.Get().(*codecEncoder)
	e.resetForPool(nil)
	if len(e.arena.sVal) != 0 || len(e.arena.comps) != 0 {
		t.Fatalf("arena lengths must be zero after resetForPool: slots=%d comps=%d",
			len(e.arena.sVal), len(e.arena.comps))
	}
	for i, sv := range e.arena.sVal[:cap(e.arena.sVal)] {
		if sv.IsValid() {
			t.Fatalf("retained slot value at %d: arena holds the object graph past the pool boundary", i)
		}
	}
	encoderPool.Put(e)

	// (ii) watermark truncation: a marshal with a slot peak above the
	// watermark leaves capacity truncated to it on reset.
	big := make([][]int64, arenaSlotWatermark*2+16)
	for i := range big {
		big[i] = []int64{int64(i)}
	}
	if _, err := codecMarshal(big); err != nil {
		t.Fatal(err)
	}
	e2 := encoderPool.Get().(*codecEncoder)
	e2.resetForPool(nil)
	if c := cap(e2.arena.sVal); c > arenaSlotWatermark {
		t.Fatalf("slot arena capacity %d must be truncated to the watermark %d", c, arenaSlotWatermark)
	}
	if c := cap(e2.arena.comps); c > arenaCompWatermark {
		t.Fatalf("component arena capacity %d must be truncated to the watermark %d", c, arenaCompWatermark)
	}
	encoderPool.Put(e2)

	// (iii) bounded cycles: over the retained capacity, slot append and
	// reset allocate nothing (the output path allocates independently of
	// the arena; this probe pins the arena itself).
	warm := newCodecEncoder()
	for i := range 64 {
		warm.arenaAppendSlot(reflect.ValueOf(i), 0)
	}
	warm.arena.reset()
	allocs := testing.AllocsPerRun(100, func() {
		for i := range 64 {
			warm.arenaAppendSlot(reflect.ValueOf(i), 0)
		}
		warm.arena.reset()
	})
	if allocs != 0 {
		t.Fatalf("warm arena cycles must not allocate, got %v allocs/run", allocs)
	}
}

// Header-table hygiene probes (scan visited-set pool contract, the
// header-table analogue of the arena probes above): (i) after
// resetForPool the table holds no entries and a key from the previous
// pool life is indistinguishable from an absent one at the next scan
// epoch; (ii) a table grown past the watermark is dropped — not
// cleared — on reset, so a rare giant scan cannot pin its table; (iii)
// once materialized, epoch cycles over the same key set allocate
// nothing (the per-scan visitedSet allocation is the deferred scalar
// half — this probe pins the header table alone).
func TestHdrTableHygiene(t *testing.T) {
	// (i) retention probe: after a shared-heavy marshal the pooled
	// encoder carries no header entries; the first visit under a fresh
	// epoch is not a hit, the second one is.
	base := make([]int64, 64)
	val := make([][]int64, 0, 32)
	for i := range 32 {
		val = append(val, base[i:i+2:i+2])
	}
	if _, err := codecMarshal(val); err != nil {
		t.Fatal(err)
	}
	e := encoderPool.Get().(*codecEncoder)
	e.resetForPool(nil)
	if len(e.hdrVisits) != 0 || e.hdrEp != 0 {
		t.Fatalf("header table must be empty after resetForPool: len=%d epoch=%d",
			len(e.hdrVisits), e.hdrEp)
	}
	e.hdrEp = 1
	k := hdrKey{ptr: 1, len: 1, cap: 1}
	if e.visitHdr(k) {
		t.Fatal("first visit after the pool boundary must not be a hit")
	}
	if !e.visitHdr(k) {
		t.Fatal("second visit within the epoch must be a hit")
	}
	encoderPool.Put(e)

	// (ii) watermark drop: a marshal visiting more headers than the
	// watermark leaves a nil (dropped) table on reset.
	big := make([][]int64, hdrVisitWatermark+16)
	for i := range big {
		big[i] = []int64{int64(i)}
	}
	if _, err := codecMarshal(big); err != nil {
		t.Fatal(err)
	}
	e2 := encoderPool.Get().(*codecEncoder)
	e2.resetForPool(nil)
	if e2.hdrVisits != nil {
		t.Fatalf("header table above the watermark must be dropped on reset, got len=%d", len(e2.hdrVisits))
	}
	encoderPool.Put(e2)

	// (iii) bounded cycles: epoch-cycled visits over a warmed key set
	// allocate nothing.
	src := make([][]int64, 64)
	keys := make([]hdrKey, len(src))
	for i := range src {
		src[i] = make([]int64, 1)
		rv := reflect.ValueOf(src[i])
		keys[i] = hdrKey{ptr: rv.Pointer(), len: rv.Len(), cap: rv.Cap()}
	}
	warm := newCodecEncoder()
	warm.hdrEp = 1
	for _, hk := range keys {
		warm.visitHdr(hk)
	}
	allocs := testing.AllocsPerRun(100, func() {
		warm.hdrEp++
		for _, hk := range keys {
			warm.visitHdr(hk)
		}
	})
	if allocs != 0 {
		t.Fatalf("warm header-table cycles must not allocate, got %v allocs/run", allocs)
	}
}
