//go:build !race

package gbon_test

// raceEnabled mirrors the race-detector build tag: heavy-deep fixtures
// that walk ~10^6 recursion frames overflow the 1GB goroutine stack in
// race builds (frames ~4x heavier) before the depth guard counter fires,
// so those legs run their configured-threshold equivalent under race.
const raceEnabled = false
