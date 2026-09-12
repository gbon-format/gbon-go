//go:build gbon_emitprobe

package gbon

import "runtime"

// emitProbeGC forces a full collection at every emission boundary while
// armed; the soak leg arms it around encodes whose members stay pinned
// only through the arena.
var emitProbeGC bool

// emitStep is the emission-boundary probe: one call per value, batch
// element, and blob member boundary.
func (e *codecEncoder) emitStep() {
	if emitProbeGC {
		runtime.GC()
	}
}
