package gbon

import (
	"encoding"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gbon-format/gbon-go/internal/wire"
)

// maxDepth is the codec-level recursion cap default for both decoding
// axes: value-body nesting and descriptor-literal parsing. A decoder's
// Limits.MaxDepth overrides it. Exceeding the cap yields an ErrBudget
// wrap, never a stack-exhaustion panic.
const maxDepth = 10000

// nameOf returns the canonical descriptor name for t:
// platform primitive names, pkg.Type for named types, structural
// composition for unnamed composites, []byte special case. Named types
// outside the standard library carry their full package path — the name is
// the descriptor-intern key and the short package name
// alone collides across packages.
func nameOf(t reflect.Type) string {
	if isUnnamedByteSlice(t) {
		return "[]byte"
	}
	switch t.Kind() {
	case reflect.Slice:
		if t.Name() != "" {
			return qualifiedName(t)
		}
		return "[]" + nameOf(t.Elem())
	case reflect.Array:
		if t.Name() != "" {
			return qualifiedName(t)
		}
		return "[" + strconv.Itoa(t.Len()) + "]" + nameOf(t.Elem())
	case reflect.Map:
		if t.Name() != "" {
			return qualifiedName(t)
		}
		return "map[" + nameOf(t.Key()) + "]" + nameOf(t.Elem())
	case reflect.Pointer:
		if t.Name() != "" {
			return qualifiedName(t)
		}
		return "*" + nameOf(t.Elem())
	}
	if t.Name() != "" && t.PkgPath() != "" {
		return qualifiedName(t)
	}
	return t.String()
}

// qualifiedName is the name of a defined type outside the standard
// library: import path + "." + the short t.String() form. Standard
// library and main packages keep the short form (wire-byte stability for
// time.Time and friends).
func qualifiedName(t reflect.Type) string {
	if p := t.PkgPath(); p != "" && !isStdlibPath(p) {
		return p + "." + t.String()
	}
	return t.String()
}

// isStdlibPath reports whether an import path belongs to the Go standard
// library: no domain dot in the first segment ("time", "net/http"); a
// main package is stdlib-shaped for naming purposes.
func isStdlibPath(p string) bool {
	seg := p
	if before, _, ok := strings.Cut(p, "/"); ok {
		seg = before
	}
	return !strings.Contains(seg, ".")
}

// nameOverride resolves a type to a scope-bound wire name (RegisterAs):
// bound=true replaces nameOf(t) at every descriptor naming point of that
// scope. nil (or bound=false) keeps the canonical nameOf derivation.
type nameOverride func(t reflect.Type) (name string, bound bool)

// scopeName is the effective descriptor name of t under naming: the
// scope binding when present, otherwise the canonical nameOf(t).
func scopeName(naming nameOverride, t reflect.Type) string {
	if naming != nil {
		if n, bound := naming(t); bound {
			return n
		}
	}
	return nameOf(t)
}

// namingOf adapts a type→wire-name binding table into a nameOverride;
// empty tables yield nil so the default path stays untouched.
func namingOf(asName map[reflect.Type]string) nameOverride {
	if len(asName) == 0 {
		return nil
	}
	return func(t reflect.Type) (string, bool) {
		n, ok := asName[t]
		return n, ok
	}
}

// isUnnamedByteSlice reports t == []byte (the BLOB canonical form).
func isUnnamedByteSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice &&
		t.Elem().Kind() == reflect.Uint8 && t.Elem().PkgPath() == ""
}

// isByteSliceBase reports slice-of-unnamed-uint8 (named or not): the value
// body is a BLOB record for such types.
func isByteSliceBase(t reflect.Type) bool {
	return t.Kind() == reflect.Slice &&
		t.Elem().Kind() == reflect.Uint8 && t.Elem().PkgPath() == ""
}

func intKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func uintKind(k reflect.Kind) bool {
	switch k {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func primitiveKeyKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	}
	return intKind(k) || uintKind(k)
}

// comparableKeyKind reports statically encodable map-key kinds:
// all compile-time comparable Go categories plus interface keys,
// whose dynamic type must be hashable — checked per value at the key
// position, never by Go hashing.
func comparableKeyKind(k reflect.Kind) bool {
	switch k {
	case reflect.Struct, reflect.Array, reflect.Pointer, reflect.Interface:
		return true
	}
	return primitiveKeyKind(k)
}

func unsupportedAt(what, path string) error {
	return errUnsupported(classUnsupportedKind, path, nil, nil, errDetail(fmt.Sprintf("unsupported %s", what)))
}

// primName maps a primitive kind to its platform name (amd64 basis).
func primName(k reflect.Kind) string {
	switch k {
	case reflect.Bool:
		return "bool"
	case reflect.Int:
		return "int"
	case reflect.Int8:
		return "int8"
	case reflect.Int16:
		return "int16"
	case reflect.Int32:
		return "int32"
	case reflect.Int64:
		return "int64"
	case reflect.Uint:
		return "uint"
	case reflect.Uint8:
		return "uint8"
	case reflect.Uint16:
		return "uint16"
	case reflect.Uint32:
		return "uint32"
	case reflect.Uint64:
		return "uint64"
	case reflect.Float32:
		return "float32"
	case reflect.Float64:
		return "float64"
	case reflect.Complex64:
		return "complex64"
	case reflect.Complex128:
		return "complex128"
	case reflect.String:
		return "string"
	}
	return k.String()
}

// primDesc builds the unnamed primitive descriptor for kind/size: INT/UINT
// width = Size; FLOAT 4/8; COMPLEX per-component Size/2.
func primDesc(k reflect.Kind, size uintptr) *wire.Desc {
	switch k {
	case reflect.Bool:
		return &wire.Desc{Kind: wire.KindBool, Name: "bool"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &wire.Desc{Kind: wire.KindInt, Name: primName(k), Width: uint64(size)}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &wire.Desc{Kind: wire.KindUint, Name: primName(k), Width: uint64(size)}
	case reflect.Float32, reflect.Float64:
		return &wire.Desc{Kind: wire.KindFloat, Name: primName(k), Width: uint64(size)}
	case reflect.Complex64, reflect.Complex128:
		return &wire.Desc{Kind: wire.KindComplex, Name: primName(k), Width: uint64(size / 2)}
	case reflect.String:
		return &wire.Desc{Kind: wire.KindString, Name: "string"}
	}
	return nil
}

// descLeafHook is the dispatch-point substitution of the descriptor walk
// traversal: it resolves t into an out-of-tree leaf when the current scope
// owns one — the encoder's coder scope; ok=false keeps t on the
// derived reflection walk. nil hook = pure grammatical walk.
type descLeafHook func(t reflect.Type) (d *wire.Desc, ok bool)

// descOf builds the wire descriptor for t. path is the value
// path of the enclosing position, used to report rejects down to the field.
func descOf(t reflect.Type, path string) (*wire.Desc, error) {
	return descWalk(t, path, make(map[reflect.Type]*wire.Desc), nil, nil)
}

// registerLeaf is the Register-path leaf hook (mirror of the encoder's
// coderLeaf dispatch in structFor): a built-in coder type resolves to an
// opaque CODER leaf instead of the derived grammatical walk — time.Time's
// runtime struct would not survive the walk (unexported fields), while its
// stream image is a coder body.
func registerLeaf(t reflect.Type) (*wire.Desc, bool) {
	if t == timeType {
		return &wire.Desc{Kind: wire.KindCoder, Name: nameOf(t)}, true
	}
	if isBigintType(t) {
		return &wire.Desc{Kind: wire.KindBigint, Name: bigIntWireName}, true
	}
	return nil, false
}

// descOfRegister validates t for the Register path: the grammatical walk
// with the built-in coder leaf hook, so coder-covered field types nest as
// CODER leaves exactly as the encoder emits them.
func descOfRegister(t reflect.Type, path string) (*wire.Desc, error) {
	return descWalk(t, path, make(map[reflect.Type]*wire.Desc), registerLeaf, nil)
}

// descWalk is the single descriptor-tree traversal: memoized per
// build — cache shares in-progress descriptors so type cycles close
// through REF tokens — with the leaf hook consulted at each
// dispatch point before the grammatical walk. Composite descriptors
// register in the cache before their children.
func descWalk(t reflect.Type, path string, cache map[reflect.Type]*wire.Desc, hook descLeafHook, naming nameOverride) (*wire.Desc, error) {
	if d, ok := cache[t]; ok {
		return d, nil
	}
	if hook != nil {
		if d, leaf := hook(t); leaf {
			cache[t] = d
			return d, nil
		}
	}
	if isUnnamedByteSlice(t) {
		d := &wire.Desc{Kind: wire.KindBlob, Name: "[]byte"}
		cache[t] = d
		return d, nil
	}
	switch t.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128,
		reflect.String:
		d := primDesc(t.Kind(), t.Size())
		if t.Name() != "" && t.PkgPath() != "" {
			d = &wire.Desc{Kind: wire.KindNamed, Name: scopeName(naming, t), Refs: []*wire.Desc{d}}
		}
		cache[t] = d
		return d, nil
	case reflect.Slice:
		if t.Name() != "" && t.Elem().Kind() == reflect.Uint8 && t.Elem().PkgPath() == "" {
			d := &wire.Desc{Kind: wire.KindNamed, Name: scopeName(naming, t),
				Refs: []*wire.Desc{{Kind: wire.KindBlob, Name: "[]byte"}}}
			cache[t] = d
			return d, nil
		}
		d := &wire.Desc{Kind: wire.KindSlice, Name: scopeName(naming, t)}
		cache[t] = d
		elem, err := descWalk(t.Elem(), path, cache, hook, naming)
		if err != nil {
			return nil, err
		}
		d.Refs = []*wire.Desc{elem}
		return d, nil
	case reflect.Array:
		d := &wire.Desc{Kind: wire.KindArray, Name: scopeName(naming, t), Len: uint64(t.Len())}
		cache[t] = d
		elem, err := descWalk(t.Elem(), path, cache, hook, naming)
		if err != nil {
			return nil, err
		}
		d.Refs = []*wire.Desc{elem}
		return d, nil
	case reflect.Map:
		if !comparableKeyKind(t.Key().Kind()) {
			return nil, unsupportedAt("map key type "+nameOf(t.Key()), path)
		}
		if hook != nil {
			if d, leaf := hook(t.Key()); leaf && d.Kind != wire.KindBigint {
				return nil, unsupportedAt("map key type "+nameOf(t.Key()), path)
			}
		}
		d := &wire.Desc{Kind: wire.KindMap, Name: scopeName(naming, t)}
		cache[t] = d
		key, err := descWalk(t.Key(), path, cache, hook, naming)
		if err != nil {
			return nil, err
		}
		elem, err := descWalk(t.Elem(), path, cache, hook, naming)
		if err != nil {
			return nil, err
		}
		d.Refs = []*wire.Desc{key, elem}
		return d, nil
	case reflect.Pointer:
		d := &wire.Desc{Kind: wire.KindPointer, Name: scopeName(naming, t)}
		cache[t] = d
		elem, err := descWalk(t.Elem(), path, cache, hook, naming)
		if err != nil {
			return nil, err
		}
		d.Refs = []*wire.Desc{elem}
		return d, nil
	case reflect.Struct:
		d := &wire.Desc{Kind: wire.KindStruct, Name: scopeName(naming, t)}
		cache[t] = d
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Name == "_" {
				continue
			}
			if f.PkgPath != "" {
				return nil, unsupportedAt("unexported field "+f.Name, path+"."+f.Name)
			}
			ft, err := descWalk(f.Type, path+"."+f.Name, cache, hook, naming)
			if err != nil {
				return nil, err
			}
			d.Fields = append(d.Fields, wire.Field{Name: f.Name, Type: ft, Idx: i})
		}
		return d, nil
	case reflect.Interface:
		d := &wire.Desc{Kind: wire.KindInterface, Name: scopeName(naming, t)}
		cache[t] = d
		return d, nil
	default:
		return nil, unsupportedAt("kind "+t.Kind().String()+" ("+nameOf(t)+")", path)
	}
}

// matchDesc strictly matches a stream descriptor against the target type:
// name plus structure — kind, width, array length, struct
// fields by name and order. skipName is set for the base of a NAMED
// descriptor, whose name check already happened one level up.
// matchDescNamed validates a stream descriptor against the target type
// under a scope naming: bound target types match their bound wire name
// instead of nameOf(t).
func matchDescNamed(d *wire.Desc, t reflect.Type, naming nameOverride) error {
	return match(d, t, false, make(map[*wire.Desc]bool), naming)
}

// match walks d against t; visiting memoizes in-progress descriptors so
// self-referential types terminate (assume a match on the back edge).
func match(d *wire.Desc, t reflect.Type, skipName bool, visiting map[*wire.Desc]bool, naming nameOverride) error {
	if visiting[d] {
		return nil
	}
	visiting[d] = true
	tn := scopeName(naming, t)
	if !skipName && tn != d.Name {
		// Pointer-ness of the built-in BIGINT kind is absorbed by the
		// kind: the wire name "big.Int" matches both
		// Go projections, in both the canonical kind-15 form and the
		// kind-14 coder form.
		if !(d.Name == bigIntWireName && isBigintType(t) &&
			(d.Kind == wire.KindBigint || d.Kind == wire.KindCoder)) {
			return errFormat(classTypeMismatch, -1, "", d.Name, tn, errDetail("type mismatch"))
		}
	}
	kindMismatch := func() error {
		return errFormat(classTypeMismatch, -1, "", d.Name, t.Kind(), errDetail(fmt.Sprintf("stream kind %d, target %s", d.Kind, t.Kind())))
	}
	switch d.Kind {
	case wire.KindNamed:
		if t.Name() == "" || t.PkgPath() == "" {
			return kindMismatch()
		}
		return match(d.Refs[0], t, true, visiting, naming)
	case wire.KindCoder:
		// opaque body: the name is the only structural anchor
	case wire.KindBool:
		if t.Kind() != reflect.Bool {
			return kindMismatch()
		}
	case wire.KindInt:
		if !intKind(t.Kind()) || uint64(t.Size()) != d.Width {
			return kindMismatch()
		}
	case wire.KindUint:
		if !uintKind(t.Kind()) || uint64(t.Size()) != d.Width {
			return kindMismatch()
		}
	case wire.KindFloat:
		if d.Width == 16 {
			// Decimal128 is specified in the format (form 2,
			// without a Go projection: the stream is valid, the
			// projection is incomplete — a loud unsupported error, never
			// a nameless mismatch or a format error.
			return errUnsupported(classUnsupportedKind, "", nil, nil, errDetail("decimal128 (FLOAT width 16, form 2) is specified by the format and not implemented in the Go projection"))
		}
		if t.Kind() != reflect.Float32 && t.Kind() != reflect.Float64 || uint64(t.Size()) != d.Width {
			return kindMismatch()
		}
	case wire.KindComplex:
		if t.Kind() != reflect.Complex64 && t.Kind() != reflect.Complex128 || uint64(t.Size()/2) != d.Width {
			return kindMismatch()
		}
	case wire.KindString:
		if t.Kind() != reflect.String {
			return kindMismatch()
		}
	case wire.KindBlob:
		if !isByteSliceBase(t) {
			return kindMismatch()
		}
	case wire.KindSlice:
		if t.Kind() != reflect.Slice {
			return kindMismatch()
		}
		// Canonical dual: a slice-of-unnamed-uint8
		// type is a BLOB; a SLICE descriptor in a byte-slice position is
		// a second legal encoding of the same value — a decoder reject.
		if isByteSliceBase(t) {
			return errFormat(classTypeMismatch, -1, "", d.Name, nameOf(t), errDetail("stream kind SLICE, target is a byte-slice base (canonical kind is BLOB per the wire specification)"))
		}
		return match(d.Refs[0], t.Elem(), false, visiting, naming)
	case wire.KindArray:
		if t.Kind() != reflect.Array || uint64(t.Len()) != d.Len {
			return kindMismatch()
		}
		return match(d.Refs[0], t.Elem(), false, visiting, naming)
	case wire.KindMap:
		if t.Kind() != reflect.Map {
			return kindMismatch()
		}
		if !comparableKeyKind(t.Key().Kind()) {
			return unsupportedAt("map key type "+nameOf(t.Key()), nameOf(t))
		}
		if err := match(d.Refs[0], t.Key(), false, visiting, naming); err != nil {
			return err
		}
		return match(d.Refs[1], t.Elem(), false, visiting, naming)
	case wire.KindPointer:
		if t.Kind() != reflect.Pointer {
			return kindMismatch()
		}
		return match(d.Refs[0], t.Elem(), false, visiting, naming)
	case wire.KindStruct:
		if t.Kind() != reflect.Struct {
			return kindMismatch()
		}
		// fields matched by name: wire fields absent on the target
		// are skipped at decode; target fields absent from the stream stay
		// zero; matched fields validate structure recursively
		for i := range d.Fields {
			sf := d.Fields[i]
			f, ok := t.FieldByName(sf.Name)
			if !ok || f.Name == "_" || f.PkgPath != "" {
				continue
			}
			if err := match(sf.Type, f.Type, false, visiting, naming); err != nil {
				return err
			}
		}
	case wire.KindInterface:
		if t.Kind() != reflect.Interface {
			return kindMismatch()
		}
	case wire.KindBigint:
		if !isBigintType(t) {
			return kindMismatch()
		}
	default:
		return errFormat(classMalformedOp, -1, "", nil, nil, errDetail(fmt.Sprintf("unknown descriptor kind %d", d.Kind)))
	}
	return nil
}

// descZeroSize reports whether the type described by d has Go size zero:
// empty structs and zero-length arrays at any depth, the only zero-size
// shapes. This is the decoder-side half of the zero-size gate —
// the encoder reserves pointer-target ids only for Elem().Size()!=0, and
// the skip path (which sees a descriptor, not a target type) mirrors
// through the descriptor structure. The depth cap terminates crafted
// descriptor cycles; real zero-size types are shallow by construction
// (an inductive type needs a non-zero carrier).
func descZeroSize(d *wire.Desc, depth int) bool {
	// Mirror the wire-layer descriptor depth budget: legitimate chains
	// within MaxDescDepth (10^4) must classify correctly; only crafted
	// nesting beyond it falls through to the non-zero path (and is
	// rejected by the depth budget before this matters).
	if depth > 10000 {
		return false
	}
	switch d.Kind {
	case wire.KindNamed:
		return descZeroSize(d.Refs[0], depth+1)
	case wire.KindStruct:
		for _, f := range d.Fields {
			if !descZeroSize(f.Type, depth+1) {
				return false
			}
		}
		return true
	case wire.KindArray:
		return d.Len == 0 || descZeroSize(d.Refs[0], depth+1)
	default:
		return false
	}
}

// Coder dispatch. Precedence on both codec sides:
// built-in time.Time > RegisterCoder > Binary adapter > Text adapter >
// derived reflection walk. Adapter eligibility is the full-pair rule on the
// pointer method set: *T must implement Marshaler+Unmarshaler of
// one flavor — value-receiver marshalers promote into it, and pointer-only
// pairs (big.Int, url.URL) qualify while staying callable through an
// addressable copy at encode time.
var (
	binMarshalerType   = reflect.TypeFor[encoding.BinaryMarshaler]()
	binUnmarshalerType = reflect.TypeFor[encoding.BinaryUnmarshaler]()
	txtMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	txtUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	timeType           = reflect.TypeFor[time.Time]()

	// The built-in BIGINT kind covers both Go projections of math/big
	// integers: pointer-ness is absorbed by the kind, one wire name.
	bigIntType     = reflect.TypeFor[big.Int]()
	bigIntPtrType  = reflect.TypeFor[*big.Int]()
	bigIntWireName = "big.Int"
)

// isBigintType reports whether t is one of the two Go projections of the
// BIGINT kind.
func isBigintType(t reflect.Type) bool {
	return t == bigIntType || t == bigIntPtrType
}

type coderKind uint8

const (
	ckNone coderKind = iota
	ckCustom
	ckBinary
	ckText
	ckTime
	ckBigint
	// ckBigintLegacy is the decode-only reader of the kind-14 "big.Int"
	// spelling: a STRING body per the text adapter, accepted on decode
	// and never emitted. coderFor never resolves to it — the
	// canonical encoding of big integers is the BIGINT kind.
	ckBigintLegacy
)

// typeCoder is one resolved coder for one reflect.Type: the public Coder for
// ckCustom, the automatic adapters and the built-in time coder otherwise.
type typeCoder struct {
	ck coderKind
	c  Coder
}

// adapterOf reports the adapter flavor for t: adapterBinary when *T
// holds the Binary pair, adapterText for the Text pair, ckNone when
// neither pair is complete (half pairs stay on the derived path).
func adapterOf(t reflect.Type) coderKind {
	pt := reflect.PointerTo(t)
	if pt.Implements(binMarshalerType) && pt.Implements(binUnmarshalerType) {
		return ckBinary
	}
	if pt.Implements(txtMarshalerType) && pt.Implements(txtUnmarshalerType) {
		return ckText
	}
	return ckNone
}

// marshalAddr returns an addressable value whose pointer method set can be
// used for marshaling v: v itself when addressable, a copy otherwise —
// Marshal* must not mutate the value (as in encoding/json).
func marshalAddr(v reflect.Value) reflect.Value {
	if v.CanAddr() {
		return v.Addr()
	}
	tmp := reflect.New(v.Type()).Elem()
	tmp.Set(v)
	return tmp.Addr()
}

// seamCoder fills the seam-assigned location onto an error returned
// through a coder seam: a core error with empty path/offset takes the
// caller's context, any other error is wrapped as coder_error with the
// original as the cause. The coder contract: return raw errors, the
// location is not the coder's concern.
func seamCoder(err error, off int, path string) error {
	if err == nil {
		return nil
	}
	if ae, ok := errors.AsType[*Error](err); ok {
		if ae.Path == "" {
			ae.Path = path
		}
		if ae.Offset < 0 && off >= 0 {
			ae.Offset = off
		}
		return ae
	}
	return errCoder(classCoderError, off, path, err)
}

// encode writes the coder body of v at the current body position. Any
// error leaving the coder body — a user coder's raw error included —
// exits through the seam with the location assigned here; the path
// renders only on the error path.
func (tc *typeCoder) encode(e *codecEncoder, v reflect.Value, p pathNode) error {
	err := tc.encodeBody(e, v, p)
	if err == nil {
		return nil
	}
	return seamCoder(err, -1, p.String())
}

func (tc *typeCoder) encodeBody(e *codecEncoder, v reflect.Value, p pathNode) error {
	switch tc.ck {
	case ckBigint:
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return e.w.WriteNil(wire.NilPointer)
			}
			return e.w.WriteBigint(v.Interface().(*big.Int))
		}
		return e.w.WriteBigint(marshalAddr(v).Interface().(*big.Int))
	case ckTime:
		tt := v.Interface().(time.Time).Round(0) // mono-strip
		_, off := tt.Zone()
		name := tt.Location().String() // IANA/UTC/Local/fixed name, not the abbreviation
		if err := e.w.WriteInt(tt.Unix()); err != nil {
			return err
		}
		if err := e.w.WriteUint(uint64(tt.Nanosecond())); err != nil {
			return err
		}
		if err := e.w.WriteInt(int64(off)); err != nil {
			return err
		}
		return e.w.WriteString(name)
	case ckBinary:
		m, err := marshalAddr(v).Interface().(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return errCoder(classCoderError, -1, p.String(), err)
		}
		L := uint64(len(m))
		if err := e.w.WriteBlobHeader(L, denseBytes(m)); err != nil {
			return err
		}
		return e.w.WriteRawBytes(m)
	case ckText:
		m, err := marshalAddr(v).Interface().(encoding.TextMarshaler).MarshalText()
		if err != nil {
			return errCoder(classCoderError, -1, p.String(), err)
		}
		return e.w.WriteString(string(m))
	default:
		if e.activeCoder(v.Type()) {
			return errCoder(classCoderRecursion, -1, p.String(), errDetail(fmt.Sprintf("coder %s recurses into itself", nameOf(v.Type()))))
		}
		e.setActiveCoder(v.Type(), true)
		defer e.setActiveCoder(v.Type(), false)
		e.inCoder++
		defer func() { e.inCoder-- }()
		return tc.c.EncodeValue(e.facade(), v)
	}
}

// zoneFor resolves a wire zone name onto a *Location: UTC and
// Local map to the singleton zones; anything else loads the IANA database
// and falls back to FixedZone(name, offsetSec) — preserving the observable
// Zone() pair — when the name is unknown or empty.
func zoneFor(name string, offsetSec int) *time.Location {
	switch name {
	case "UTC":
		return time.UTC
	case "Local":
		return time.Local
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone(name, offsetSec)
}

// decode reads the coder body into the addressable target. Any error
// leaving the coder body — a user coder's raw error included — exits
// through the seam with the location assigned here; the path renders
// only on the error path.
func (tc *typeCoder) decode(d *codecDecoder, target reflect.Value, p pathNode) error {
	err := tc.decodeBody(d, target, p)
	if err == nil {
		return nil
	}
	return d.fail(seamCoder(err, int(d.r.Pos()), p.String()))
}

func (tc *typeCoder) decodeBody(d *codecDecoder, target reflect.Value, p pathNode) error {
	switch tc.ck {
	case ckBigintLegacy:
		s, err := d.r.ReadString()
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkBytes(p); err != nil {
			return err
		}
		b := new(big.Int)
		if err := b.UnmarshalText([]byte(s)); err != nil {
			return d.fail(errCoder(classCoderError, d.r.Pos(), p.String(), err))
		}
		if target.Kind() == reflect.Pointer {
			target.Set(reflect.ValueOf(b))
			return nil
		}
		target.Set(reflect.ValueOf(*b))
		return nil
	case ckTime:
		sec, err := d.r.ReadInt()
		if err != nil {
			return d.mapErr(err)
		}
		nsec, err := d.r.ReadUint()
		if err != nil {
			return d.mapErr(err)
		}
		off, err := d.r.ReadInt()
		if err != nil {
			return d.mapErr(err)
		}
		name, err := d.r.ReadString()
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkBytes(p); err != nil {
			return err
		}
		tt := time.Unix(sec, int64(nsec)).In(zoneFor(name, int(off)))
		if _, zoff := tt.Zone(); zoff != int(off) {
			// the name-resolved zone disagrees with the wire offset (crafted
			// or historical): pin the offset (round-trip contract)
			tt = time.Unix(sec, int64(nsec)).In(time.FixedZone(name, int(off)))
		}
		if target.CanAddr() {
			// addressable targets take the value directly — no interface
			// boxing of the 24-byte struct per decode
			if tp, ok := target.Addr().Interface().(*time.Time); ok {
				*tp = tt
				return nil
			}
		}
		target.Set(reflect.ValueOf(tt))
		return nil
	case ckBinary:
		L, E, _, err := d.r.ReadBlobHeader()
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkLen(L); err != nil {
			return err
		}
		body, err := d.r.ReadRawBytes(E)
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkBytes(p); err != nil {
			return err
		}
		if err := d.chargeAlloc(L, 1, p); err != nil {
			return err
		}
		full := make([]byte, L)
		copy(full, body)
		if err := target.Addr().Interface().(encoding.BinaryUnmarshaler).UnmarshalBinary(full); err != nil {
			return d.fail(errCoder(classCoderError, -1, p.String(), err))
		}
		return nil
	case ckText:
		s, err := d.r.ReadString()
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkBytes(p); err != nil {
			return err
		}
		if err := target.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
			return d.fail(errCoder(classCoderError, -1, p.String(), err))
		}
		return nil
	default:
		// the facade's sub-decode route points at this decoder for the
		// duration of the callback: coder sub-values inherit the stream's
		// depth/nodes/bytes counters (mirror of the encoder's encodeSub)
		fac := d.facade()
		prev := fac.cur
		fac.cur = d
		defer func() { fac.cur = prev }()
		return tc.c.DecodeValue(fac, target)
	}
}

// coderBuildDesc is the encoder-scope descriptor build: the
// single descWalk tree with the coder leaf hook — types with coders in
// scope leave the grammatical walk as CODER leaves carrying per-stream
// encounter-order tags, so nested type-refs always agree with the bodies
// actually emitted.
func (e *codecEncoder) coderBuildDesc(t reflect.Type, path string, cache map[reflect.Type]*wire.Desc) (*wire.Desc, error) {
	return descWalk(t, path, cache, e.coderLeaf, e.wireNaming())
}

// coderLeaf is the encoder's descLeafHook: a type with a coder in scope
// resolves to an opaque CODER leaf; its per-stream tag is
// assigned here on first dispatch.
func (e *codecEncoder) coderLeaf(t reflect.Type) (*wire.Desc, bool) {
	if isBigintType(t) {
		return &wire.Desc{Kind: wire.KindBigint, Name: bigIntWireName}, true
	}
	if e.coderFor(t) == nil {
		return nil, false
	}
	return &wire.Desc{Kind: wire.KindCoder, Name: scopeName(e.wireNaming(), t), Tag: e.coderTag(t)}, true
}
