//go:build !gbon_emitprobe

package gbon

// emitStep is the emission-boundary probe hook: a no-op outside the
// probe build, compiled away from production and bench legs.
func (e *codecEncoder) emitStep() {}
