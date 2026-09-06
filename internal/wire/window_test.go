package wire

import (
	"bytes"
	"testing"
)

// Window arithmetic of the sliding buffer: the boundary table of the
// trusted-input capture — typical window, front clamp, tail clamp with
// no realignment, the empty degenerate, and the front-clamped marked
// position — plus the side-effect freedom (no source reads, no state
// change).

// winCase is one boundary row: reader buffer state (absolute base, buffered
// bytes), the absolute offset under diagnosis, and the expected window
// [start, end).
type winCase struct {
	name string
	base int
	buf  []byte
	off  int
	want []byte
	lo   int
}

func winCases() []winCase {
	pattern := func(n int) []byte { // deterministic, distinguishable bytes
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(0x20 + i%95)
		}
		return b
	}
	return []winCase{
		// typical window: buffer [0,100), offset 40 -> [8,56), len 48
		{name: "typical", base: 0, buf: pattern(100), off: 40, want: pattern(100)[8:56], lo: 8},
		// front clamp: buffer [0,20), offset 5 -> [0,20)
		{name: "front", base: 0, buf: pattern(20), off: 5, want: pattern(20)[0:20], lo: 0},
		// tail clamp, start not 16-aligned: buffer [100,164), offset 164 -> [132,164)
		{name: "tail", base: 100, buf: pattern(64), off: 164, want: pattern(64)[32:64], lo: 132},
		// zero offset: buffer [0,end), offset 0 -> [0,min(end,16))
		{name: "zero", base: 0, buf: pattern(40), off: 0, want: pattern(40)[0:16], lo: 0},
		// empty buffer: window clamps empty, start at the buffer base
		{name: "empty", base: 0, buf: nil, off: 0, want: nil, lo: 0},
		// front-clamped offset: buffer [10,50), offset 10 -> [10,26)
		{name: "offset-at-base", base: 10, buf: pattern(40), off: 10, want: pattern(40)[0:16], lo: 10},
	}
}

func TestWindowBounds(t *testing.T) {
	for _, tc := range winCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reader{buf: tc.buf, base: tc.base, pos: len(tc.buf)}
			got, lo := r.Window(tc.off, 32, 16)
			if lo != tc.lo {
				t.Fatalf("window start = %d, want %d", lo, tc.lo)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("window = %d bytes, want %d bytes (equal=%v)", len(got), len(tc.want), bytes.Equal(got, tc.want))
			}
			if len(got) > 48 {
				t.Fatalf("window length %d exceeds the 32+16 bound", len(got))
			}
		})
	}
}

// TestWindowNoSideEffects: Window neither reads the source nor disturbs
// the reader — identical window before and after a pull attempt, and a
// source whose Read would fail loudly is never touched.
func TestWindowNoSideEffects(t *testing.T) {
	src := &fatalReadReader{}
	r := NewStreamReader(src)
	r.buf = []byte("0123456789abcdefghij")
	r.base = 7
	r.pos = 3
	before := r.pos
	w1, lo1 := r.Window(15, 32, 16)
	w2, lo2 := r.Window(15, 32, 16)
	if !bytes.Equal(w1, w2) || lo1 != lo2 {
		t.Fatalf("window not stable: [%d,%q] vs [%d,%q]", lo1, w1, lo2, w2)
	}
	if r.pos != before || r.base != 7 || len(r.buf) != 20 {
		t.Fatalf("reader state disturbed: pos=%d base=%d len=%d", r.pos, r.base, len(r.buf))
	}
}

// fatalReadReader fails the test process on any read attempt.
type fatalReadReader struct{}

func (fatalReadReader) Read([]byte) (int, error) {
	panic("Window must not read the underlying source")
}
