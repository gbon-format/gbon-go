package wire

import (
	"bytes"
	"errors"
	"fmt"
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
	if !bytes.Equal(buf.Bytes(), []byte{0x67, 0x62, 0x6F, 0x6E, 0x00, 0x01, 0xAB, 0xCD}) {
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

// TestPeekDescKind pins the header lookahead: kind forms, the literal
// name, no consumption, idempotent repeat, and rejection outcomes.
func TestPeekDescKind(t *testing.T) {
	r := NewReader([]byte{0xD6, 0x60})
	if k, name, ok := r.PeekDesc(); !ok || k != KindInterface || name != "" {
		t.Fatalf("inline form: PeekDesc = %d, %q, %v, want interface, \"\"", k, name, ok)
	}
	if r.Pos() != 0 {
		t.Fatalf("PeekDesc consumed: Pos = %d", r.Pos())
	}
	if k, _, ok := r.PeekDesc(); !ok || k != KindInterface {
		t.Fatalf("repeat: PeekDesc = %d, %v, want interface", k, ok)
	}
	if _, err := r.ReadDesc(); err != nil {
		t.Fatalf("consuming read after peek: %v", err)
	}

	// named pointer header + inline name: kind and name without consumption
	r1 := NewReader([]byte{0xD5, 0x68, '*', 'm', 'a', 'i', 'n', '.', 'S', '1'})
	if k, name, ok := r1.PeekDesc(); !ok || k != KindPointer || name != "*main.S1" {
		t.Fatalf("named form: PeekDesc = %d, %q, %v, want pointer, *main.S1", k, name, ok)
	}
	if r1.Pos() != 0 {
		t.Fatalf("named form consumed: Pos = %d", r1.Pos())
	}

	// ARG-extended form: kind 12 rides a u8 payload; u8-length name.
	r2 := NewReader([]byte{0xDC, 0x0C, 0x6C, 0x03, 'a', 'b', 'c'})
	if k, name, ok := r2.PeekDesc(); !ok || k != KindString || name != "abc" {
		t.Fatalf("arg form: PeekDesc = %d, %q, %v, want string, abc", k, name, ok)
	}
	if r2.Pos() != 0 {
		t.Fatalf("arg form consumed: Pos = %d", r2.Pos())
	}

	// negative: non-DESC classes
	for _, b := range []byte{0xC2, 0x01, 0x62} {
		if _, _, ok := NewReader([]byte{b}).PeekDesc(); ok {
			t.Fatalf("class %#02x: PeekDesc must reject", b)
		}
	}
	// negative: truncated header, truncated name token, truncated name body
	for _, b := range [][]byte{{0xDC}, {0xD5}, {0xD5, 0x6C}, {0xD5, 0x6C, 0x04, 'a'}} {
		if _, _, ok := NewReader(b).PeekDesc(); ok {
			t.Fatalf("truncated % x: PeekDesc must reject", b)
		}
	}
	// negative: header not followed by a STRING token
	if _, _, ok := NewReader([]byte{0xD5, 0xC2}).PeekDesc(); ok {
		t.Fatal("non-string name position: PeekDesc must reject")
	}
	// negative: non-minimal kind (inline-range kind via ARG) and
	// reserved kind
	for _, payload := range []byte{0x05, 0x10} {
		if _, _, ok := NewReader([]byte{0xDC, payload}).PeekDesc(); ok {
			t.Fatalf("arg payload %#02x: PeekDesc must reject", payload)
		}
	}
	// negative: unknown inline form
	if _, _, ok := NewReader([]byte{0xDD}).PeekDesc(); ok {
		t.Fatal("form 13: PeekDesc must reject")
	}
}

// TestPeekDescStreamWindow mirrors the peek contract over a live
// source: the lookahead blocks for exactly the token extent, and the
// consuming read matches the buffered-mode oracle.
func TestPeekDescStreamWindow(t *testing.T) {
	var raw []byte
	raw = append(raw, Magic...)
	raw = append(raw, Major, Minor)
	raw = append(raw, 0xD5, 0x68, '*', 'm', 'a', 'i', 'n', '.', 'S', '1') // pointer header + inline name

	sr := NewStreamReader(bytes.NewReader(raw))
	if _, _, err := sr.ReadHeader(); err != nil {
		t.Fatalf("header: %v", err)
	}
	br := NewReader(raw)
	if _, _, err := br.ReadHeader(); err != nil {
		t.Fatalf("header: %v", err)
	}
	wantK, wantName, ok := br.PeekDesc()
	if !ok || wantK != KindPointer || wantName != "*main.S1" {
		t.Fatalf("buffered PeekDesc = %d, %q, %v", wantK, wantName, ok)
	}
	gotK, gotName, ok := sr.PeekDesc()
	if !ok || gotK != wantK || gotName != wantName {
		t.Fatalf("stream PeekDesc = %d, %q, %v, want %d, %q", gotK, gotName, ok, wantK, wantName)
	}
	if sr.Pos() != br.Pos() {
		t.Fatalf("stream Pos %d drifted from buffered %d", sr.Pos(), br.Pos())
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

// TestPeekRefWindowBoundary pins the peek-vs-compaction contract at
// the largeRead boundary: no consumption, no cursor motion, idempotent
// repeat, and the consuming read matches the buffered-mode reader.
func TestPeekRefWindowBoundary(t *testing.T) {
	id := uint64(0xDEADBEEFCAFE) // variable: shifts must not fold into byte constants
	for _, shift := range []int{-1, 0, 1, 4, 8} {
		t.Run(fmt.Sprintf("shift%d", shift), func(t *testing.T) {
			pad := largeRead - 1 + shift
			var raw []byte
			raw = append(raw, Magic...)
			raw = append(raw, Major, Minor)
			for range pad {
				raw = append(raw, 0x21) // filler int token
			}
			// REF token, u64 ARG form: 9 bytes total.
			raw = append(raw, classRef<<4|0x0F)
			raw = append(raw, byte(id>>56), byte(id>>48), byte(id>>40), byte(id>>32),
				byte(id>>24), byte(id>>16), byte(id>>8), byte(id))

			// Stream mode: consume the header and the padding in
			// window-sized steps so fills and compacts happen on the
			// way to the token.
			sr := NewStreamReader(bytes.NewReader(raw))
			if _, _, err := sr.ReadHeader(); err != nil {
				t.Fatalf("header: %v", err)
			}
			for sr.pos < pad+6 {
				step := min(1<<16, pad+6-sr.pos)
				if _, err := sr.readN(uint64(step)); err != nil {
					t.Fatalf("pad read: %v", err)
				}
			}
			// Buffered-mode oracle over the same bytes.
			br := NewReader(raw)
			if _, _, err := br.ReadHeader(); err != nil {
				t.Fatalf("header: %v", err)
			}
			if _, err := br.readN(uint64(pad)); err != nil {
				t.Fatalf("pad read: %v", err)
			}
			wantID, ok := br.PeekRef()
			if !ok {
				t.Fatalf("buffered PeekRef failed")
			}
			if wantID != id {
				t.Fatalf("buffered PeekRef = %#x, want %#x", wantID, id)
			}

			posBefore := sr.Pos()
			gotID, ok := sr.PeekRef()
			if !ok {
				t.Fatalf("stream PeekRef failed at the boundary")
			}
			if gotID != wantID {
				t.Fatalf("stream PeekRef = %#x, want buffered %#x", gotID, wantID)
			}
			if sr.Pos() != posBefore {
				t.Fatalf("PeekRef moved observable Pos: %d → %d", posBefore, sr.Pos())
			}
			if sr.pos < 0 {
				t.Fatalf("internal cursor negative: %d", sr.pos)
			}
			if class, err := sr.peekFirst(); err != nil || class != classRef {
				t.Fatalf("probe after peek: class %#x err %v", class, err)
			}
			if again, _ := sr.PeekRef(); again != wantID {
				t.Fatalf("repeated PeekRef = %#x, want %#x", again, wantID)
			}
			consumed, err := sr.readTokenArg(classRef)
			if err != nil || consumed != wantID {
				t.Fatalf("consuming read: %#x err %v, want %#x", consumed, err, wantID)
			}
			if !sr.AtEOF() {
				t.Fatalf("stream not exhausted after the token")
			}
		})
	}
}
