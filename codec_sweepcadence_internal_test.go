package gbon

import (
	"runtime"
	"testing"
	"unsafe"
)

// TestSweepCadenceLiveTableBoundedWork: on a stream whose table only
// grows (no deaths) the cadence does no full-table work — probe work
// and sweep count stay within their bounds at any table size.
func TestSweepCadenceLiveTableBoundedWork(t *testing.T) {
	e := newCodecEncoder()
	const live1 = 100000
	const live2 = 10000
	nodes := make([]*retentionNode, 0, live1+live2)
	for range live1 {
		nodes = append(nodes, &retentionNode{Val: 1})
	}
	if err := e.Encode(nodes); err != nil {
		t.Fatalf("Encode live1: %v", err)
	}
	for range live2 {
		nodes = append(nodes, &retentionNode{Val: 2})
	}
	if err := e.Encode(nodes); err != nil {
		t.Fatalf("Encode live2: %v", err)
	}
	total := len(nodes)
	if len(e.ptrs) != total {
		t.Fatalf("live table lost entries: %d, want %d", len(e.ptrs), total)
	}
	maxProbes := int64(total/sweepProbeEvery+1) * sweepProbeSample
	if e.probeWork > maxProbes {
		t.Fatalf("probe work %d exceeds sample bound %d — full-scan probe path is back", e.probeWork, maxProbes)
	}
	maxSweeps := uint64(total/sweepMaxInserts) + 2
	if e.sweeps > maxSweeps {
		t.Fatalf("sweeps %d exceed amortized bound %d on a live-only table", e.sweeps, maxSweeps)
	}
	runtime.KeepAlive(nodes)
}

// TestSweepCadenceDeadBurstSampleSweep: a dead-majority table is caught
// by the bounded sample within one cadence window of fresh inserts —
// detection does not wait for the insert run to outgrow the table.
func TestSweepCadenceDeadBurstSampleSweep(t *testing.T) {
	e := newCodecEncoder()
	var wave [sweepMaxInserts]*retentionNode
	for i := range wave {
		wave[i] = &retentionNode{Val: i}
	}
	if err := e.Encode(wave); err != nil {
		t.Fatalf("Encode wave: %v", err)
	}
	for i := range wave {
		wave[i] = nil
	}
	runtime.GC()
	runtime.GC()

	// Half a cadence window of fresh live inserts: the first probe at
	// sweepProbeEvery inserts samples the dead-majority table and
	// sweeps; capacity would need sweepMaxInserts more inserts.
	var fresh [sweepProbeEvery]*retentionNode
	for i := range fresh {
		fresh[i] = &retentionNode{Val: -i}
	}
	if err := e.Encode(fresh); err != nil {
		t.Fatalf("Encode fresh: %v", err)
	}
	if e.sweeps < 2 {
		t.Fatalf("sample sweep did not fire on dead-majority table (sweeps=%d)", e.sweeps)
	}
	dead := 0
	for _, ent := range e.ptrs {
		if ent.wp.Value() == nil {
			dead++
		}
	}
	if dead != 0 {
		t.Fatalf("post-sweep table holds %d dead entries", dead)
	}
	if len(e.ptrs) < len(fresh) {
		t.Fatalf("fresh live entries lost: %d", len(e.ptrs))
	}
	for _, n := range fresh {
		if _, hit := e.ptrs[uintptr(unsafe.Pointer(n))]; !hit {
			t.Fatalf("fresh entry missing after sample sweep")
		}
	}
	runtime.KeepAlive(fresh)
}
