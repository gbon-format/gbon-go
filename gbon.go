package gbon

import (
	"errors"
	"fmt"
	"io"
	"reflect"
)

// The sentinel errors (ErrFormat, ErrBudget, ErrUnsupported, ErrIO) and
// the structured error type live in errors.go.

// Marshal encodes v and returns the wire bytes. It is safe for concurrent
// use by multiple goroutines.
func Marshal(v any) ([]byte, error) { return codecMarshal(v) }

// Unmarshal decodes wire bytes produced by Marshal into the value pointed to
// by v. On any error the value pointed to by v is left exactly as it was
// before the call (decode atomicity). It always decodes with the default
// budgets and without a type
// registry beyond the basic Go types (int..int64, uint..uint64, floats,
// complexes, bool, string, []byte, and the basic composites []any,
// map[string]any, []string, []int64, map[string]string), so interface
// slots holding basic values decode here; other concrete interface values
// need Decoder.Register or
// Decoder.RegisterAs and fail here with ErrFormat; nil interface
// values decode normally. It is safe for concurrent use by multiple
// goroutines.
func Unmarshal(data []byte, v any) error { return codecUnmarshal(data, v) }

// Limits bounds resource consumption on both sides of the codec. On the
// decode side it caps input consumption plus derived backing allocations
// (MaxBytes is one counter: input bytes and charged allocation bytes —
// slice/blob backings booked as L·elemsize before the allocation happens).
// On the encode side it caps derived output resources only: MaxDepth is
// recursion frames, MaxNodes output nodes, MaxBytes output bytes;
// MaxMapPairs and MaxSliceLen do not apply on encode (a live value is not
// a consumable input resource). The zero value of every field means a
// default: decoding defaults are conservative (MaxDepth 10^4, MaxNodes
// 10^6, MaxBytes 10^8, MaxMapPairs 10^6, MaxSliceLen 10^8); encoding
// defaults are looser (MaxDepth 10^6, MaxNodes 10^7, MaxBytes 10^9) —
// the input side is trusted. Marshal's encode defaults (10^6/10^7/10^9)
// exceed the decoder's conservative defaults (10^4/10^6/10^8): values
// encoded at default limits may require SetLimits on the decoding side —
// decode defaults prioritize untrusted-input safety. Exceeding a budget
// fails with an ErrBudget error naming the budget. A negative field is an
// ErrUnsupported error reported before any input is consumed — except that
// on the Encoder a negative MaxBytes removes the byte cap: an explicit
// trust decision for a producer-owned value (decode stays strict — its
// input is untrusted). SetLimits replaces the whole set when given; there
// is no unlimited zero — use a large explicit value.
type Limits struct {
	MaxDepth    int
	MaxNodes    int
	MaxBytes    int
	MaxMapPairs int
	MaxSliceLen int
}

// Encoder writes values to an output stream. An Encoder carries per-stream
// state (intern tables, coder tags), so a single Encoder must be owned by
// one goroutine; create one Encoder per goroutine or per stream. The coder
// registry is per-Encoder: there is no global registry. Encode calls on
// distinct Encoders are safe to run in parallel.
type Encoder struct {
	enc *codecEncoder
	w   io.Writer
	err error
	// reserved holds the names withdrawn from the wire-name namespace
	// (RegisterReserved); encoding a value whose type carries one of
	// these wire names fails before anything reaches the stream.
	reserved   map[string]bool
	reservedOK map[reflect.Type]bool
}

// NewEncoder returns an Encoder writing to w. A nil or typed-nil w makes
// every Encode return a contract_mismatch error; the check is not sticky.
func NewEncoder(w io.Writer) *Encoder {
	e := &Encoder{enc: newCodecEncoder(), w: w}
	e.enc.fac = e
	return e
}

// isNilSource reports a nil interface or a typed-nil pointer passed as an
// io.Reader or io.Writer source.
func isNilSource(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// Encode writes one value to the stream. After an error the Encoder is
// invalid and every subsequent call returns the same error. Calls made from
// inside a Coder's EncodeValue append to the stream buffer without
// flushing; buffered bytes reach w only at the outermost Encode. A wire
// name reserved through RegisterReserved on this Encoder gates the call
// before anything reaches the stream. A nil or typed-nil writer fails
// Encode with a contract_mismatch error before anything is encoded; the
// failure is not sticky.
func (e *Encoder) Encode(v any) error {
	if e.err != nil {
		return e.err
	}
	// in-coder sub-encodes never flush; internal sub-marshals reuse a
	// writer-less facade, so the nil-writer gate applies to the outer Encode.
	if isNilSource(e.w) && e.enc.inCoder == 0 {
		return errUnsupported(classContractMismatch, "", nil, nil, errDetail(fmt.Sprintf("writer must be a non-nil value, got %T", e.w)))
	}
	if err := e.gateReserved(v); err != nil {
		return err
	}
	if err := e.enc.Encode(v); err != nil {
		e.err = err
		return err
	}
	if e.enc.inCoder > 0 {
		return nil
	}
	if err := e.enc.FlushTo(e.w); err != nil {
		err = errIOWrite(err)
		e.err = err
		return err
	}
	return nil
}

// RegisterAs binds wireName to example's type in this Encoder's scope:
// descriptors of the type travel under wireName instead of the
// import-path-derived name. The binding renames identity, not bytes: the
// wire format and the encoding of every unbound type are unchanged. Bind
// a type before encoding it in this stream; binding a struct type that
// has already been encoded here is an ErrUnsupported error (one wire name per type per
// stream keeps the descriptor intern deterministic), as are a reserved
// name (platform primitives, time.Time), a nil example, a wire name
// already bound to a different type, or one type bound to two names;
// re-binding the same pair is a no-op. RegisterAs does not write to the
// stream.
func (e *Encoder) RegisterAs(wireName string, example any) error {
	if e.err != nil {
		return e.err
	}
	t := reflect.TypeOf(example)
	if t == nil {
		return errRegister("RegisterAs example must be a non-nil value")
	}
	if wireName == "" || reservedWireName(wireName) {
		return errRegister(fmt.Sprintf("wire name %q is reserved", wireName))
	}
	if n, ok := e.enc.asName[t]; ok {
		if n != wireName {
			return errRegister(fmt.Sprintf("type %s already bound to wire name %q", t, n))
		}
		return nil
	}
	if e.reserved[wireName] {
		return errRegister(fmt.Sprintf("wire name %q is reserved", wireName))
	}
	if _, cached := e.enc.structDescs[t]; cached {
		return errRegister(fmt.Sprintf("type %s already encoded in this stream", t))
	}
	if prev, ok := e.enc.asType[wireName]; ok && prev != t {
		return errRegister(fmt.Sprintf("wire name %q already bound to %s", wireName, prev))
	}
	if e.enc.asName == nil {
		e.enc.asName = make(map[reflect.Type]string)
	}
	if e.enc.asType == nil {
		e.enc.asType = make(map[string]reflect.Type)
	}
	e.enc.asName[t] = wireName
	e.enc.asType[wireName] = t
	return nil
}

// RegisterReserved marks name as withdrawn from the wire-name namespace
// in this Encoder's scope — a tombstone declaration. The name must be
// well-formed (a wire
// type name with an optional field component, or a dot-free, slash-free
// type token); encoding a value whose type carries a reserved wire name —
// at the root or in any nested position — fails with an ErrUnsupported
// error before anything reaches the stream. Re-reserving the same name is
// a no-op; a name already bound to a live binding or a coder of this
// scope is an error. RegisterReserved does not write to the stream.
func (e *Encoder) RegisterReserved(name string) error {
	if e.err != nil {
		return e.err
	}
	if !validReservedName(name) {
		return errRegister(fmt.Sprintf("reserved name %q is not a well-formed tombstone name", name))
	}
	if e.reserved[name] {
		return nil
	}
	if _, ok := e.enc.asType[name]; ok {
		return errRegister(fmt.Sprintf("reserved name %q already bound to a live binding", name))
	}
	for t := range e.enc.coders {
		if nameOf(t) == name {
			return errRegister(fmt.Sprintf("reserved name %q already holds a coder", name))
		}
	}
	if e.reserved == nil {
		e.reserved = make(map[string]bool)
	}
	e.reserved[name] = true
	// A new reservation can name a type whose clean scan is cached.
	e.reservedOK = nil
	return nil
}

// gateReserved pre-scans the value's type graph for reserved wire names
// (root and every nested position, coder leaves included). A clean scan
// memoizes per root type; an unwalkable type is left to the encode path
// to report.
func (e *Encoder) gateReserved(v any) error {
	if len(e.reserved) == 0 {
		return nil
	}
	t := reflect.TypeOf(v)
	if t == nil {
		return nil
	}
	if e.reservedOK == nil {
		e.reservedOK = make(map[reflect.Type]bool)
	}
	if e.reservedOK[t] {
		return nil
	}
	names, err := scanDescNames(e.enc, t)
	if err != nil {
		return nil
	}
	for n := range names {
		if e.reserved[n] {
			return errRegister(fmt.Sprintf("wire name %q is reserved", n))
		}
	}
	e.reservedOK[t] = true
	return nil
}

// SetLimits replaces the encoding budgets wholesale (mirror of
// Decoder.SetLimits). MaxDepth caps recursion frames, MaxNodes output
// nodes, MaxBytes output bytes of the stream; MaxMapPairs and MaxSliceLen
// do not apply on the encode side. A negative MaxBytes removes the byte
// cap — an explicit trust decision for a producer-owned value; MaxDepth
// and MaxNodes still apply. Encoding defaults (10^6/10^7/10^9)
// exceed the decoder's conservative defaults (10^4/10^6/10^8): values
// encoded at default limits may require SetLimits on the decoding side —
// decode defaults prioritize untrusted-input safety. A negative field
// other than MaxBytes is an ErrUnsupported error reported before anything
// is stored; after the Encoder failed, SetLimits is a no-op returning the
// sticky error.
func (e *Encoder) SetLimits(l Limits) error {
	if e.err != nil {
		return e.err
	}
	if err := validateLimits(l, true); err != nil {
		return err
	}
	e.enc.setLimits(l)
	return nil
}

// RegisterCoder installs c as the custom codec for values of example's type
// in this Encoder's scope. Precedence over the
// derived reflection codec and the automatic Binary/Text adapters: built-in
// types (time.Time) first, then the registered coder, then adapters.
// The built-in time coder preserves the location name and offset pair;
// the abbreviation reported by Zone() derives from that pair, not from
// a tzdata-resolved abbreviation.
// Registering a built-in type, a nil example or coder, or a second,
// different coder for the same type is an ErrUnsupported error;
// re-registering the same coder is a no-op. RegisterCoder does not consume
// the stream. Sub-serialization inside a coder goes through Encode calls on
// the Encoder handed to EncodeValue.
func (e *Encoder) RegisterCoder(example any, c Coder) error {
	t, err := coderExampleType(example, c)
	if err != nil {
		return err
	}
	if t == timeType || isBigintType(t) {
		return errRegister(fmt.Sprintf("%s is built-in coded and cannot be overridden", t))
	}
	if prev, ok := e.enc.coders[t]; ok {
		if prev == c {
			return nil
		}
		return errRegister(fmt.Sprintf("coder for %s already registered (different coder)", t))
	}
	if e.enc.coders == nil {
		e.enc.coders = make(map[reflect.Type]Coder)
	}
	e.enc.coders[t] = c
	return nil
}

// coderExampleType validates a RegisterCoder argument pair: both must be
// non-nil; the type is taken from the example.
func coderExampleType(example, c any) (reflect.Type, error) {
	if example == nil || c == nil {
		return nil, errRegister("RegisterCoder example and coder must be non-nil")
	}
	t := reflect.TypeOf(example)
	if t == nil {
		return nil, errRegister("RegisterCoder example must be a non-nil value")
	}
	return t, nil
}

// Decoder reads values from an input stream record by record: the source
// is consumed incrementally through a sliding window holding one
// in-flight record (plus lookahead), so large streams decode without
// materializing the input. Interned state — descriptors, shared backings
// — is retained for the stream's lifetime per the identity contract.
//
// A Decoder carries per-stream state (type and coder registries, decoding
// position, sticky errors), so a single Decoder must be owned by one
// goroutine; Decoders with separate readers are safe to use in parallel.
// The registries are per-Decoder: there is no global registry.
type Decoder struct {
	limits Limits
	src    io.Reader
	dec    *codecStreamDecoder
	reg    map[string]reflect.Type
	asName map[reflect.Type]string // RegisterAs bindings: type → wire name
	coders map[string]coderEntry
	err    error
	// trusted switches the trusted-input diagnostics (SetTrustedInput):
	// decode errors capture a bounded input window around the failure
	// offset. Read only on the error path.
	trusted bool
	// cur is the in-flight codecDecoder while a coder body decodes:
	// Decode calls made from inside DecodeValue route through it,
	// inheriting the stream's depth/nodes/bytes counters (mirror
	// of the encoder's encodeSub)
	cur *codecDecoder
	// reserved holds the names withdrawn from the wire-name namespace
	// (RegisterReserved); a stream name landing in the set fails the
	// decode with a registry-contract error.
	reserved map[string]bool
}

// basicTypes are the predeclared Go value types pre-registered by
// NewDecoder and the stateless Unmarshal for interface positions (the
// encoding/gob registerBasics precedent): method-less leaves, so no coder
// or adapter is reachable through them.
var basicTypes = []reflect.Type{
	reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](),
	reflect.TypeFor[int32](), reflect.TypeFor[int64](),
	reflect.TypeFor[uint](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](),
	reflect.TypeFor[uint32](), reflect.TypeFor[uint64](),
	reflect.TypeFor[float32](), reflect.TypeFor[float64](),
	reflect.TypeFor[complex64](), reflect.TypeFor[complex128](),
	reflect.TypeFor[bool](), reflect.TypeFor[string](),
	reflect.TypeFor[[]byte](),
	// basic composites over the basic element types: the stateless
	// Unmarshal scope resolves any/interface slots holding these without
	// Register (nested interface elements fall back to the same basic set)
	reflect.TypeFor[[]any](), reflect.TypeFor[map[string]any](),
	reflect.TypeFor[[]string](), reflect.TypeFor[[]int64](),
	reflect.TypeFor[map[string]string](),
}

// newBasicReg returns a fresh name→type registry of the basic types; a
// private map per decoder keeps subsequent Register calls scoped.
func newBasicReg() map[string]reflect.Type {
	reg := make(map[string]reflect.Type, len(basicTypes)+1)
	for _, t := range basicTypes {
		reg[nameOf(t)] = t
	}
	// The built-in BIGINT kind registers under its wire name (pointer-ness
	// absorbed); interface positions materialize *big.Int.
	reg[bigIntWireName] = bigIntPtrType
	return reg
}

// reservedWireName reports whether name is a platform primitive name or
// the built-in coder name: such names never bind through RegisterAs.
func reservedWireName(name string) bool {
	if name == nameOf(timeType) || name == bigIntWireName {
		return true
	}
	for _, t := range basicTypes {
		if nameOf(t) == name {
			return true
		}
	}
	return false
}

// NewDecoder returns a Decoder reading from r. A nil or typed-nil r makes
// every Decode return a contract_mismatch error; the check is not sticky.
// The basic Go types (int,
// int8..int64, uint, uint8..uint64, float32/64, complex64/128, bool,
// string, []byte, and the basic composites []any, map[string]any,
// []string, []int64, map[string]string) come pre-registered, so interface
// slots holding basic values decode without Register; structured and
// domain types still need Register or RegisterAs. The type registry is
// allocated eagerly: types registered after the first Decode are visible
// to subsequent Decode calls.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{src: r, reg: newBasicReg(), asName: make(map[reflect.Type]string)}
}

// SetLimits replaces the decoding budgets wholesale.
func (d *Decoder) SetLimits(l Limits) { d.limits = l }

// SetTrustedInput switches the trusted-input diagnostics: with trusted
// set, every decode error carrying an input offset captures a bounded hex
// window of the input around that offset — appended to the verbose
// rendering and available in machine form through Error.Snippet. The
// default is off and input bytes never enter error output. The window is
// captured at the moment the error is raised; flipping the flag afterwards
// does not retrofit windows onto errors already returned.
func (d *Decoder) SetTrustedInput(trusted bool) { d.trusted = trusted }

// Decode reads one value from the stream into the value pointed to by v.
// On any error other than io.EOF the value pointed to by v is left exactly
// as it was before the call (decode atomicity). At the end of the
// stream it returns io.EOF. A stream carrying no values after the
// header decodes to a truncated error on the first call: an Encoder
// writes the header together with the first value, so a value-less
// header-only input cannot be the end of a well-formed stream. Errors
// detected before the
// stream is consumed — an invalid target, a negative Limits field, a nil
// or typed-nil reader — leave the Decoder usable; the nil reader fails
// with a contract_mismatch error and the check is not sticky. After any
// other error the Decoder is invalid and every subsequent call returns
// the same error.
func (d *Decoder) Decode(v any) error {
	if d.err != nil {
		return d.err
	}
	if err := validateLimits(d.limits, false); err != nil {
		return err
	}
	if _, err := decodeTarget(v); err != nil {
		return err
	}
	// inside a coder body: sub-decode shares the in-flight counters
	if d.cur != nil {
		return d.cur.decodeSub(v)
	}
	if d.dec == nil {
		if isNilSource(d.src) {
			return errUnsupported(classContractMismatch, "", nil, nil, errDetail(fmt.Sprintf("reader must be a non-nil value, got %T", d.src)))
		}
		sd := &codecStreamDecoder{reg: d.reg, asName: d.asName, coders: d.coders, fac: d}
		if err := sd.init(d.src); err != nil {
			d.err = err
			return err
		}
		d.dec = sd
	}
	err := d.dec.Decode(v, d.limits)
	if err != nil && !errors.Is(err, io.EOF) {
		if re := d.mapReserved(err); re != nil {
			d.err = re
			return re
		}
		d.err = err
	}
	return err
}

// RegisterReserved marks name as withdrawn from the wire-name namespace
// in this Decoder's scope — a tombstone declaration. The name must be
// well-formed (a wire
// type name with an optional field component, or a dot-free, slash-free
// type token); a stream carrying a reserved name in a position that
// resolves through the registry fails the decode with an ErrUnsupported
// error. Re-reserving the same name is a no-op; a name already registered
// to a type of this scope is an error. RegisterReserved does not consume
// the stream.
func (d *Decoder) RegisterReserved(name string) error {
	if !validReservedName(name) {
		return errRegister(fmt.Sprintf("reserved name %q is not a well-formed tombstone name", name))
	}
	if d.reserved[name] {
		return nil
	}
	if d.reg != nil {
		if _, ok := d.reg[name]; ok {
			return errRegister(fmt.Sprintf("reserved name %q already registered to a type", name))
		}
	}
	for _, bound := range d.asName {
		if bound == name {
			return errRegister(fmt.Sprintf("reserved name %q already bound to a live binding", name))
		}
	}
	if _, ok := d.coders[name]; ok {
		return errRegister(fmt.Sprintf("reserved name %q already holds a coder", name))
	}
	if d.reserved == nil {
		d.reserved = make(map[string]bool)
	}
	d.reserved[name] = true
	return nil
}

// mapReserved re-reports a registry miss on a reserved name as the
// reserved-name contract error; nil keeps the original error.
func (d *Decoder) mapReserved(err error) error {
	if len(d.reserved) == 0 {
		return nil
	}
	var ae *Error
	if !errors.As(err, &ae) || ae.Class() != classUnknownName {
		return nil
	}
	name, _ := ae.Got.(string)
	if !d.reserved[name] {
		return nil
	}
	return errUnsupported(classRegisterConflict, "", name, nil,
		errDetail(fmt.Sprintf("stream name %q is reserved in this scope", name)))
}

// Register adds concrete types for interface decoding in this Decoder's
// scope. Each argument is an example value of
// the type to register: any serializable type qualifies, including
// unhashable ones (maps, slices) for interface value positions — the type
// is taken from the example without hashing it. Map keys with unhashable
// dynamic values stay rejected by the encoder regardless of registration.
// Registering a name with a different type is an ErrUnsupported error;
// re-registering the same type is a no-op. A type bound through
// RegisterAs registers under its bound wire name; the order of the
// Register and RegisterAs calls does not matter. Register does not consume
// the stream. To decode concrete interface values, use this Decoder (with the
// types registered) rather than the stateless Unmarshal.
func (d *Decoder) Register(types ...any) error {
	for _, ex := range types {
		t := reflect.TypeOf(ex)
		if t == nil {
			return errRegister("Register example must be a non-nil value")
		}
		if _, err := descOfRegister(t, ""); err != nil {
			return err
		}
		name := nameOf(t)
		if n, ok := d.asName[t]; ok {
			name = n
		}
		if d.reserved[name] {
			return errRegister(fmt.Sprintf("wire name %q is reserved", name))
		}
		if prev, ok := d.reg[name]; ok {
			if prev != t {
				return errRegister(fmt.Sprintf("interface type name %q already registered to %s", name, prev))
			}
			continue
		}
		if d.reg == nil {
			d.reg = make(map[string]reflect.Type)
		}
		d.reg[name] = t
	}
	return nil
}

// RegisterAs binds wireName to example's type in this Decoder's scope:
// descriptors carrying wireName resolve to the local type — interface
// positions, concrete-target matches and coder lookups — bypassing the
// import-path-derived name. This is the cross-name evolution hook: a
// producer's v2 type and a consumer's v1 type meet on the shared wire
// name with the field semantics unchanged (dropped fields skipped,
// absent fields zero, kind drift rejected); the binding fixes the name,
// not field compatibility. Binding a reserved name (platform primitives,
// time.Time), a nil example, a wire name already registered to a
// different type, or one type to two names is an ErrUnsupported error;
// re-binding the same pair is a no-op. RegisterAs does not consume the
// stream.
func (d *Decoder) RegisterAs(wireName string, example any) error {
	t := reflect.TypeOf(example)
	if t == nil {
		return errRegister("RegisterAs example must be a non-nil value")
	}
	if wireName == "" || reservedWireName(wireName) {
		return errRegister(fmt.Sprintf("wire name %q is reserved", wireName))
	}
	if d.reserved[wireName] {
		return errRegister(fmt.Sprintf("wire name %q is reserved", wireName))
	}
	if _, err := descOfRegister(t, ""); err != nil {
		return err
	}
	if n, ok := d.asName[t]; ok {
		if n != wireName {
			return errRegister(fmt.Sprintf("type %s already bound to wire name %q", t, n))
		}
		return nil
	}
	if prev, ok := d.reg[wireName]; ok && prev != t {
		return errRegister(fmt.Sprintf("wire name %q already registered to %s", wireName, prev))
	}
	if d.reg == nil {
		d.reg = make(map[string]reflect.Type)
	}
	if d.asName == nil {
		d.asName = make(map[reflect.Type]string)
	}
	d.reg[wireName] = t
	d.asName[t] = wireName
	return nil
}

// RegisterCoder installs c as the custom codec for values of example's type
// in this Decoder's scope. Resolution precedence mirrors the encoder:
// built-in types (time.Time) first, then
// this registry, then the automatic Binary/Text adapters. Registering a
// built-in type, a nil example or coder, a second different coder for the
// same type, or a name already bound to a different type is an
// ErrUnsupported error; re-registering the same coder is a no-op.
// RegisterCoder does not consume the stream; registrations between Decode
// calls are visible to subsequent Decode calls. Sub-deserialization inside
// a coder goes through Decode calls on the Decoder handed to DecodeValue.
func (d *Decoder) RegisterCoder(example any, c Coder) error {
	t, err := coderExampleType(example, c)
	if err != nil {
		return err
	}
	if t == timeType || isBigintType(t) {
		return errRegister(fmt.Sprintf("%s is built-in coded and cannot be overridden", t))
	}
	name := nameOf(t)
	if d.reserved[name] {
		return errRegister(fmt.Sprintf("wire name %q is reserved", name))
	}
	if prev, ok := d.coders[name]; ok {
		if prev.typ == t && prev.tc.c == c {
			return nil
		}
		return errRegister(fmt.Sprintf("coder for %q already registered (different type or coder)", name))
	}
	if d.coders == nil {
		d.coders = make(map[string]coderEntry)
	}
	d.coders[name] = coderEntry{typ: t, tc: typeCoder{ck: ckCustom, c: c}}
	return nil
}

// Coder is the extension point for type-specific optimal encodings. Encoders
// and decoders dispatch to a registered Coder instead of the derived
// reflection-based codec for that type; the type travels as a CODER type
// descriptor (name plus per-stream tag), one tag per type per stream.
// EncodeValue writes the value's body at the current position; DecodeValue
// reads it into the (settable) target. Nested values must go through
// Encode/Decode calls on the handed Encoder/Decoder — sub-serialization
// shares the stream's intern space and participates in topology tracking
// and in the stream's budgets on both sides (decode input/allocations and
// encode depth/nodes/bytes: coder sub-values inherit the stream counters
// without isolation); calling back with the coder's own value recurses and
// fails — on encode with a format error, on decode by exhausting the
// inherited depth budget. Errors from a coder surface wrapped in exactly
// one sentinel; a failing coder breaks the stream like any other error.
// Coder values in map key positions are unsupported (no canonical key bytes
// for coder bodies). A Coder encoding values with pointer-bearing types:
// the canonical-order tie-break sub-marshals pair values without
// inheriting the active-coder set — recursion through this path fails on
// the depth budget rather than a format error.
type Coder interface {
	EncodeValue(*Encoder, reflect.Value) error
	DecodeValue(*Decoder, reflect.Value) error
}
