package wire

import (
	"math"
	"math/big"
	"reflect"
)

// WriteNil writes a nil-selector token.
func (w *Writer) WriteNil(k NilKind) error {
	w.buf = append(w.buf, classNil<<4|byte(k))
	return nil
}

// ReadNil reads a nil-selector token; selectors 4..11 and width forms are
// reserved/unknown and yield ErrFormat.
func (r *Reader) ReadNil() (NilKind, error) {
	form, err := r.readFirst(classNil)
	if err != nil {
		return 0, err
	}
	if form > byte(NilInterface) {
		return 0, werr(kindMalformedOp, "wire: unknown nil selector %d", form)
	}
	return NilKind(form), nil
}

// WriteBool writes a BOOL token: selector 0=false, 1=true.
func (w *Writer) WriteBool(v bool) error {
	form := byte(0)
	if v {
		form = 1
	}
	w.buf = append(w.buf, classBool<<4|form)
	return nil
}

// ReadBool reads a BOOL token; selectors beyond 0/1 are unknown opcodes.
func (r *Reader) ReadBool() (bool, error) {
	form, err := r.readFirst(classBool)
	if err != nil {
		return false, err
	}
	switch form {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, werr(kindMalformedOp, "wire: unknown bool selector %d", form)
	}
}

// WriteInt writes a signed integer token: ARG carries zigzag(n).
func (w *Writer) WriteInt(n int64) error {
	w.writeTokenArg(classInt, zigzag(n))
	return nil
}

// ReadInt reads a signed integer token: ARG carries zigzag(n).
func (r *Reader) ReadInt() (int64, error) {
	u, err := r.readTokenArg(classInt)
	if err != nil {
		return 0, err
	}
	return unzigzag(u), nil
}

// WriteUint writes an unsigned integer token for values beyond MaxInt64
// zigzag range (UINT class).
func (w *Writer) WriteUint(n uint64) error {
	w.writeTokenArg(classUint, n)
	return nil
}

// ReadUint reads an unsigned integer token (UINT class).
func (r *Reader) ReadUint() (uint64, error) {
	return r.readTokenArg(classUint)
}

// WriteFloat32 writes an f32 token: raw IEEE 754 bits, big-endian.
func (w *Writer) WriteFloat32(f float32) error {
	w.buf = append(w.buf, classFloat<<4) // form 0: f32
	w.buf = appendArgBytes(w.buf, argU32, uint64(math.Float32bits(f)))
	return nil
}

// ReadFloat32 reads an f32 token (form 0 only).
func (r *Reader) ReadFloat32() (float32, error) {
	form, err := r.readFirst(classFloat)
	if err != nil {
		return 0, err
	}
	if form != 0 {
		return 0, werr(kindMalformedOp, "wire: unknown float form %d", form)
	}
	b, err := r.readN(4)
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])), nil
}

// WriteFloat64 writes an f64 token: raw IEEE 754 bits, big-endian.
// NaN payloads, ±0, subnormals, and infinities are preserved bit-for-bit.
func (w *Writer) WriteFloat64(f float64) error {
	w.buf = append(w.buf, classFloat<<4|1)
	w.buf = appendArgBytes(w.buf, argU64, math.Float64bits(f))
	return nil
}

// ReadFloat64 reads an f64 token (form 1 only).
func (r *Reader) ReadFloat64() (float64, error) {
	form, err := r.readFirst(classFloat)
	if err != nil {
		return 0, err
	}
	if form != 1 {
		return 0, werr(kindMalformedOp, "wire: unknown float form %d", form)
	}
	b, err := r.readN(8)
	if err != nil {
		return 0, err
	}
	v := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	return math.Float64frombits(v), nil
}

// zigzagBig maps an arbitrary-precision integer onto the naturals with the
// same bijection as zigzag (generalized beyond 64 bits):
// n ≥ 0 → 2n, n < 0 → −2n−1.
func zigzagBig(n *big.Int) *big.Int {
	u := new(big.Int).Abs(n)
	u.Lsh(u, 1)
	if n.Sign() < 0 {
		u.Sub(u, big.NewInt(1))
	}
	return u
}

// unzigzagBig is the inverse of zigzagBig: even u → u/2, odd u → −(u+1)/2.
func unzigzagBig(u *big.Int) *big.Int {
	n := new(big.Int).Rsh(u, 1)
	if u.Bit(0) == 1 {
		n.Neg(n)
		n.Sub(n, big.NewInt(1))
	}
	return n
}

// WriteBigint writes the value body of a BIGINT position (intern
// the zigzag image as a bare minimal argument — inline/width
// forms below 2^64, the ext form at and above. A nil value is the
// nil selector 0, byte-identical to the inline zero it coincides with:
// pointer-ness is absorbed by the kind.
func (w *Writer) WriteBigint(v *big.Int) error {
	if v == nil {
		return w.WriteNil(NilPointer)
	}
	u := zigzagBig(v)
	if u.IsUint64() {
		w.writeArg(u.Uint64())
		return nil
	}
	w.writeExtArg(u.Bytes())
	return nil
}

// ReadBigint reads the value body of a BIGINT position (intern
// The zero/nil coincidence byte yields a nil value: callers
// materialize it per their projection — nil for pointer shapes, zero for
// value shapes. An advertised ext length above maxLen is ErrBudget before
// any allocation (non-zero maxLen gates, decode budgets).
func (r *Reader) ReadBigint(maxLen uint64) (*big.Int, error) {
	b, err := r.byteAt()
	if err != nil {
		return nil, err
	}
	if b == 0x00 {
		return nil, nil
	}
	if b != argExt {
		u, err := r.argPayload(b)
		if err != nil {
			return nil, err
		}
		return new(big.Int).SetInt64(unzigzag(u)), nil
	}
	body, err := r.readExtArg(maxLen)
	if err != nil {
		return nil, err
	}
	return unzigzagBig(new(big.Int).SetBytes(body)), nil
}

// SkipBigint consumes a BIGINT value body grammatically without
// materializing a value; budget semantics mirror
// ReadBigint.
func (r *Reader) SkipBigint(maxLen uint64) error {
	b, err := r.byteAt()
	if err != nil {
		return err
	}
	if b == 0x00 {
		return nil
	}
	if b != argExt {
		_, err := r.argPayload(b)
		return err
	}
	_, err = r.readExtArg(maxLen)
	return err
}

// SkipDecimal128 consumes a FLOAT form-2 token grammatically without
// materializing the decimal: the token byte and
// 16 raw payload bytes. Materializing readers reject the form; the skip
// path must parse it.
func (r *Reader) SkipDecimal128() error {
	form, err := r.readFirst(classFloat)
	if err != nil {
		return err
	}
	if form != 2 {
		return werr(kindMalformedOp, "wire: unknown float form %d", form)
	}
	_, err = r.readN(16)
	return err
}

// WriteComplex64 writes a c64 token: raw bits of re then im.
func (w *Writer) WriteComplex64(c complex64) error {
	w.buf = append(w.buf, classComplex<<4) // form 0
	w.buf = appendArgBytes(w.buf, argU32, uint64(math.Float32bits(real(c))))
	w.buf = appendArgBytes(w.buf, argU32, uint64(math.Float32bits(imag(c))))
	return nil
}

// ReadComplex64 reads a c64 token (form 0 only).
func (r *Reader) ReadComplex64() (complex64, error) {
	form, err := r.readFirst(classComplex)
	if err != nil {
		return 0, err
	}
	if form != 0 {
		return 0, werr(kindMalformedOp, "wire: unknown complex form %d", form)
	}
	b, err := r.readN(8)
	if err != nil {
		return 0, err
	}
	re := math.Float32frombits(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
	im := math.Float32frombits(uint32(b[4])<<24 | uint32(b[5])<<16 | uint32(b[6])<<8 | uint32(b[7]))
	return complex(re, im), nil
}

// WriteComplex128 writes a c128 token: raw bits of re then im.
func (w *Writer) WriteComplex128(c complex128) error {
	w.buf = append(w.buf, classComplex<<4|1)
	w.buf = appendArgBytes(w.buf, argU64, math.Float64bits(real(c)))
	w.buf = appendArgBytes(w.buf, argU64, math.Float64bits(imag(c)))
	return nil
}

// ReadComplex128 reads a c128 token (form 1 only).
func (r *Reader) ReadComplex128() (complex128, error) {
	form, err := r.readFirst(classComplex)
	if err != nil {
		return 0, err
	}
	if form != 1 {
		return 0, werr(kindMalformedOp, "wire: unknown complex form %d", form)
	}
	b, err := r.readN(16)
	if err != nil {
		return 0, err
	}
	get := func(i int) uint64 {
		return uint64(b[i])<<56 | uint64(b[i+1])<<48 | uint64(b[i+2])<<40 | uint64(b[i+3])<<32 |
			uint64(b[i+4])<<24 | uint64(b[i+5])<<16 | uint64(b[i+6])<<8 | uint64(b[i+7])
	}
	return complex(math.Float64frombits(get(0)), math.Float64frombits(get(8))), nil
}

// WriteStringLit writes a STRING literal: ARG length, raw bytes —
// any bytes, no UTF-8 gate. The string is registered in the intern space
// on first encounter.
func (w *Writer) WriteStringLit(s string) error {
	w.strs[s] = w.allocID()
	w.writeTokenArg(classString, uint64(len(s)))
	w.buf = append(w.buf, s...)
	return nil
}

// WriteString writes a string position: REF on repeated encounter, literal
// on first.
func (w *Writer) WriteString(s string) error {
	if id, ok := w.strs[s]; ok {
		return w.WriteRef(id)
	}
	return w.WriteStringLit(s)
}

// ReadStringLit reads a STRING literal and registers it in the intern space.
func (r *Reader) ReadStringLit() (string, error) {
	return r.readStringLit(0)
}

// SkipStringLit reads a skipped STRING literal position (codec skip
// paths): the intern entry registers like a materialized
// string (the id-space mirror), no value is handed out, and the
// advertised length is budget-checked before the body is consumed.
func (r *Reader) SkipStringLit(maxLen uint64) (string, error) {
	return r.readStringLit(maxLen)
}

func (r *Reader) readStringLit(maxLen uint64) (string, error) {
	n, err := r.readTokenArg(classString)
	if err != nil {
		return "", err
	}
	if maxLen != 0 && n > maxLen {
		return "", werr(kindBudgetBytes, "wire: string length %d exceeds budget %d", n, maxLen)
	}
	b, err := r.readN(n)
	if err != nil {
		return "", err
	}
	s := string(b)
	r.allocEntry(entryString, s, nil, reflect.Value{}, 0)
	return s, nil
}

// ReadString reads a string position: STRING literal or REF to a record
// interned string.
func (r *Reader) ReadString() (string, error) {
	form, err := r.peekFirst()
	if err != nil {
		return "", err
	}
	switch form {
	case classString:
		return r.ReadStringLit()
	case classRef:
		id, err := r.ReadRef()
		if err != nil {
			return "", err
		}
		kind, err := r.kindAt(id)
		if err != nil {
			return "", err
		}
		if kind != entryString {
			return "", werr(kindBadRef, "wire: ref %d is not a string", id)
		}
		return r.strs[id], nil
	default:
		return "", werr(kindMalformedOp, "wire: expected string position, class 0x%X", form)
	}
}

// WriteRef writes a REF token for an intern-space id.
func (w *Writer) WriteRef(id uint64) error {
	w.writeTokenArg(classRef, id)
	return nil
}

// ReadRef reads a REF token and verifies that the id is registered.
func (r *Reader) ReadRef() (uint64, error) {
	id, err := r.readTokenArg(classRef)
	if err != nil {
		return 0, err
	}
	if _, err := r.kindAt(id); err != nil {
		return 0, err
	}
	return id, nil
}

// WriteMapHeader writes a MAP record header: ARG pair count (
// pairs are a codec-layer concern). The map is registered in the intern
// space at its header, before its pairs (record-then-fill).
func (w *Writer) WriteMapHeader(n uint64) error {
	w.allocID()
	w.writeTokenArg(classMap, n)
	return nil
}

// ReadMapHeader reads a MAP record header: the ARG pair count and the record
// id. The entry is registered before its pairs; the codec
// materializes the map object into the entry (SetMapVal) before decoding
// pairs, so cycle REFs resolve mid-fill.
func (r *Reader) ReadMapHeader() (uint64, uint64, error) {
	n, err := r.readTokenArg(classMap)
	if err != nil {
		return 0, 0, err
	}
	id := r.allocEntry(entryMap, "", nil, reflect.Value{}, 0)
	return n, id, nil
}

// WriteStructHeader writes a STRUCT token: field values follow in
// descriptor order.
func (w *Writer) WriteStructHeader() error {
	w.buf = append(w.buf, classStruct<<4) // form 0
	return nil
}

// ReadStructHeader reads a STRUCT token; any form other than 0 is an
// unknown opcode.
func (r *Reader) ReadStructHeader() error {
	form, err := r.readFirst(classStruct)
	if err != nil {
		return err
	}
	if form != 0 {
		return werr(kindMalformedOp, "wire: unknown struct form %d", form)
	}
	return nil
}

// knownEscSubclass holds the experimental/private escape subclasses of
// the format; none are defined.
var knownEscSubclass [256]bool

// ReadEscToken reads an escape token of either class. The reserved low
// nibble of the first byte MUST be zero — a nonzero nibble is a format
// error, not an ignored field: two spellings of one token would break the
// one-legal-byte-sequence contract. Unknown subclasses
// yield ErrFormat — never a silent skip.
func (r *Reader) ReadEscToken() (class byte, sub byte, err error) {
	b, err := r.byteAt()
	if err != nil {
		return 0, 0, err
	}
	class = b >> 4
	if class != classEscExp && class != classEscPriv {
		return 0, 0, werr(kindMalformedArg, "wire: expected escape class, got 0x%02X", b)
	}
	if b&0x0F != 0 {
		return 0, 0, werr(kindMalformedArg, "wire: escape token 0x%02X has a nonzero reserved low nibble", b)
	}
	sub, err = r.byteAt()
	if err != nil {
		return 0, 0, err
	}
	if !knownEscSubclass[sub] {
		return 0, 0, werr(kindMalformedArg, "wire: unknown escape subclass 0x%02X", sub)
	}
	return class, sub, nil
}

// peekFirst returns the class of the next token without consuming it.
func (r *Reader) peekFirst() (byte, error) {
	if r.pos >= len(r.buf) && r.src != nil {
		if err := r.fill(1); err != nil {
			return 0, err
		}
	}
	if r.pos >= len(r.buf) {
		return 0, werrAt(int(r.base+r.pos), kindTruncated, "wire: truncated input")
	}
	return r.buf[r.pos] >> 4, nil
}
