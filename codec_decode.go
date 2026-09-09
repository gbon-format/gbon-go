package gbon

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unsafe"

	"github.com/gbon-format/gbon-go/internal/wire"
)

// Default decoding budgets: the zero Limits value maps onto these
// conservative per-value caps.
const (
	defaultMaxNodes    = 1000000
	defaultMaxBytes    = 100000000
	defaultMaxMapPairs = 1000000
	defaultMaxSliceLen = 100000000
)

// effLimits resolves the zero fields of l to the default budgets.
func effLimits(l Limits) Limits {
	if l.MaxDepth == 0 {
		l.MaxDepth = maxDepth
	}
	if l.MaxNodes == 0 {
		l.MaxNodes = defaultMaxNodes
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = defaultMaxBytes
	}
	if l.MaxMapPairs == 0 {
		l.MaxMapPairs = defaultMaxMapPairs
	}
	if l.MaxSliceLen == 0 {
		l.MaxSliceLen = defaultMaxSliceLen
	}
	return l
}

// validateLimits rejects negative budgets before any stream consumption
// (not sticky). On the encode side a negative MaxBytes is the documented
// no-cap switch and stays accepted (all other fields on both sides).
func validateLimits(l Limits, isEncode bool) error {
	names := [...]struct {
		name string
		v    int
	}{
		{"MaxDepth", l.MaxDepth},
		{"MaxNodes", l.MaxNodes},
		{"MaxBytes", l.MaxBytes},
		{"MaxMapPairs", l.MaxMapPairs},
		{"MaxSliceLen", l.MaxSliceLen},
	}
	for _, f := range names {
		if isEncode && f.name == "MaxBytes" {
			continue
		}
		if f.v < 0 {
			return errUnsupported(classContractMismatch, "", nil, nil, errDetail(fmt.Sprintf("Limits.%s is negative (%d)", f.name, f.v)))
		}
	}
	return nil
}

type codecDecoder struct {
	r        *wire.Reader
	depth    int
	shared   map[uint64]reflect.Value // stream-shared materialized backings per record id
	reg      map[string]reflect.Type  // scoped concrete-type registry
	coders   map[string]coderEntry    // scoped coder registry
	binds    *coderBinds              // per-stream tag↔name binds
	asName   map[reflect.Type]string  // RegisterAs bindings: type → wire name
	fac      *Decoder                 // public facade for Coder callbacks
	maxDepth int
	maxNodes int
	nodes    int
	maxPairs uint64
	maxBytes int
	start    int                          // stream offset where the current value began (MaxBytes)
	alloc    int                          // charged backing-allocation bytes (derived, booked via chargeAlloc)
	broken   error                        // sticky error of facade sub-decodes inside coder bodies
	skel     *skeletonScratch             // reused canonical-key render state (map ordering)
	flat     map[reflect.Type]*flatLayout // per-value flat-struct layout cache
	ccCache  map[reflect.Type]*typeCoder  // per-value resolved concreteCoder memo (nil = no coder)
}

// skelScratch returns the decoder's skeleton render scratch, built on
// first use.
func (d *codecDecoder) skelScratch() *skeletonScratch {
	if d.skel == nil {
		d.skel = newSkeletonScratch()
	}
	return d.skel
}

// coderEntry is one name-resolved coder: the concrete type to materialize
// (interface positions) plus its typeCoder.
type coderEntry struct {
	typ reflect.Type
	tc  typeCoder
}

// coderBinds enforces the 1:1 tag↔name binding of CODER descriptors per
// stream: a tag rebound to another name or a name rebound to
// another tag is a self-desynchronized stream.
type coderBinds struct {
	tagToName map[uint64]string
	nameToTag map[string]uint64
}

// naming exposes this decoder scope's RegisterAs bindings to name
// resolution points (match, coder lookups).
func (d *codecDecoder) naming() nameOverride {
	return namingOf(d.asName)
}

// newBudgetDecoder builds a per-value codecDecoder with the default budgets
// of eff applied onto r. coders/binds/fac/shared are the stream-scoped
// references shared across values (registry lifecycle): shared is the
// stream's materialized-backing table (mirror — a view or REF from
// value N resolves over a backing materialized in value M < N; nil allocates
// a fresh table for the single-value stateless path).
func newBudgetDecoder(r *wire.Reader, eff Limits, reg map[string]reflect.Type,
	coders map[string]coderEntry, binds *coderBinds, fac *Decoder,
	shared map[uint64]reflect.Value, asName map[reflect.Type]string) *codecDecoder {
	r.MaxDescDepth = eff.MaxDepth
	r.MaxDescNodes = eff.MaxNodes
	r.MaxSliceLen = uint64(eff.MaxSliceLen)
	if binds == nil {
		binds = &coderBinds{}
	}
	if shared == nil {
		shared = make(map[uint64]reflect.Value)
	}
	return &codecDecoder{
		r:        r,
		shared:   shared,
		reg:      reg,
		coders:   coders,
		binds:    binds,
		asName:   asName,
		fac:      fac,
		maxDepth: eff.MaxDepth,
		maxNodes: eff.MaxNodes,
		maxPairs: uint64(eff.MaxMapPairs),
		maxBytes: eff.MaxBytes,
		start:    r.Pos(),
	}
}

// facade returns the public Decoder handed to Coder callbacks (sub-decode
// through Decode); lazily built for the stateless path, where
// custom coders are unreachable.
func (d *codecDecoder) facade() *Decoder {
	if d.fac == nil {
		d.fac = &Decoder{}
	}
	return d.fac
}

// checkBytes enforces the per-value MaxBytes budget — one counter shared
// by input bytes and charged allocation bytes (derived backing
// allocations, chargeAlloc); bulk leaf reads bypass the decodeBody entry
// check, so they re-check after reading.
func (d *codecDecoder) checkBytes(p pathNode) error {
	if d.r.Pos()-d.start+d.alloc > d.maxBytes {
		return d.fail(errBudget(classBudgetBytes, d.r.Pos(), p.String(), nil, d.maxBytes, errDetail(fmt.Sprintf("value consumes more than MaxBytes budget %d", d.maxBytes))))
	}
	return nil
}

// chargeAlloc books a derived backing allocation — l elements of es bytes,
// implicit zero tail [E,L) included (l·es, not E·es) — against the MaxBytes
// budget BEFORE the make, so an oversized request fails without allocating.
func (d *codecDecoder) chargeAlloc(l, es uint64, p pathNode) error {
	n := l * es
	if es != 0 && n/es != l {
		return d.fail(errBudget(classBudgetBytes, d.r.Pos(), p.String(), nil, d.maxBytes, errDetail(fmt.Sprintf("allocation of %d×%d bytes exceeds MaxBytes budget %d", l, es, d.maxBytes))))
	}
	used := d.r.Pos() - d.start + d.alloc
	if used > d.maxBytes || n > uint64(d.maxBytes-used) {
		return d.fail(errBudget(classBudgetBytes, d.r.Pos(), p.String(), nil, d.maxBytes, errDetail(fmt.Sprintf("allocation of %d bytes exceeds MaxBytes budget %d", n, d.maxBytes))))
	}
	d.alloc += int(n)
	return nil
}

// fail is the decode-error choke point: in trusted-input mode it captures
// the input window around the error's offset — 32 bytes before, 16 after,
// clamped to the reader's buffered bytes — and attaches it to the error
// (the copy owns its bytes; the error never aliases the sliding buffer).
// Outside trusted mode, or for an error without an input offset, it is a
// pass-through: the default error path is unchanged. Capture never reads
// the underlying source — only bytes already pulled into the buffer.
func (d *codecDecoder) fail(err error) error {
	if d == nil || d.fac == nil || !d.fac.trusted {
		return err
	}
	ae, ok := err.(*Error)
	if !ok || ae.Offset < 0 || ae.snippetSet {
		return err
	}
	win, start := d.r.Window(ae.Offset, snippetBefore, snippetAfter)
	ae.snippet = append([]byte(nil), win...)
	ae.snippetOff = start
	ae.snippetSet = true
	return err
}

// mapErr lifts a wire-layer error into the core error type: the class
// comes from the sentinel family of the cause (budget errors as budget
// classes, format errors as data/format classes; EOF-shaped faults stay
// truncated data), with the input offset. Any other fault surfacing here
// is a failed read on the underlying input source (io.ReadFull paths in
// the wire reader return raw non-EOF reader errors): attributed to the
// environment as io_read with the original fault as the cause.
func (d *codecDecoder) mapErr(err error) error {
	off := int(d.r.Pos())
	if we, ok := errors.AsType[*wire.Error](err); ok {
		if we.Off >= 0 {
			off = we.Off
		}
		switch classSentinels[we.Kind] {
		case ErrBudget:
			return d.fail(errBudget(we.Kind, off, "", we.Got, we.Want, errDetail(we.Msg)))
		case ErrUnsupported:
			return errUnsupported(we.Kind, "", we.Got, we.Want, errDetail(we.Msg))
		default:
			return d.fail(errFormat(we.Kind, off, "", we.Got, we.Want, errDetail(we.Msg)))
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return d.fail(errFormat(classTruncated, off, "", nil, nil, errDetail(err.Error())))
	}
	return d.fail(errIO(off, err))
}

// withOffset fills the decode offset onto a core error raised without
// input context (descriptor match faults surface through callers that
// own the reader position).
func withOffset(err error, off int) error {
	var ae *Error
	if errors.As(err, &ae) && ae.Offset < 0 {
		ae.Offset = off
	}
	return err
}

// allocPanicError converts a decode-time panic into an ErrBudget wrap: a crafted backing
// length admitted by user-raised limits panics at the allocation; a non-allocation bug
// surfacing as ErrBudget is a registered limitation; stack exhaustion is prevented by depth budgets.
func (d *codecDecoder) allocPanicError(p any) error {
	return d.fail(errBudget(classBudgetAlloc, -1, "", nil, nil, errDetail(fmt.Sprintf("allocation during decode exceeded limits: %v", p))))
}

// codecUnmarshal decodes one self-contained stream into the value pointed to
// by v. v must be a non-nil pointer (misuse is an
// ErrUnsupported error, never a panic).
func codecUnmarshal(data []byte, v any) (err error) {
	target, err := decodeTarget(v)
	if err != nil {
		return err
	}
	var rootRestore func()
	var d *codecDecoder
	defer func() {
		if p := recover(); p != nil {
			err = d.allocPanicError(p)
		}
		if rootRestore != nil && err != nil {
			rootRestore()
		}
	}()
	d = newBudgetDecoder(wire.NewReader(data), effLimits(Limits{}), newBasicReg(), nil, nil, nil, nil, nil)
	if _, _, err := d.r.ReadHeader(); err != nil {
		return d.mapErr(err)
	}
	desc, err := d.r.ReadDesc()
	if err != nil {
		return d.mapErr(err)
	}
	pd, err := d.unwrapRootNamed(desc)
	if err != nil {
		return err
	}
	if rootDerefApplies(pd, target) {
		return d.decodeRootPointer(pd, target, func(f func()) { rootRestore = f })
	}
	if err := matchRootDesc(desc, target.Type(), nil); err != nil {
		return d.fail(withOffset(err, int(d.r.Pos())))
	}
	// Root staging (decode atomicity): the whole value decodes into a
	// codec-owned copy; the single target.Set on success is the commit
	// point of the call — on any error the target keeps its pre-call state.
	tmp := reflect.New(target.Type()).Elem()
	if err := d.decodeBody(desc, tmp, pathNode{idx: -1}); err != nil {
		return d.fail(withOffset(err, int(d.r.Pos())))
	}
	target.Set(tmp)
	return nil
}

// matchRootDesc validates the root descriptor: concrete streams into an
// interface target defer to the registry resolution.
func matchRootDesc(desc *wire.Desc, t reflect.Type, naming nameOverride) error {
	if t.Kind() == reflect.Interface && desc.Kind != wire.KindInterface {
		return nil
	}
	return matchDescNamed(desc, t, naming)
}

// derefNamed walks a NAMED-wrapper chain ed -> Refs[0] -> ... down to
// the first non-NAMED descriptor, reporting the wrapper count. The
// out-degree-1 chain is cycle-checked Floyd-style (tortoise-hare
// pointer comparison, zero allocation): readDescLit interns a
// descriptor before its name and body, so a crafted REF can legally
// close a self-loop on the wire — a degenerate NAMED cycle is reported
// as ok=false to the caller. The walk pass mirrors the loop's hop
// counting and budget charges; the detection pass only reads.
func derefNamed(ed *wire.Desc) (*wire.Desc, int, bool) {
	slow, fast := ed, ed
	for fast.Kind == wire.KindNamed {
		fast = fast.Refs[0]
		if fast.Kind != wire.KindNamed {
			break
		}
		fast = fast.Refs[0]
		slow = slow.Refs[0]
		if slow == fast {
			return nil, 0, false
		}
	}
	hops := 0
	for ed.Kind == wire.KindNamed {
		ed = ed.Refs[0]
		hops++
	}
	return ed, hops, true
}

// unwrapRootNamed strips leading NAMED wrappers off a root descriptor:
// the root deref applies through a NAMED-wrapped root pointer. A
// degenerate NAMED cycle (crafted self-referential wrapper) is a format
// error, never a hang.
func (d *codecDecoder) unwrapRootNamed(desc *wire.Desc) (*wire.Desc, error) {
	dsc, _, ok := derefNamed(desc)
	if !ok {
		return nil, d.fail(errFormat(classBadRef, int(d.r.Pos()), "", nil, nil, errDetail("root descriptor closes a degenerate NAMED cycle")))
	}
	return dsc, nil
}

// decodeRootPointer handles a root pointer descriptor (`*T`, post-NAMED)
// against a concrete non-pointer target T: exactly one deref, top entry
// only (R1). A nil root selector is a loud format error; a REF root
// resolves to the interned cell and copies its pointee. Otherwise the
// pointee body materializes in place into the caller's cell (R2): the cell
// registers in the intern space before the body decodes (two-phase mirror
// of the encoder's reserve; zero-size pointees are not tracked), so every
// REF-to-root inside the value resolves to the caller's storage. Atomicity
// is snapshot-restore: setRestore installs the rollback before any byte
// reaches the target, and the caller's defer invokes it on every error
// path, including the recovered-panic budget path.
func (d *codecDecoder) decodeRootPointer(pd *wire.Desc, target reflect.Value, setRestore func(func())) error {
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	switch class {
	case wire.ClassNil:
		if _, err := d.r.ReadNil(); err != nil {
			return d.mapErr(err)
		}
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), "", target.Type(), nil, errDetail("nil root pointer cannot dereference into target")))
	case wire.ClassRef:
		id, err := d.r.ReadRef()
		if err != nil {
			return d.mapErr(err)
		}
		pv, err := d.r.ValueAt(id)
		if err != nil {
			return d.mapErr(err)
		}
		if !pv.IsValid() || pv.Type() != reflect.PointerTo(target.Type()) {
			return d.fail(errFormat(classBadRef, d.r.Pos(), "", nil, nil, errDetail(fmt.Sprintf("root ref %d is not a *%s value", id, target.Type()))))
		}
		target.Set(pv.Elem())
		return nil
	default:
		if err := matchDescNamed(pd.Refs[0], target.Type(), d.naming()); err != nil {
			return d.fail(withOffset(err, int(d.r.Pos())))
		}
		snap := reflect.New(target.Type()).Elem()
		snap.Set(target)
		setRestore(func() { target.Set(snap) })
		if target.Type().Size() != 0 {
			d.r.RegisterValue(target.Addr())
		}
		return d.decodeBody(pd.Refs[0], target, pathNode{idx: -1})
	}
}

// rootDerefApplies reports whether the R1 root-unwrap branch covers the
// pair: a pointer descriptor (post-NAMED) into a concrete non-pointer,
// non-interface target. Pointer targets keep the direct match; interface
// targets keep the registry resolution path
func rootDerefApplies(pd *wire.Desc, target reflect.Value) bool {
	if pd.Kind != wire.KindPointer {
		return false
	}
	switch target.Kind() {
	case reflect.Interface, reflect.Pointer:
		return false
	}
	return true
}

// decodeTarget validates the Unmarshal/Decode destination.
func decodeTarget(v any) (reflect.Value, error) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, errUnsupported(classContractMismatch, "", nil, nil, errDetail(fmt.Sprintf("target must be a non-nil pointer, got %T", v)))
	}
	return rv.Elem(), nil
}

// decodeSub is the coder-callback Decode path (mirror of the encoder's
// encodeSub): the sub-value decodes through THIS decoder, so the
// depth/nodes/bytes windows stay cumulative across the coder boundary —
// recursion through the facade exhausts a budget instead of the stack.
// A target error leaves the stream usable; a stream error is sticky.
func (d *codecDecoder) decodeSub(v any) error {
	if d.broken != nil {
		return d.broken
	}
	target, err := decodeTarget(v)
	if err != nil {
		return err
	}
	if d.r.AtEnd() {
		return io.EOF
	}
	cd, err := d.r.ReadDesc()
	if err != nil {
		d.broken = d.mapErr(err)
		return d.broken
	}
	if err := matchRootDesc(cd, target.Type(), d.naming()); err != nil {
		err = d.fail(withOffset(err, int(d.r.Pos())))
		d.broken = err
		return err
	}
	if err := d.decodeBody(cd, target, pathNode{idx: -1}); err != nil {
		d.broken = err
		return err
	}
	return nil
}

// codecStreamDecoder decodes a multi-value stream (Decoder facade).
type codecStreamDecoder struct {
	r       *wire.Reader
	started bool
	// sawValue records that at least one value has been decoded: the
	// header is written with the first value (lazy encode), so input
	// ending right after the header was truncated before any value —
	// AtEOF before sawValue is malformed input, not a clean end.
	sawValue bool
	broken   error
	reg      map[string]reflect.Type
	coders   map[string]coderEntry
	asName   map[reflect.Type]string
	binds    *coderBinds
	fac      *Decoder
	shared   map[uint64]reflect.Value // stream-scoped materialized backings per record id
}

// init prepares the stream decoder: the source is wrapped for buffered
// pull-based reading and the stream header is validated (first Decode).
// Records decode incrementally: nothing beyond the in-flight record (plus
// lookahead) is held in memory.
func (d *codecStreamDecoder) init(src io.Reader) error {
	switch src.(type) {
	case *bufio.Reader, *bytes.Reader, *bytes.Buffer, *strings.Reader:
	default:
		src = bufio.NewReaderSize(src, 1<<16)
	}
	d.r = wire.NewStreamReader(src)
	d.r.MaxDescDepth = maxDepth
	d.started = true
	if d.binds == nil {
		d.binds = &coderBinds{}
	}
	if d.shared == nil {
		d.shared = make(map[uint64]reflect.Value)
	}
	if _, _, err := d.r.ReadHeader(); err != nil {
		return d.mapErrInit(err)
	}
	return nil
}

func (d *codecStreamDecoder) mapErrInit(err error) error {
	dd := codecDecoder{r: d.r, fac: d.fac}
	return dd.mapErr(err)
}

// Decode reads one value from the stream into the value pointed to by v.
// l carries the Decoder budgets: zero fields resolve to the default budgets
// before touching the wire (the MaxSliceLen bridge still applies); the end of
// stream yields io.EOF. After a decode error the decoder is
// invalid: subsequent calls fail without reading (sticky-error).
func (d *codecStreamDecoder) Decode(v any, l Limits) (err error) {
	if d.broken != nil {
		return d.broken
	}
	target, err := decodeTarget(v)
	if err != nil {
		return err
	}
	if d.r.AtEOF() {
		if d.sawValue {
			return io.EOF
		}
		// No value was ever decoded and the input ends right after
		// the header: a legit encoder writes the header together with
		// the first value, so this is a truncated stream, not the
		// clean end of one. Typed truncated error; sticky through the
		// facade (non-EOF branch).
		err := d.mapErrInit(io.EOF)
		d.broken = err
		return err
	}
	eff := effLimits(l)
	var rootRestore func()
	var dec *codecDecoder
	defer func() {
		if p := recover(); p != nil {
			err = dec.allocPanicError(p)
		}
		if rootRestore != nil && err != nil {
			rootRestore()
		}
	}()
	dec = newBudgetDecoder(d.r, eff, d.reg, d.coders, d.binds, d.fac, d.shared, d.asName)
	desc, err := d.r.ReadDesc()
	if err != nil {
		d.broken = dec.mapErr(err)
		return d.broken
	}
	pd, err := dec.unwrapRootNamed(desc)
	if err != nil {
		d.broken = err
		return err
	}
	if rootDerefApplies(pd, target) {
		if err := dec.decodeRootPointer(pd, target, func(f func()) { rootRestore = f }); err != nil {
			d.broken = err
			return err
		}
		d.sawValue = true
		d.r.Discard()
		return nil
	}
	if err := matchRootDesc(desc, target.Type(), namingOf(d.asName)); err != nil {
		err = dec.fail(withOffset(err, int(d.r.Pos())))
		d.broken = err
		return err
	}
	// Root staging (decode atomicity): one commit-point target.Set on
	// success; on any error the target keeps its pre-call state.
	tmp := reflect.New(target.Type()).Elem()
	if err := dec.decodeBody(desc, tmp, pathNode{idx: -1}); err != nil {
		d.broken = err
		return err
	}
	target.Set(tmp)
	d.sawValue = true
	d.r.Discard()
	return nil
}

// decodeBody reads value-body tokens for one position (opcode table). The value
// recursion depth is capped by maxDepth.
func (d *codecDecoder) decodeBody(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if target.Kind() == reflect.Interface && desc.Kind != wire.KindInterface {
		return d.resolveConcrete(desc, target, p)
	}
	d.depth++
	defer func() { d.depth-- }()
	if d.depth > d.maxDepth {
		return d.fail(errBudget(classBudgetDepth, d.r.Pos(), p.String(), nil, d.maxDepth, errDetail(fmt.Sprintf("value nesting exceeds depth budget MaxDepth=%d", d.maxDepth))))
	}
	d.nodes++
	if d.nodes > d.maxNodes {
		return d.fail(errBudget(classBudgetNodes, d.r.Pos(), p.String(), nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
	}
	if err := d.checkBytes(p); err != nil {
		return err
	}
	switch desc.Kind {
	case wire.KindNamed:
		return d.decodeBody(desc.Refs[0], target, p)
	case wire.KindBool:
		b, err := d.r.ReadBool()
		if err != nil {
			return d.mapErr(err)
		}
		target.SetBool(b)
		return nil
	case wire.KindInt:
		n, err := d.r.ReadInt()
		if err != nil {
			return d.mapErr(err)
		}
		if !fitsSigned(target.Type(), n) {
			return d.fail(errFormat(classOverflowValue, d.r.Pos(), p.String(), n, target.Type(), errDetail(fmt.Sprintf("value overflows %s", target.Type()))))
		}
		target.SetInt(n)
		return nil
	case wire.KindUint:
		u, err := d.r.ReadUint()
		if err != nil {
			return d.mapErr(err)
		}
		if !fitsUnsigned(target.Type(), u) {
			return d.fail(errFormat(classOverflowValue, d.r.Pos(), p.String(), u, target.Type(), errDetail(fmt.Sprintf("value overflows %s", target.Type()))))
		}
		target.SetUint(u)
		return nil
	case wire.KindFloat:
		switch desc.Width {
		case 4:
			f, err := d.r.ReadFloat32()
			if err != nil {
				return d.mapErr(err)
			}
			target.SetFloat(float64(f))
		case 8:
			f, err := d.r.ReadFloat64()
			if err != nil {
				return d.mapErr(err)
			}
			target.SetFloat(f)
		default:
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), "", nil, nil, errDetail(fmt.Sprintf("float width %d invalid", desc.Width))))
		}
		return nil
	case wire.KindComplex:
		switch desc.Width {
		case 4:
			c, err := d.r.ReadComplex64()
			if err != nil {
				return d.mapErr(err)
			}
			target.SetComplex(complex128(c))
		case 8:
			c, err := d.r.ReadComplex128()
			if err != nil {
				return d.mapErr(err)
			}
			target.SetComplex(c)
		default:
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), "", nil, nil, errDetail(fmt.Sprintf("complex width %d invalid", desc.Width))))
		}
		return nil
	case wire.KindString:
		s, err := d.r.ReadString()
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkBytes(p); err != nil {
			return err
		}
		target.SetString(s)
		return nil
	case wire.KindBlob:
		return d.decodeBlob(desc, target, p)
	case wire.KindSlice:
		return d.decodeSlice(desc, target, p)
	case wire.KindArray:
		return d.decodeArray(desc, target, p)
	case wire.KindMap:
		return d.decodeMap(desc, target, p)
	case wire.KindStruct:
		return d.decodeStruct(desc, target, p)
	case wire.KindPointer:
		return d.decodePointer(desc, target, p)
	case wire.KindInterface:
		return d.resolveIface(desc, target, p)
	case wire.KindCoder:
		return d.decodeCoder(desc, target, p)
	case wire.KindBigint:
		return d.decodeBigint(desc, target, p)
	}
	return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("unknown descriptor kind %d", desc.Kind))))
}

// decodeBigint materializes a BIGINT position: the
// value body is one bare argument — inline/width below 2^64, the ext form
// at and above. The zero/nil coincidence byte materializes
// by projection: nil for pointer and interface shapes, zero for value
// shapes. The advertised ext length is budget-gated before any
// allocation.
func (d *codecDecoder) decodeBigint(desc *wire.Desc, target reflect.Value, p pathNode) error {
	n, err := d.r.ReadBigint(uint64(d.maxBytes))
	if err != nil {
		return d.mapErr(err)
	}
	if err := d.checkBytes(p); err != nil {
		return err
	}
	if n == nil {
		target.Set(reflect.Zero(target.Type()))
		return nil
	}
	if target.Kind() == reflect.Pointer {
		target.Set(reflect.ValueOf(n))
		return nil
	}
	target.Set(reflect.ValueOf(*n))
	return nil
}

// bindCoder records the CODER descriptor's tag↔name pair.
func (d *codecDecoder) bindCoder(desc *wire.Desc, p pathNode) error {
	if n, ok := d.binds.tagToName[desc.Tag]; ok && n != desc.Name {
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), desc.Name, n, errDetail(fmt.Sprintf("coder tag %d rebound", desc.Tag))))
	}
	if t, ok := d.binds.nameToTag[desc.Name]; ok && t != desc.Tag {
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), desc.Name, nil, errDetail(fmt.Sprintf("coder name rebound from tag %d to %d", t, desc.Tag))))
	}
	if d.binds.tagToName == nil {
		d.binds.tagToName = make(map[uint64]string)
		d.binds.nameToTag = make(map[string]uint64)
	}
	d.binds.tagToName[desc.Tag] = desc.Name
	d.binds.nameToTag[desc.Name] = desc.Tag
	return nil
}

// lookupCoderEntry resolves a coder name for an interface position
// (resolution order): built-in time.Time, the RegisterCoder scope, then the
// plain Register registry for adapter-eligible types, mirroring the encoder-side resolution order;
// a plain-registered type without any coder is a miss.
func (d *codecDecoder) lookupCoderEntry(name string) (coderEntry, bool) {
	if name == nameOf(timeType) {
		return coderEntry{typ: timeType, tc: typeCoder{ck: ckTime}}, true
	}
	if name == bigIntWireName {
		// Kind-14 channel: the adapter STRING body, accepted on decode,
		// materializing *big.Int.
		return coderEntry{typ: bigIntPtrType, tc: typeCoder{ck: ckBigintLegacy}}, true
	}
	if ent, ok := d.coders[name]; ok {
		return ent, true
	}
	// RegisterAs resolution: a bound wire name maps through the registry
	// to the local type, whose coder stays keyed by its canonical name.
	if t, ok := d.reg[name]; ok {
		if ent, ok := d.coders[nameOf(t)]; ok && ent.typ == t {
			return ent, true
		}
		if ak := adapterOf(t); ak != ckNone {
			return coderEntry{typ: t, tc: typeCoder{ck: ak}}, true
		}
	}
	return coderEntry{}, false
}

// concreteCoder resolves the coder for a concrete target type by coder-name
// precedence: built-in > registered (same type, keyed by the type's
// canonical name — a RegisterAs binding renames the stream side only) >
// adapters.
func (d *codecDecoder) concreteCoder(t reflect.Type, name string) *typeCoder {
	if tc, ok := d.ccCache[t]; ok {
		return tc
	}
	var tc *typeCoder
	switch {
	case t == timeType:
		tc = &typeCoder{ck: ckTime}
	case isBigintType(t):
		tc = &typeCoder{ck: ckBigint}
	default:
		if ent, ok := d.coders[nameOf(t)]; ok && ent.typ == t {
			c := ent.tc
			tc = &c
		} else if ak := adapterOf(t); ak != ckNone {
			tc = &typeCoder{ck: ak}
		}
	}
	if d.ccCache == nil {
		d.ccCache = make(map[reflect.Type]*typeCoder)
	}
	d.ccCache[t] = tc
	return tc
}

// decodeCoder decodes a CODER-descriptor position: bind the tag,
// resolve through the coder registry for interface targets, or by target
// precedence for concrete targets.
func (d *codecDecoder) decodeCoder(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if err := d.bindCoder(desc, p); err != nil {
		return err
	}
	if target.Kind() == reflect.Interface {
		return d.resolveCoder(desc, target, p)
	}
	if name := scopeName(d.naming(), target.Type()); name != desc.Name &&
		!(desc.Name == bigIntWireName && isBigintType(target.Type())) {
		return d.fail(errFormat(classTypeMismatch, d.r.Pos(), p.String(), desc.Name, name, errDetail("type mismatch")))
	}
	tc := d.concreteCoder(target.Type(), desc.Name)
	if tc == nil {
		return d.fail(errFormat(classUnknownName, d.r.Pos(), p.String(), desc.Name, nil, errDetail("no coder: use Decoder.RegisterCoder")))
	}
	if tc.ck == ckBigint && desc.Kind == wire.KindCoder {
		// Kind-14 "big.Int" streams: the adapter STRING body stays
		// accepted on decode; dispatch is by descriptor kind, not by name.
		tc = &typeCoder{ck: ckBigintLegacy}
	}
	return d.decodeCoderInto(tc, target, p)
}

// resolveCoder materializes a coder-coded concrete dynamic value behind an
// interface position through the coder registry.
func (d *codecDecoder) resolveCoder(cd *wire.Desc, target reflect.Value, p pathNode) error {
	ent, ok := d.lookupCoderEntry(cd.Name)
	if !ok {
		return d.fail(errFormat(classUnknownName, d.r.Pos(), p.String(), cd.Name, nil, errDetail("custom coder not registered: use Decoder.RegisterCoder")))
	}
	rt := ent.typ
	if st := target.Type(); st.Kind() == reflect.Interface && st.NumMethod() > 0 && !rt.Implements(st) {
		return errUnsupported(classContractMismatch, "", nil, nil, errDetail(fmt.Sprintf("coder type %s does not implement %s", rt, st)))
	}
	cv := reflect.New(rt).Elem()
	if err := d.decodeCoderInto(&ent.tc, cv, p); err != nil {
		return err
	}
	target.Set(cv)
	return nil
}

// decodeCoderInto charges the node/bytes budgets around the coder body:
// the outer cumulative MaxBytes is enforced after the body, so
// sub-decode budget windows cannot leak total consumption.
func (d *codecDecoder) decodeCoderInto(tc *typeCoder, target reflect.Value, p pathNode) error {
	d.nodes++
	if d.nodes > d.maxNodes {
		return d.fail(errBudget(classBudgetNodes, d.r.Pos(), p.String(), nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
	}
	if err := d.checkBytes(p); err != nil {
		return err
	}
	if err := tc.decode(d, target, p); err != nil {
		return err
	}
	return d.checkBytes(p)
}

// resolveIface decodes one interface position: a nil-interface
// selector yields the zero interface (typed nils arrive as a concrete
// descriptor plus the concrete kind's nil token); a concrete tag resolves
// through the scoped registry — miss is a registry-contract ErrUnsupported,
// a structural mismatch is ErrFormat — then Implements gates non-empty
// static interfaces before the concrete body materializes.
func (d *codecDecoder) resolveIface(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if d.r.IsNextNil() {
		k, err := d.r.ReadNil()
		if err != nil {
			return d.mapErr(err)
		}
		if k != wire.NilInterface {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for interface", k))))
		}
		target.Set(reflect.Zero(target.Type()))
		return nil
	}
	cd, err := d.r.ReadDesc()
	if err != nil {
		return d.mapErr(err)
	}
	return d.resolveConcrete(cd, target, p)
}

// resolveConcrete materializes a concrete dynamic value behind an already
// read tag cd (the root position hands the tag in directly).
func (d *codecDecoder) resolveConcrete(cd *wire.Desc, target reflect.Value, p pathNode) error {
	if d.reg == nil {
		return d.fail(errFormat(classUnknownName, d.r.Pos(), p.String(), cd.Name, nil, errDetail("interface concrete type needs a type registry: use Decoder.Register")))
	}
	rt, ok := d.reg[cd.Name]
	if !ok {
		return d.fail(errFormat(classUnknownName, d.r.Pos(), p.String(), cd.Name, nil, errDetail("interface concrete type not registered: use Decoder.Register")))
	}
	if err := matchDescNamed(cd, rt, d.naming()); err != nil {
		return d.fail(withOffset(err, int(d.r.Pos())))
	}
	if st := target.Type(); st.Kind() == reflect.Interface && st.NumMethod() > 0 && !rt.Implements(st) {
		return errUnsupported(classContractMismatch, "", nil, nil, errDetail(fmt.Sprintf("registered type %s does not implement %s", rt, st)))
	}
	cv := reflect.New(rt).Elem()
	if err := d.decodeBody(cd, cv, p); err != nil {
		return err
	}
	target.Set(cv)
	return nil
}

// fitsSigned range-checks n against the target integer's width derived
// from reflect.Type.Size() — platform-independent: a Go int with width 4
// (32-bit build) gets the int32 range, width 8 the int64 range, whatever
// the host running this code. Checked before any mutation (width
// validation).
func fitsSigned(t reflect.Type, n int64) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
	default:
		return true
	}
	switch t.Size() {
	case 1:
		return n >= math.MinInt8 && n <= math.MaxInt8
	case 2:
		return n >= math.MinInt16 && n <= math.MaxInt16
	case 4:
		return n >= math.MinInt32 && n <= math.MaxInt32
	default:
		return true // width 8: n already spans the full range
	}
}

// fitsUnsigned reports whether u fits the target unsigned integer's width
// derived from reflect.Type.Size() (width 4 → MaxUint32 even when the
// host int is 8 bytes), before any value is stored.
func fitsUnsigned(t reflect.Type, u uint64) bool {
	switch t.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
	default:
		return true
	}
	switch t.Size() {
	case 1:
		return u <= math.MaxUint8
	case 2:
		return u <= math.MaxUint16
	case 4:
		return u <= math.MaxUint32
	default:
		return true // width 8: u already spans the full range
	}
}

// checkLen bounds a backing length before allocation (uint64 → int safety).
func (d *codecDecoder) checkLen(L uint64) error {
	if L > math.MaxInt {
		return d.fail(errBudget(classBudgetNodes, -1, "", nil, nil, errDetail(fmt.Sprintf("backing length %d exceeds addressable memory", L))))
	}
	return nil
}

// viewWindow re-validates the view geometry {0 ≤ off ≤ off+len ≤
// off+cap ≤ L} against the actual materialized backing — the direct
// view→backing correlation, before any reflect windowing:
// a crafted window past the backing is ErrFormat,
// never a panic and never a budget wrap.
func (d *codecDecoder) viewWindow(backing reflect.Value, view wire.View, p pathNode) (reflect.Value, error) {
	L := uint64(backing.Len())
	if view.Off+view.Len < view.Off || view.Off+view.Cap < view.Off ||
		view.Len > view.Cap || view.Off+view.Cap > L {
		return reflect.Value{}, errFormat(classBadView, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("view geometry off=%d len=%d cap=%d exceeds backing L=%d", view.Off, view.Len, view.Cap, L)))
	}
	return backing.Slice3(int(view.Off), int(view.Off+view.Len), int(view.Off+view.Cap)), nil
}

// decodeBlob reconstructs a []byte: on the record's first encounter the
// backing via make(L) with [0,E) filled is cached under the record id; every
// view is a window over that one backing.
func (d *codecDecoder) decodeBlob(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if d.r.IsNextNil() {
		if k, err := d.r.ReadNil(); err != nil {
			return d.mapErr(err)
		} else if k != wire.NilSlice {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for slice", k))))
		}
		target.Set(reflect.Zero(target.Type()))
		return nil
	}
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	if class == wire.ClassView {
		view, err := d.r.ReadView()
		if err != nil {
			return d.mapErr(err)
		}
		backing, ok := d.shared[view.ID]
		// assignability, not identity: the encoder groups windows of one
		// allocation regardless of named/unnamed byte-slice type, so a
		// backing materialized as []byte serves a named Raw target and
		// vice versa (Go assignability: one side unnamed)
		if !ok || !backing.Type().AssignableTo(target.Type()) {
			return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("shared backing %d unavailable for %s", view.ID, target.Type()))))
		}
		win, err := d.viewWindow(backing, view, p)
		if err != nil {
			return err
		}
		target.Set(win)
		return nil
	}
	L, E, id, err := d.r.ReadBlobHeader()
	if err != nil {
		return d.mapErr(err)
	}
	if err := d.checkLen(L); err != nil {
		return err
	}
	if err := d.chargeAlloc(L, 1, p); err != nil {
		return err
	}
	backing := reflect.MakeSlice(target.Type(), int(L), int(L))
	// Record-then-fill: the backing registers before its body, so a view
	// over this record resolves even from inside the body's own stream
	// positions.
	d.shared[id] = backing
	body, err := d.r.ReadRawBytes(E)
	if err != nil {
		return d.mapErr(err)
	}
	// Dense-prefix minimality: the last dense byte
	// MUST be nonzero — a zero byte there is elision fodder, and E is
	// the one-past-the-last-dense-byte position.
	if E > 0 && body[E-1] == 0 {
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("blob dense prefix E=%d ends in a zero byte", E))))
	}
	if err := d.checkBytes(p); err != nil {
		return err
	}
	reflect.Copy(backing, reflect.ValueOf(body))
	view, err := d.r.ReadView()
	if err != nil {
		return d.mapErr(err)
	}
	win, err := d.viewWindow(backing, view, p)
	if err != nil {
		return err
	}
	target.Set(win)
	return nil
}

// decodeSlice reconstructs a []T: the ARRAY record's backing (MakeSlice(L,L),
// elements [0,E) decoded) is cached under the record id before the element
// cycle; first and repeat views are Slice3 windows over one backing, and a
// view over this record from inside the element list (slice cycles through
// interfaces) resolves against the mid-fill backing.
func (d *codecDecoder) decodeSlice(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if d.r.IsNextNil() {
		if k, err := d.r.ReadNil(); err != nil {
			return d.mapErr(err)
		} else if k != wire.NilSlice {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for slice", k))))
		}
		target.Set(reflect.Zero(target.Type()))
		return nil
	}
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	if class == wire.ClassView {
		view, err := d.r.ReadView()
		if err != nil {
			return d.mapErr(err)
		}
		backing, ok := d.shared[view.ID]
		// assignability, not identity — same rule as decodeBlob's views
		if !ok || !backing.Type().AssignableTo(target.Type()) {
			return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("shared backing %d unavailable for %s", view.ID, target.Type()))))
		}
		win, err := d.viewWindow(backing, view, p)
		if err != nil {
			return err
		}
		target.Set(win)
		return nil
	}
	L, E, id, err := d.r.ReadArrayHeader()
	if err != nil {
		return d.mapErr(err)
	}
	if err := d.checkLen(L); err != nil {
		return err
	}
	if err := d.chargeAlloc(L, uint64(target.Type().Elem().Size()), p); err != nil {
		return err
	}
	backing := reflect.MakeSlice(target.Type(), int(L), int(L))
	// Record-then-fill: the backing registers before its elements, so a
	// view over this record from inside the element list resolves
	d.shared[id] = backing
	if err := d.decodeElems(desc.Refs[0], backing, E, p); err != nil {
		return err
	}
	// Dense-prefix minimality: element [E−1] MUST NOT
	// be bitwise zero — a zero element there is elision fodder (the
	// encoder predicate is bit-level, so −0.0 stays dense and never
	// trips this check).
	if E > 0 && isBitZero(backing.Index(int(E-1))) {
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("slice dense prefix E=%d ends in a zero element", E))))
	}
	view, err := d.r.ReadView()
	if err != nil {
		return d.mapErr(err)
	}
	win, err := d.viewWindow(backing, view, p)
	if err != nil {
		return err
	}
	target.Set(win)
	return nil
}

// decodeArray reconstructs an [N]T value: one ARRAY record, no view; the
// record length must equal the descriptor/target length.
func (d *codecDecoder) decodeArray(desc *wire.Desc, target reflect.Value, p pathNode) error {
	L, E, _, err := d.r.ReadArrayHeader()
	if err != nil {
		return d.mapErr(err)
	}
	if L != uint64(target.Type().Len()) {
		return d.fail(errFormat(classTypeMismatch, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("array length %d does not match target %s", L, target.Type()))))
	}
	if err := d.checkLen(L); err != nil {
		return err
	}
	// Decode-into-temp-then-assign (decode atomicity): elements stage in
	// a codec-owned array; the single target.Set on success is the commit
	// point — an error in any element leaves the target untouched.
	tmp := reflect.New(target.Type()).Elem()
	if err := d.decodeElems(desc.Refs[0], tmp, E, p); err != nil {
		return err
	}
	// Dense-prefix minimality: element [E−1] MUST NOT
	// be bitwise zero (same predicate as the slice record path).
	if E > 0 && isBitZero(tmp.Index(int(E-1))) {
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("array dense prefix E=%d ends in a zero element", E))))
	}
	target.Set(tmp)
	return nil
}

// decodeElems decodes the E dense elements of an ARRAY record into dst
// (the slice backing or the array staging value). Primitive element
// descriptors take a per-record batch loop — one kind dispatch per
// record instead of one decodeBody frame per element — with budget
// semantics identical to the per-element frames: nodes accrue per
// element (including one per NAMED wrapper hop), MaxBytes re-checks
// before every element read, and the depth window is checked once for
// the uniform element depth.
func (d *codecDecoder) decodeElems(ed *wire.Desc, dst reflect.Value, E uint64, p pathNode) error {
	if E == 0 {
		return nil
	}
	ed, hops, ok := derefNamed(ed)
	if !ok {
		return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail("element descriptor closes a degenerate NAMED cycle")))
	}
	switch ed.Kind {
	case wire.KindInt, wire.KindUint, wire.KindFloat:
		return d.decodePrimElems(ed, dst, E, hops, p)
	}
	for i := range E {
		en := pathNode{parent: &p, idx: int(i)}
		if err := d.decodeElemFrame(ed, dst.Index(int(i)), i, hops, en); err != nil {
			return err
		}
	}
	return nil
}

// decodeElemFrame runs one non-primitive element through the regular
// decodeBody after booking the NAMED wrapper frames it would have
// consumed (node count and depth are per-frame properties).
func (d *codecDecoder) decodeElemFrame(ed *wire.Desc, target reflect.Value, i uint64, hops int, en pathNode) error {
	for range hops {
		d.nodes++
		if d.nodes > d.maxNodes {
			return d.fail(errBudget(classBudgetNodes, d.r.Pos(), en.String(), nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
		}
		if err := d.checkBytes(en); err != nil {
			return err
		}
	}
	return d.decodeBody(ed, target, en)
}

// primSlot is one staged primitive element for the flat-struct and batch
// paths: numeric bits in u, string payload in s.
type primSlot struct {
	u uint64
	s string
}

// elemBase returns the element-0 pointer of a slice backing or an
// addressable array staging value for the typed batch loops.
func elemBase(dst reflect.Value) unsafe.Pointer {
	if dst.Kind() == reflect.Array {
		return dst.Addr().UnsafePointer()
	}
	return dst.UnsafePointer()
}

// decodePrimElems reads E primitive elements of kind ed directly into
// dst. Width-8 integer and matching-width float elements go through
// typed slices (no reflect per element); every other width keeps the
// per-element fits checks of the generic decodeBody path.
func (d *codecDecoder) decodePrimElems(ed *wire.Desc, dst reflect.Value, E uint64, hops int, p pathNode) error {
	if d.depth+1+hops > d.maxDepth {
		en := pathNode{parent: &p, idx: 0}
		return d.fail(errBudget(classBudgetDepth, d.r.Pos(), en.String(), nil, d.maxDepth, errDetail(fmt.Sprintf("value nesting exceeds depth budget MaxDepth=%d", d.maxDepth))))
	}
	et := dst.Type().Elem()
	fast := true
	switch {
	case ed.Kind == wire.KindInt && isSignedKind(et) && et.Size() == 8:
		s := unsafe.Slice((*int64)(elemBase(dst)), int(E))
		for i := range E {
			if err := d.elemBudgetHops(i, hops, p); err != nil {
				return err
			}
			n, err := d.r.ReadInt()
			if err != nil {
				return d.mapErr(err)
			}
			s[i] = n
		}
	case ed.Kind == wire.KindUint && isUnsignedKind(et) && et.Size() == 8:
		s := unsafe.Slice((*uint64)(elemBase(dst)), int(E))
		for i := range E {
			if err := d.elemBudgetHops(i, hops, p); err != nil {
				return err
			}
			u, err := d.r.ReadUint()
			if err != nil {
				return d.mapErr(err)
			}
			s[i] = u
		}
	case ed.Kind == wire.KindFloat && ed.Width == 8 && et == float64Type:
		s := unsafe.Slice((*float64)(elemBase(dst)), int(E))
		for i := range E {
			if err := d.elemBudgetHops(i, hops, p); err != nil {
				return err
			}
			f, err := d.r.ReadFloat64()
			if err != nil {
				return d.mapErr(err)
			}
			s[i] = f
		}
	case ed.Kind == wire.KindFloat && ed.Width == 4 && et == float32Type:
		s := unsafe.Slice((*float32)(elemBase(dst)), int(E))
		for i := range E {
			if err := d.elemBudgetHops(i, hops, p); err != nil {
				return err
			}
			f, err := d.r.ReadFloat32()
			if err != nil {
				return d.mapErr(err)
			}
			s[i] = f
		}
	default:
		fast = false
	}
	if fast {
		return nil
	}
	// Generic widths: per-element decodeBody semantics via a local frame
	// equivalent (fits checks included), writes through Index(i).
	for i := range E {
		en := pathNode{parent: &p, idx: int(i)}
		if err := d.elemBudgetFull(i, hops, en); err != nil {
			return err
		}
		tf := dst.Index(int(i))
		switch ed.Kind {
		case wire.KindInt:
			n, err := d.r.ReadInt()
			if err != nil {
				return d.mapErr(err)
			}
			if !fitsSigned(tf.Type(), n) {
				return d.fail(errFormat(classOverflowValue, d.r.Pos(), en.String(), n, tf.Type(), errDetail(fmt.Sprintf("value overflows %s", tf.Type()))))
			}
			tf.SetInt(n)
		case wire.KindUint:
			u, err := d.r.ReadUint()
			if err != nil {
				return d.mapErr(err)
			}
			if !fitsUnsigned(tf.Type(), u) {
				return d.fail(errFormat(classOverflowValue, d.r.Pos(), en.String(), u, tf.Type(), errDetail(fmt.Sprintf("value overflows %s", tf.Type()))))
			}
			tf.SetUint(u)
		case wire.KindFloat:
			switch ed.Width {
			case 4:
				f, err := d.r.ReadFloat32()
				if err != nil {
					return d.mapErr(err)
				}
				tf.SetFloat(float64(f))
			case 8:
				f, err := d.r.ReadFloat64()
				if err != nil {
					return d.mapErr(err)
				}
				tf.SetFloat(f)
			default:
				return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("float width %d invalid", ed.Width))))
			}
		}
	}
	return nil
}

// elemBudget books one element's node count (plus NAMED wrapper hops)
// and the MaxBytes window re-check, mirroring the per-element decodeBody
// frames on the fast paths where no read can overflow.
// elemBudgetHops charges element plus NAMED-descriptor hops for the
// typed fast path, mirroring elemBudgetFull's accounting.
func (d *codecDecoder) elemBudgetHops(i uint64, hops int, p pathNode) error {
	for h := 0; h <= hops; h++ {
		d.nodes++
		if d.nodes > d.maxNodes {
			en := pathNode{parent: &p, idx: int(i)}
			return d.fail(errBudget(classBudgetNodes, d.r.Pos(), en.String(), nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
		}
	}
	return d.checkBytes(p)
}

// elemBudgetFull is elemBudget for the generic-width loop.
func (d *codecDecoder) elemBudgetFull(i uint64, hops int, en pathNode) error {
	for h := 0; h <= hops; h++ {
		d.nodes++
		if d.nodes > d.maxNodes {
			return d.fail(errBudget(classBudgetNodes, d.r.Pos(), en.String(), nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
		}
	}
	return d.checkBytes(en)
}

// isSignedKind reports the Go signed integer kinds.
func isSignedKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// isUnsignedKind reports the Go unsigned integer kinds.
func isUnsignedKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

var (
	float64Type = reflect.TypeFor[float64]()
	float32Type = reflect.TypeFor[float32]()
)

// keyTypeHasPointer reports whether the key type contains a pointer
// component at any depth (pointer-containing keys
// use intern-once + len-guard, never the byte-set).
func keyTypeHasPointer(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface:
		return true
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if keyTypeHasPointer(t.Field(i).Type) {
				return true
			}
		}
	case reflect.Array:
		return keyTypeHasPointer(t.Elem())
	}
	return false
}

// descHasPointer is the stream-side mirror of keyTypeHasPointer: whether a
// descriptor's type contains a POINTER or INTERFACE component at any depth.
// Recursive descriptors terminate on the visiting set.
func descHasPointer(d *wire.Desc, visiting map[*wire.Desc]bool) bool {
	if visiting == nil {
		visiting = make(map[*wire.Desc]bool)
	}
	if visiting[d] {
		return false
	}
	visiting[d] = true
	switch d.Kind {
	case wire.KindPointer, wire.KindInterface:
		return true
	case wire.KindNamed, wire.KindSlice, wire.KindArray:
		return descHasPointer(d.Refs[0], visiting)
	case wire.KindMap:
		return descHasPointer(d.Refs[0], visiting) || descHasPointer(d.Refs[1], visiting)
	case wire.KindStruct:
		for _, f := range d.Fields {
			if descHasPointer(f.Type, visiting) {
				return true
			}
		}
	}
	return false
}

// keyHasNaNDeep reports a NaN component at any key depth (KO-3 recursive);
// pointer cycles terminate via a visited set.
func keyHasNaNDeep(k reflect.Value) bool {
	// The visited-set materializes only when the key actually reaches a
	// pointer component; pointer-free keys (the common maps) skip the
	// allocation entirely.
	var seen map[uintptr]bool
	var visit func(v reflect.Value) bool
	visit = func(v reflect.Value) bool {
		switch v.Kind() {
		case reflect.Float32, reflect.Float64:
			return math.IsNaN(v.Float())
		case reflect.Complex64, reflect.Complex128:
			c := v.Complex()
			return math.IsNaN(real(c)) || math.IsNaN(imag(c))
		case reflect.Pointer:
			if v.IsNil() {
				return false
			}
			if seen == nil {
				seen = make(map[uintptr]bool)
			}
			if seen[v.Pointer()] {
				return false
			}
			seen[v.Pointer()] = true
			return visit(v.Elem())
		case reflect.Interface:
			if !v.IsNil() {
				return visit(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if visit(v.Field(i)) {
					return true
				}
			}
		case reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if visit(v.Index(i)) {
					return true
				}
			}
		}
		return false
	}
	return visit(k)
}

// keyHashableDeep reports whether a decoded map key is safe to hand to Go
// map operations: crafted streams can carry unhashable dynamic
// values in interface positions that no Go-constructed map could hold
// (insertion itself would panic), so the decoder rejects before SetMapIndex.
func keyHashableDeep(k reflect.Value) bool {
	switch k.Kind() {
	case reflect.Interface:
		return k.IsNil() || keyHashableDeep(k.Elem())
	case reflect.Slice, reflect.Map, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return false
	case reflect.Struct:
		for i := 0; i < k.NumField(); i++ {
			if !keyHashableDeep(k.Field(i)) {
				return false
			}
		}
	case reflect.Array:
		for i := 0; i < k.Len(); i++ {
			if !keyHashableDeep(k.Index(i)) {
				return false
			}
		}
	}
	return true
}

// decodeMap reconstructs a map: an intern-space record — a REF in a map
// position resolves to a map of the same type registered earlier in the
// literal MAP header registers the (already allocated) map before its
// pairs, so value REFs inside the pairs resolve mid-fill (identity,
// cycles). Duplicate rejection
// is split by pointer containment: (a) pointer-free keys — byte-set
// of canonical skeleton bytes plus the len==count guard; (b) pointer-
// containing keys — intern-once (a REF before definition fails in the
// wire layer) plus the len==count guard, since byte-identical literal keys
// are legal when they intern to distinct objects (KO-2a). NaN components and
// the Go-== collapse guard apply to both branches.
func (d *codecDecoder) decodeMap(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if d.r.IsNextNil() {
		if k, err := d.r.ReadNil(); err != nil {
			return d.mapErr(err)
		} else if k != wire.NilMap {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for map", k))))
		}
		target.Set(reflect.Zero(target.Type()))
		return nil
	}
	if desc.Refs[0].Kind == wire.KindCoder {
		return errUnsupported(classUnsupportedKind, p.String(), nil, nil, errDetail("coder-coded map key type"))
	}
	if class, err := d.r.PeekClass(); err != nil {
		return d.mapErr(err)
	} else if class == wire.ClassRef {
		id, err := d.r.ReadRef()
		if err != nil {
			return d.mapErr(err)
		}
		mv, err := d.r.MapAt(id)
		if err != nil {
			return d.mapErr(err)
		}
		if mv.Type() != target.Type() {
			return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("ref %d is not a %s", id, target.Type()))))
		}
		target.Set(mv)
		return nil
	}
	count, id, err := d.r.ReadMapHeader()
	if err != nil {
		return d.mapErr(err)
	}
	if count > d.maxPairs {
		return d.fail(errBudget(classBudgetNodes, d.r.Pos(), p.String(), count, d.maxPairs, errDetail(fmt.Sprintf("map pair count %d exceeds MaxMapPairs=%d", count, d.maxPairs))))
	}
	t := target.Type()
	// Map pairs are derived backing memory: count×(keysize+valuesize)
	// charged against MaxBytes before MakeMap and the per-pair New
	// allocations — the map entry of the chargeAlloc taxonomy.
	if err := d.chargeAlloc(count, uint64(t.Key().Size()+t.Elem().Size()), p); err != nil {
		return err
	}
	m := reflect.MakeMap(t)
	// Record-then-fill: the map registers before its pairs (wire-format
	// so cycle REFs inside the pairs resolve to this mid-fill map.
	d.r.SetMapVal(id, m)
	byBytes := !keyTypeHasPointer(t.Key())
	strKeys := byBytes && t.Key().Kind() == reflect.String
	var seen map[string]struct{}
	if byBytes {
		seen = make(map[string]struct{})
	}
	k := reflect.New(t.Key()).Elem()
	val := reflect.New(t.Elem()).Elem()
	zk, zv := reflect.Zero(t.Key()), reflect.Zero(t.Elem())
	for i := range count {
		// Per-pair reset: stream fields absent on the target keep the
		// target's prior value, so the reused staging slots start
		// from zero exactly like fresh reflect.New temporaries.
		k.Set(zk)
		val.Set(zv)
		pn := pathNode{parent: &p, idx: int(i)}
		if err := d.decodeBody(desc.Refs[0], k, pn); err != nil {
			return err
		}
		if !strKeys {
			if keyHasNaNDeep(k) {
				return d.fail(errFormat(classMalformedOp, d.r.Pos(), pn.String(), nil, nil, errDetail("NaN map key")))
			}
			if !keyHashableDeep(k) {
				return d.fail(errFormat(classMalformedOp, d.r.Pos(), pn.String(), nil, nil, errDetail("unhashable map key")))
			}
		}
		if strKeys {
			// A string key's canonical skeleton is the literal token —
			// byte-equal iff string-equal — so the decoded key itself
			// keys the duplicate set without the skeleton render.
			if _, dup := seen[k.String()]; dup {
				return d.fail(errFormat(classDuplicateKey, d.r.Pos(), p.String()+boundedKey(k.String()), nil, nil, nil))
			}
			seen[k.String()] = struct{}{}
		} else if byBytes {
			canon, err := skeletonKeyBytes(k, pn, d.skelScratch())
			if err != nil {
				return err
			}
			if _, dup := seen[string(canon)]; dup {
				return d.fail(errFormat(classDuplicateKey, d.r.Pos(), p.String()+boundedKey(string(canon)), nil, nil, nil))
			}
			seen[string(canon)] = struct{}{}
		}
		if err := d.decodeBody(desc.Refs[1], val, pn); err != nil {
			return err
		}
		m.SetMapIndex(k, val)
	}
	if uint64(m.Len()) != count {
		return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("map pair count %d collapsed to %d keys", count, m.Len()))))
	}
	target.Set(m)
	return nil
}

// decodeStruct reconstructs field values matched by name: stream
// fields absent on the target are skipped parse-only; target fields absent
// from the stream keep their zero value.
func (d *codecDecoder) decodeStruct(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if err := d.r.ReadStructHeader(); err != nil {
		return d.mapErr(err)
	}
	if fl := d.flatLayoutFor(desc, target.Type()); fl != nil {
		return d.decodeFlatStruct(fl, target, p)
	}
	// Decode-into-temp-then-assign (decode atomicity): fields stage in a
	// codec-owned copy; the single target.Set on success is the commit
	// point — an error anywhere above leaves the target untouched (the
	// same discipline the container paths already follow).
	tmp := reflect.New(target.Type()).Elem()
	for _, f := range desc.Fields {
		fn := pathNode{parent: &p, name: f.Name, idx: -1}
		tf := tmp.FieldByName(f.Name)
		if !tf.IsValid() || !tf.CanSet() {
			if err := d.skipValue(f.Type, fn.String()); err != nil {
				return err
			}
			continue
		}
		if err := d.decodeBody(f.Type, tf, fn); err != nil {
			return err
		}
	}
	target.Set(tmp)
	return nil
}

// flatFieldKind is the staging discipline of one flat-struct field.
type flatFieldKind uint8

const (
	fkBool flatFieldKind = iota
	fkInt
	fkUint
	fkF32
	fkF64
	fkString
)

// flatField is one resolved flat-struct field: the target index path and
// the primitive read kind.
type flatField struct {
	idx  []int
	kind flatFieldKind
	ft   reflect.Type
}

// flatLayout is the per-(type, descriptor) flat-struct plan: every
// stream field resolves on the target to a settable primitive field.
// A nil fields slice is the cached negative result: the pair is not
// flat-primitive, and repeat visits skip field resolution entirely.
type flatLayout struct {
	desc   *wire.Desc
	fields []flatField
}

// maxFlatFields bounds the staged (stack) flat path.
const maxFlatFields = 16

// flatLayoutFor resolves the flat-struct plan for (desc, t) or nil when
// the pair is not flat-primitive (composite fields, NAMED wrappers,
// missing/unsettable target fields). Cached per target type; a structurally
// different descriptor rebuilds the entry. Negative results up to
// maxFlatFields are cached like a plan; wide layouts (> maxFlatFields)
// resolve nil on every visit without caching.
func (d *codecDecoder) flatLayoutFor(desc *wire.Desc, t reflect.Type) *flatLayout {
	if d.flat != nil {
		if fl, ok := d.flat[t]; ok {
			if fl.desc == desc {
				if fl.fields == nil {
					return nil
				}
				return fl
			}
			// same Go type, different stream descriptor: rebuild
			delete(d.flat, t)
		}
	} else if len(desc.Fields) > maxFlatFields {
		return nil
	}
	if len(desc.Fields) > maxFlatFields {
		return nil
	}
	fields := make([]flatField, len(desc.Fields))
	for i, f := range desc.Fields {
		if f.Type.Kind == wire.KindNamed {
			return d.cacheFlat(t, desc, nil)
		}
		sf, ok := t.FieldByName(f.Name)
		if !ok || sf.PkgPath != "" {
			return d.cacheFlat(t, desc, nil)
		}
		var kind flatFieldKind
		switch {
		case f.Type.Kind == wire.KindBool && sf.Type.Kind() == reflect.Bool:
			kind = fkBool
		case f.Type.Kind == wire.KindInt && isSignedKind(sf.Type):
			kind = fkInt
		case f.Type.Kind == wire.KindUint && isUnsignedKind(sf.Type):
			kind = fkUint
		case f.Type.Kind == wire.KindFloat && f.Type.Width == 4 && sf.Type.Kind() == reflect.Float32:
			kind = fkF32
		case f.Type.Kind == wire.KindFloat && f.Type.Width == 8 && sf.Type.Kind() == reflect.Float64:
			kind = fkF64
		case f.Type.Kind == wire.KindString && sf.Type.Kind() == reflect.String:
			kind = fkString
		default:
			return d.cacheFlat(t, desc, nil)
		}
		fields[i] = flatField{idx: sf.Index, kind: kind, ft: sf.Type}
	}
	return d.cacheFlat(t, desc, fields)
}

// cacheFlat stores the plan for (t, desc); a nil fields slice is the
// negative marker, surfaced to callers as a nil plan.
func (d *codecDecoder) cacheFlat(t reflect.Type, desc *wire.Desc, fields []flatField) *flatLayout {
	fl := &flatLayout{desc: desc, fields: fields}
	if d.flat == nil {
		d.flat = make(map[reflect.Type]*flatLayout)
	}
	d.flat[t] = fl
	if fields == nil {
		return nil
	}
	return fl
}

// decodeFlatStruct decodes an all-primitive struct into stack staging
// and commits to the target only after every field read succeeds — the
// atomicity guard of the temp path without the reflect.New staging copy.
// Budget semantics mirror the per-field decodeBody frames: one node per
// field, MaxBytes re-check before each read (and after each string).
func (d *codecDecoder) decodeFlatStruct(fl *flatLayout, target reflect.Value, p pathNode) error {
	var stage [maxFlatFields]primSlot
	for j, ff := range fl.fields {
		fn := pathNode{parent: &p, name: fl.desc.Fields[j].Name, idx: -1}
		if d.depth+1 > d.maxDepth {
			return d.fail(errBudget(classBudgetDepth, d.r.Pos(), fn.String(), nil, d.maxDepth, errDetail(fmt.Sprintf("value nesting exceeds depth budget MaxDepth=%d", d.maxDepth))))
		}
		d.nodes++
		if d.nodes > d.maxNodes {
			return d.fail(errBudget(classBudgetNodes, d.r.Pos(), fn.String(), nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
		}
		if err := d.checkBytes(fn); err != nil {
			return err
		}
		switch ff.kind {
		case fkBool:
			b, err := d.r.ReadBool()
			if err != nil {
				return d.mapErr(err)
			}
			stage[j].u = 0
			if b {
				stage[j].u = 1
			}
		case fkInt:
			n, err := d.r.ReadInt()
			if err != nil {
				return d.mapErr(err)
			}
			if !fitsSigned(ff.ft, n) {
				return d.fail(errFormat(classOverflowValue, d.r.Pos(), fn.String(), n, ff.ft, errDetail(fmt.Sprintf("value overflows %s", ff.ft))))
			}
			stage[j].u = uint64(n)
		case fkUint:
			u, err := d.r.ReadUint()
			if err != nil {
				return d.mapErr(err)
			}
			if !fitsUnsigned(ff.ft, u) {
				return d.fail(errFormat(classOverflowValue, d.r.Pos(), fn.String(), u, ff.ft, errDetail(fmt.Sprintf("value overflows %s", ff.ft))))
			}
			stage[j].u = u
		case fkF32:
			f, err := d.r.ReadFloat32()
			if err != nil {
				return d.mapErr(err)
			}
			stage[j].u = math.Float64bits(float64(f))
		case fkF64:
			f, err := d.r.ReadFloat64()
			if err != nil {
				return d.mapErr(err)
			}
			stage[j].u = math.Float64bits(f)
		case fkString:
			s, err := d.r.ReadString()
			if err != nil {
				return d.mapErr(err)
			}
			if err := d.checkBytes(fn); err != nil {
				return err
			}
			stage[j].s = s
		}
	}
	// Fields absent from the stream but present on the target decode to
	// their zero value: reset the target before writing staged
	// fields so stale values from a previous decode do not survive.
	target.Set(reflect.Zero(target.Type()))
	for j, ff := range fl.fields {
		tf := target.FieldByIndex(ff.idx)
		switch ff.kind {
		case fkBool:
			tf.SetBool(stage[j].u != 0)
		case fkInt:
			tf.SetInt(int64(stage[j].u))
		case fkUint:
			tf.SetUint(stage[j].u)
		case fkF32, fkF64:
			tf.SetFloat(math.Float64frombits(stage[j].u))
		case fkString:
			tf.SetString(stage[j].s)
		}
	}
	return nil
}

// checkBytesStr is the string-path form of checkBytes for the skip
// paths (cold).
func (d *codecDecoder) checkBytesStr(path string) error {
	if d.r.Pos()-d.start+d.alloc > d.maxBytes {
		return d.fail(errBudget(classBudgetBytes, d.r.Pos(), path, nil, d.maxBytes, errDetail(fmt.Sprintf("value consumes more than MaxBytes budget %d", d.maxBytes))))
	}
	return nil
}

// indexPathStr builds an element path "<path>[<i>]" plus an optional tail
// for the skip paths (cold): straightforward concatenation.
func indexPathStr(path string, i int, tail string) string {
	return path + "[" + strconv.Itoa(i) + "]" + tail
}

// skipValue advances past one value position without materializing it:
// parse-only, budgets (nodes/bytes/pairs/depth) accrue on
// the skipped tokens, and every internable record the writer allocated an id
// for — string literals, blob/array backings, map objects, descriptors,
// non-zero-size pointer targets — registers its reader-side entry in the same
// order (intern-space mirror. Built-in coder bodies (time.Time)
// are skipped by grammar; custom coder bodies reject with ErrUnsupported
// (their grammar belongs to the coder) and yield ErrUnsupported.
func (d *codecDecoder) skipValue(desc *wire.Desc, path string) error {
	d.depth++
	defer func() { d.depth-- }()
	if d.depth > d.maxDepth {
		return d.fail(errBudget(classBudgetDepth, d.r.Pos(), path, nil, d.maxDepth, errDetail(fmt.Sprintf("value nesting exceeds depth budget MaxDepth=%d", d.maxDepth))))
	}
	d.nodes++
	if d.nodes > d.maxNodes {
		return d.fail(errBudget(classBudgetNodes, d.r.Pos(), path, nil, d.maxNodes, errDetail(fmt.Sprintf("value materializes more nodes than MaxNodes=%d", d.maxNodes))))
	}
	if err := d.checkBytesStr(path); err != nil {
		return err
	}
	switch desc.Kind {
	case wire.KindNamed:
		return d.skipValue(desc.Refs[0], path)
	case wire.KindBool:
		if _, err := d.r.ReadBool(); err != nil {
			return d.mapErr(err)
		}
		return nil
	case wire.KindInt:
		if _, err := d.r.ReadInt(); err != nil {
			return d.mapErr(err)
		}
		return nil
	case wire.KindUint:
		if _, err := d.r.ReadUint(); err != nil {
			return d.mapErr(err)
		}
		return nil
	case wire.KindFloat:
		switch desc.Width {
		case 4:
			if _, err := d.r.ReadFloat32(); err != nil {
				return d.mapErr(err)
			}
		case 8:
			if _, err := d.r.ReadFloat64(); err != nil {
				return d.mapErr(err)
			}
		case 16:
			// Decimal128 skips grammatically: the token and
			// 16 raw bytes consumed, no materialization, no name matching.
			if err := d.r.SkipDecimal128(); err != nil {
				return d.mapErr(err)
			}
		default:
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), "", nil, nil, errDetail(fmt.Sprintf("float width %d invalid", desc.Width))))
		}
		return nil
	case wire.KindComplex:
		switch desc.Width {
		case 4:
			if _, err := d.r.ReadComplex64(); err != nil {
				return d.mapErr(err)
			}
		case 8:
			if _, err := d.r.ReadComplex128(); err != nil {
				return d.mapErr(err)
			}
		default:
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), "", nil, nil, errDetail(fmt.Sprintf("complex width %d invalid", desc.Width))))
		}
		return nil
	case wire.KindString:
		return d.skipStringToken(path)
	case wire.KindBlob:
		// Mirror of decodeBlob: nil selector, bare VIEW (backing already
		// consumed earlier in the stream), or the ARRAY/BLOB record.
		if d.r.IsNextNil() {
			if _, err := d.r.ReadNil(); err != nil {
				return d.mapErr(err)
			}
			return nil
		}
		if class, err := d.r.PeekClass(); err != nil {
			return d.mapErr(err)
		} else if class == wire.ClassView {
			if _, err := d.r.ReadView(); err != nil {
				return d.mapErr(err)
			}
			return nil
		}
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
		// Dense-prefix minimality mirror: the skipped
		// payload is byte-visible, so the check is exact here too.
		if E > 0 && body[E-1] == 0 {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), path, nil, nil, errDetail(fmt.Sprintf("blob dense prefix E=%d ends in a zero byte", E))))
		}
		if _, err := d.r.ReadView(); err != nil {
			return d.mapErr(err)
		}
		return d.checkBytesStr(path)
	case wire.KindSlice:
		// Mirror of decodeSlice: nil selector or bare VIEW before the
		// ARRAY-record path (the encoder legitimately emits
		// both for dropped fields).
		if d.r.IsNextNil() {
			if _, err := d.r.ReadNil(); err != nil {
				return d.mapErr(err)
			}
			return nil
		}
		if class, err := d.r.PeekClass(); err != nil {
			return d.mapErr(err)
		} else if class == wire.ClassView {
			if _, err := d.r.ReadView(); err != nil {
				return d.mapErr(err)
			}
			return nil
		}
		L, E, _, err := d.r.ReadArrayHeader()
		if err != nil {
			return d.mapErr(err)
		}
		if err := d.checkLen(L); err != nil {
			return err
		}
		for i := range E {
			if err := d.skipValue(desc.Refs[0], indexPathStr(path, int(i), "")); err != nil {
				return err
			}
		}
		if _, err := d.r.ReadView(); err != nil {
			return d.mapErr(err)
		}
		return d.checkBytesStr(path)
	case wire.KindArray:
		L, E, _, err := d.r.ReadArrayHeader()
		if err != nil {
			return d.mapErr(err)
		}
		if L != desc.Len {
			return d.fail(errFormat(classTypeMismatch, d.r.Pos(), path, L, desc.Len, errDetail("array length does not match descriptor")))
		}
		for i := range E {
			if err := d.skipValue(desc.Refs[0], indexPathStr(path, int(i), "")); err != nil {
				return err
			}
		}
		return d.checkBytesStr(path)
	case wire.KindMap:
		// Mirror of decodeMap: nil selector before the MAP header.
		if d.r.IsNextNil() {
			if _, err := d.r.ReadNil(); err != nil {
				return d.mapErr(err)
			}
			return nil
		}
		count, _, err := d.r.ReadMapHeader()
		if err != nil {
			return d.mapErr(err)
		}
		if count > d.maxPairs {
			return d.fail(errBudget(classBudgetNodes, d.r.Pos(), path, count, d.maxPairs, errDetail(fmt.Sprintf("map pair count %d exceeds MaxMapPairs=%d", count, d.maxPairs))))
		}
		// Duplicate-slot reject (KO-8): for pointer-free
		// key types, byte-identical key token sequences are value-equal
		// — a duplicate. Pointer-carrying keys are exempt on the skip
		// path: distinct pointers may share byte-identical encodings
		// (KO-2a), and the value-level len-collapse guard exists only
		// for materialized maps.
		var seen map[string]struct{}
		if !descHasPointer(desc.Refs[0], nil) {
			seen = make(map[string]struct{})
		}
		for i := range count {
			start := d.r.Pos()
			if err := d.skipValue(desc.Refs[0], indexPathStr(path, int(i), ".k")); err != nil {
				return err
			}
			if seen != nil {
				key := string(d.r.ByteRange(start, d.r.Pos()))
				if _, dup := seen[key]; dup {
					return d.fail(errFormat(classDuplicateKey, d.r.Pos(), path+boundedKey(key), nil, nil, errDetail("duplicate map key slot (identical key bytes)")))
				}
				seen[key] = struct{}{}
			}
			if err := d.skipValue(desc.Refs[1], indexPathStr(path, int(i), ".v")); err != nil {
				return err
			}
		}
		return d.checkBytesStr(path)
	case wire.KindStruct:
		if err := d.r.ReadStructHeader(); err != nil {
			return d.mapErr(err)
		}
		for _, f := range desc.Fields {
			if err := d.skipValue(f.Type, path+"."+f.Name); err != nil {
				return err
			}
		}
		return d.checkBytesStr(path)
	case wire.KindPointer:
		class, err := d.r.PeekClass()
		if err != nil {
			return d.mapErr(err)
		}
		switch class {
		case wire.ClassNil:
			if _, err := d.r.ReadNil(); err != nil {
				return d.mapErr(err)
			}
			return nil
		case wire.ClassRef:
			if _, err := d.r.ReadRef(); err != nil {
				return d.mapErr(err)
			}
			return nil
		default:
			// placeholder object record: keeps the id space aligned so
			// cycle REFs inside the skipped body resolve;
			// zero-size targets mirror the encoder and reserve nothing
			if !descZeroSize(desc.Refs[0], 0) {
				d.r.RegisterValue(reflect.Value{})
			}
			return d.skipValue(desc.Refs[0], path)
		}
	case wire.KindInterface:
		if d.r.IsNextNil() {
			if _, err := d.r.ReadNil(); err != nil {
				return d.mapErr(err)
			}
			return nil
		}
		cd, err := d.r.ReadDesc()
		if err != nil {
			return d.mapErr(err)
		}
		return d.skipValue(cd, path)
	case wire.KindBigint:
		if err := d.r.SkipBigint(uint64(d.maxBytes)); err != nil {
			if errors.Is(err, wire.ErrBudget) {
				return d.fail(errBudget(classBudgetBytes, d.r.Pos(), path, nil, d.maxBytes, err))
			}
			return d.mapErr(err)
		}
		return d.checkBytesStr(path)
	case wire.KindCoder:
		// The built-in time coder body has a fixed grammar (INT sec,
		// UINT nsec, INT offset, STRING zone) — parse-skip it like any
		// skipped value: no materialization, budgets accrue, the zone
		// string interns per the intern-space mirror. Custom coder bodies
		// belong to the coder's grammar and stay unskippable; the
		// atomicity staging keeps the target untouched on the reject.
		if desc.Name == nameOf(timeType) {
			if _, err := d.r.ReadInt(); err != nil {
				return d.mapErr(err)
			}
			if _, err := d.r.ReadUint(); err != nil {
				return d.mapErr(err)
			}
			if _, err := d.r.ReadInt(); err != nil {
				return d.mapErr(err)
			}
			return d.skipStringToken(path)
		}
		return d.fail(errFormat(classUnknownName, d.r.Pos(), path, desc.Name, nil, errDetail("cannot skip coder-coded field")))
	}
	return d.fail(errFormat(classMalformedOp, d.r.Pos(), path, nil, nil, errDetail(fmt.Sprintf("unknown descriptor kind %d", desc.Kind))))
}

// skipStringToken consumes one string token (REF or literal) parse-only:
// the literal interns like a materialized string — the reader's id space
// mirrors the writer's per token, including skipped ones (wire-format
// The advertised length is budget-checked before the body is
// consumed (crafted-input hygiene).
func (d *codecDecoder) skipStringToken(path string) error {
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	if class == wire.ClassRef {
		if _, err := d.r.ReadRef(); err != nil {
			return d.mapErr(err)
		}
		return nil
	}
	if _, err := d.r.SkipStringLit(uint64(d.maxBytes)); err != nil {
		if errors.Is(err, wire.ErrBudget) {
			return d.fail(errBudget(classBudgetBytes, d.r.Pos(), path, nil, d.maxBytes, err))
		}
		return d.mapErr(err)
	}
	return d.checkBytesStr(path)
}

// decodePointer reconstructs a pointer: nil selector, REF to an already
// reconstructed target, or a fresh allocation registered before its children
// decode (two-phase; cycles close through the registered id).
func (d *codecDecoder) decodePointer(desc *wire.Desc, target reflect.Value, p pathNode) error {
	if desc.Refs[0].Kind == wire.KindInterface {
		return d.decodePtrToIface(desc, target, p)
	}
	if ptrChainToIface(desc, target) {
		return d.decodePtrChainToIface(desc, target, p)
	}
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	switch class {
	case wire.ClassNil:
		if k, err := d.r.ReadNil(); err != nil {
			return d.mapErr(err)
		} else if k != wire.NilPointer {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for pointer", k))))
		}
		target.Set(reflect.Zero(target.Type()))
		return nil
	case wire.ClassRef:
		id, err := d.r.ReadRef()
		if err != nil {
			return d.mapErr(err)
		}
		pv, err := d.r.ValueAt(id)
		if err != nil {
			return d.mapErr(err)
		}
		if !pv.IsValid() || pv.Type() != target.Type() {
			return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("ref %d is not a %s target", id, target.Type()))))
		}
		target.Set(pv)
		return nil
	default:
		pv := reflect.New(target.Type().Elem())
		// zero-size targets are not tracked: the encoder reserves
		// no id for them, so the decoder registers none — mirroring keeps
		// subsequent REF ids aligned
		if target.Type().Elem().Size() != 0 {
			d.r.RegisterValue(pv)
		}
		if err := d.decodeBody(desc.Refs[0], pv.Elem(), p); err != nil {
			return err
		}
		target.Set(pv)
		return nil
	}
}

// decodePtrToIface reconstructs a pointer to an interface pointee. A leading REF
// is the dynamic-type tag (descriptor) or the cycle ref (object); a leading nil
// selector marks the nil pointer or the nil-interface pointee.
func (d *codecDecoder) decodePtrToIface(desc *wire.Desc, target reflect.Value, p pathNode) error {
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	switch class {
	case wire.ClassNil:
		k, err := d.r.ReadNil()
		if err != nil {
			return d.mapErr(err)
		}
		if k == wire.NilPointer {
			target.Set(reflect.Zero(target.Type()))
			return nil
		}
		if k != wire.NilInterface {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for pointer to interface", k))))
		}
		pv := reflect.New(target.Type().Elem())
		if target.Type().Elem().Size() != 0 {
			d.r.RegisterValue(pv)
		}
		target.Set(pv)
		return nil
	case wire.ClassRef:
		id, err := d.r.ReadRef()
		if err != nil {
			return d.mapErr(err)
		}
		if pv, err := d.r.ValueAt(id); err == nil {
			if !pv.IsValid() || pv.Type() != target.Type() {
				return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("ref %d is not a %s target", id, target.Type()))))
			}
			target.Set(pv)
			return nil
		}
		cd, err := d.r.DescAt(id)
		if err != nil {
			return d.mapErr(err)
		}
		pv := reflect.New(target.Type().Elem())
		if target.Type().Elem().Size() != 0 {
			d.r.RegisterValue(pv)
		}
		if err := d.resolveConcrete(cd, pv.Elem(), p); err != nil {
			return err
		}
		target.Set(pv)
		return nil
	default:
		pv := reflect.New(target.Type().Elem())
		if target.Type().Elem().Size() != 0 {
			d.r.RegisterValue(pv)
		}
		if err := d.decodeBody(desc.Refs[0], pv.Elem(), p); err != nil {
			return err
		}
		target.Set(pv)
		return nil
	}
}

// ptrChainToIface reports a pointer chain of at least two levels whose
// descriptor chain and target type chain are equally long and end at an
// interface; any mismatch keeps the generic path.
func ptrChainToIface(desc *wire.Desc, target reflect.Value) bool {
	if desc.Refs[0].Kind != wire.KindPointer {
		return false
	}
	t := target.Type()
	for t.Kind() == reflect.Pointer && desc.Refs[0].Kind == wire.KindPointer {
		t = t.Elem()
		desc = desc.Refs[0]
	}
	return t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Interface &&
		desc.Kind == wire.KindPointer && desc.Refs[0].Kind == wire.KindInterface
}

// decodePtrChainToIface decodes a pointer chain of depth ≥2 with an
// interface leaf: one leading token carries the whole chain's state —
// nil selector, sort-disambiguated REF, or the fresh DESC literal tag.
func (d *codecDecoder) decodePtrChainToIface(desc *wire.Desc, target reflect.Value, p pathNode) error {
	class, err := d.r.PeekClass()
	if err != nil {
		return d.mapErr(err)
	}
	switch class {
	case wire.ClassNil:
		k, err := d.r.ReadNil()
		if err != nil {
			return d.mapErr(err)
		}
		if k == wire.NilPointer {
			target.Set(reflect.Zero(target.Type()))
			return nil
		}
		if k != wire.NilInterface {
			return d.fail(errFormat(classMalformedOp, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("nil selector %d for pointer", k))))
		}
		slot := d.materializePtrChain(target)
		slot.Set(reflect.Zero(slot.Type()))
		return nil
	case wire.ClassRef:
		id, err := d.r.ReadRef()
		if err != nil {
			return d.mapErr(err)
		}
		if pv, err := d.r.ValueAt(id); err == nil {
			if !pv.IsValid() || pv.Type() != target.Type() {
				return d.fail(errFormat(classBadRef, d.r.Pos(), p.String(), nil, nil, errDetail(fmt.Sprintf("ref %d is not a %s target", id, target.Type()))))
			}
			target.Set(pv)
			return nil
		}
		cd, err := d.r.DescAt(id)
		if err != nil {
			return d.mapErr(err)
		}
		slot := d.materializePtrChain(target)
		return d.resolveConcrete(cd, slot, p)
	default:
		slot := d.materializePtrChain(target)
		leaf := desc
		for leaf.Refs[0].Kind == wire.KindPointer {
			leaf = leaf.Refs[0]
		}
		return d.decodeBody(leaf.Refs[0], slot, p)
	}
}

// materializePtrChain allocates and registers the non-nil chain level by
// level, outermost first — the encoder reserves each level's id on entry,
// before descending — and returns the interface slot at the leaf.
func (d *codecDecoder) materializePtrChain(target reflect.Value) reflect.Value {
	slot := target
	for slot.Type().Kind() != reflect.Interface {
		pv := reflect.New(slot.Type().Elem())
		if pv.Type().Elem().Size() != 0 {
			d.r.RegisterValue(pv)
		}
		slot.Set(pv)
		slot = pv.Elem()
	}
	return slot
}
