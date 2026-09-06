package wire

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

// Regression class: a declared record length must never size an allocation
// (allocation amplification on untrusted input). A short source serving a
// direct read declared at 2^32 costs a transient proportional to the bytes
// actually delivered, and the verdict stays truncated.

const ampProbe = "gbon\x00\x00\xd3n\xff\xffeinti"

func TestReadDirectTruncatedBoundedAlloc(t *testing.T) {
	r := NewStreamReader(bytes.NewReader([]byte(ampProbe)))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := r.readN(1 << 32)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("truncated source served a 2^32 read")
	}
	if !strings.Contains(err.Error(), "truncated body") {
		t.Fatalf("verdict changed: %v", err)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 8<<20 {
		t.Fatalf("transient allocation %d MiB for a %d-byte source", d>>20, len(ampProbe))
	}
}

func TestReadDirectServesLargeRecord(t *testing.T) {
	body := bytes.Repeat([]byte{0xA7}, 3<<20)
	r := NewStreamReader(bytes.NewReader(body))
	got, err := r.readN(uint64(len(body)))
	if err != nil {
		t.Fatalf("large record read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("large record body mismatch")
	}
}

// dribbleReader serves one byte per Read: the growth loop must assemble the
// record from arbitrarily small source pulls.
type dribbleReader struct{ b []byte }

func (d *dribbleReader) Read(p []byte) (int, error) {
	if len(d.b) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = d.b[0]
	d.b = d.b[1:]
	return 1, nil
}

func TestReadDirectDribblingSource(t *testing.T) {
	body := bytes.Repeat([]byte{0x5C}, (1<<20)+5000)
	r := NewStreamReader(&dribbleReader{b: append([]byte(nil), body...)})
	got, err := r.readN(uint64(len(body)))
	if err != nil {
		t.Fatalf("dribbling read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("dribbling body mismatch")
	}
	if _, err := r.readN(1); !errors.Is(err, io.EOF) && err == nil {
		t.Fatalf("post-record state: %v", err)
	}
}
