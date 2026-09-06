package gbon_test

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// Regression class: allocation amplification. A 15-byte committed fuzz seed
// drove the stream reader into a declared-length preallocation of ~4 GiB
// per decoder pass (trusted and default alike). Decoding the seeds must stay
// within a small allocation budget in both trust modes.
func TestDecodeAllocationAmplificationBounded(t *testing.T) {
	seeds := [][]byte{
		[]byte("gbon\x00\x00\xd3n\xff\xffeinti"),
		[]byte("gbon\x00\x00\xd3n\xff\xffeanti"),
		[]byte("gbon\x000"),
	}
	for _, seed := range seeds {
		for _, trusted := range []bool{false, true} {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			d := gbon.NewDecoder(bytes.NewReader(seed))
			d.SetTrustedInput(trusted)
			var v any
			_ = d.Decode(&v)
			runtime.ReadMemStats(&after)
			if d := after.TotalAlloc - before.TotalAlloc; d > 8<<20 {
				t.Fatalf("seed %q trusted=%v: TotalAlloc delta %d MiB", seed, trusted, d>>20)
			}
		}
	}
}
