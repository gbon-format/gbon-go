package wire

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// White-box tests of the additive codec-facing helpers, each with a
// negative fixture proving the checker rejects known violations.

func TestReserveAndNextID(t *testing.T) {
	w := NewWriter()
	if w.NextID() != 0 {
		t.Fatalf("fresh writer NextID = %d", w.NextID())
	}
	if id := w.ReserveID(); id != 0 {
		t.Fatalf("ReserveID = %d", id)
	}
	if w.NextID() != 1 {
		t.Fatalf("after reserve NextID = %d", w.NextID())
	}
}

func TestWriteRawBytesAndFlushTo(t *testing.T) {
	w := NewWriter()
	_ = w.WriteHeader()
	if err := w.WriteRawBytes([]byte{0xAB, 0xCD}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := w.FlushTo(&buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), []byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x00, 0xAB, 0xCD}) {
		t.Fatalf("flushed % x", buf.Bytes())
	}
	// drain leaves an empty buffer; a second flush writes nothing
	buf.Reset()
	if err := w.FlushTo(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("second flush wrote %d bytes", buf.Len())
	}
}

func TestReadRawBytesTruncated(t *testing.T) {
	r := NewReader([]byte{0x01})
	if _, err := r.ReadRawBytes(2); !errors.Is(err, ErrFormat) {
		t.Fatalf("want ErrFormat, got %v", err)
	}
}

func TestRegisterValueValueAt(t *testing.T) {
	r := NewReader(nil)
	id := r.RegisterValue(reflect.ValueOf(42))
	if id != 0 {
		t.Fatalf("id = %d", id)
	}
	v, err := r.ValueAt(id)
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsValid() || v.Interface() != 42 {
		t.Fatalf("value round: %v", v)
	}
	// negative: unregistered id
	if _, err := r.ValueAt(7); !errors.Is(err, ErrFormat) {
		t.Fatalf("unregistered: want ErrFormat, got %v", err)
	}
	// negative: id of a non-object record
	r2 := NewReader([]byte{0x60}) // STRING literal ""
	if _, err := r2.ReadStringLit(); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ValueAt(0); !errors.Is(err, ErrFormat) {
		t.Fatalf("non-object entry: want ErrFormat, got %v", err)
	}
}

func TestInternRecordSortAt(t *testing.T) {
	// DESC literal (interface kind, empty name) registers the descriptor
	// at id 0 before its name string.
	r := NewReader([]byte{0xD6, 0x60})
	desc, err := r.ReadDesc()
	if err != nil {
		t.Fatal(err)
	}
	if desc.Kind != KindInterface {
		t.Fatalf("kind = %d, want interface", desc.Kind)
	}
	if kind, _ := r.RecordAt(0); kind != RecordDesc {
		t.Fatalf("RecordAt(0) sort = %d, want descriptor record", kind)
	}
	// negative: id of an object record
	r2 := NewReader(nil)
	if id := r2.RegisterValue(reflect.ValueOf(42)); id != 0 {
		t.Fatalf("id = %d", id)
	}
	if kind, v := r2.RecordAt(0); kind != RecordValue || v.Interface() != 42 {
		t.Fatalf("object entry: sort %d value %v, want value record 42", kind, v)
	}
	// negative: unregistered id
	r3 := NewReader(nil)
	if kind, v := r3.RecordAt(7); kind != RecordOther || v.IsValid() {
		t.Fatalf("unregistered: sort %d, want other with no value", kind)
	}
}

func TestPeekClassIsNextNil(t *testing.T) {
	r := NewReader([]byte{0x01})
	if c, err := r.PeekClass(); err != nil || c != ClassNil {
		t.Fatalf("PeekClass = %d, %v", c, err)
	}
	if !r.IsNextNil() {
		t.Fatal("IsNextNil must be true")
	}
	r3 := NewReader([]byte{0x22})
	if r3.IsNextNil() {
		t.Fatal("IsNextNil must be false for INT token")
	}
	if _, err := NewReader(nil).PeekClass(); !errors.Is(err, ErrFormat) {
		t.Fatalf("empty PeekClass: want ErrFormat, got %v", err)
	}
}

func TestMaxDescDepthGuard(t *testing.T) {
	chain := []byte{0xD5, 0x60, 0xD5, 0x60, 0xD5, 0x60, 0xD5, 0x60, 0xD5, 0x60}
	r := NewReader(chain)
	r.MaxDescDepth = 3
	if _, err := r.ReadDesc(); !errors.Is(err, ErrBudget) {
		t.Fatalf("guarded: want ErrBudget, got %v", err)
	}
	// negative control: without the cap the same chain parses (until
	// truncation), proving the guard is what rejects
	r2 := NewReader(chain)
	if _, err := r2.ReadDesc(); !errors.Is(err, ErrFormat) {
		t.Fatalf("unguarded: want truncation ErrFormat, got %v", err)
	}
}
