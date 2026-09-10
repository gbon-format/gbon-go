# gbon

The **GBON format** serializes Go values **as graphs**: a real object
graph (cycles, pointer sharing, slice backing aliasing) round-trips
intact, equal values encode to **canonical bytes**, and every decode is
**bounded by explicit budgets**: a fuzzed, hostile, or corrupted stream
yields an error, never a panic, a hang, or a surprise allocation.

In short: *gob for graphs* — Go-native and self-describing, but honest
about topology, always-canonical, and hardened on untrusted input.

This is the reference implementation of the GBON specification.

> gbon-go is the reference implementation of GBON for Go: the full Go
> value model — aliasing, cycles, NaN payloads, typed nils — round-trips
> bit-exact, single-pass streamed, under hard decode budgets.

Measurable evidence — measurements, cross-codec behavioral matrices,
and the conformance run — lives in the
[showcase repository](https://github.com/gbon-format/showcase).

## Why

No Go encoder preserves value-graph topology. `encoding/json` does not
support cycles (golang/go#40756; json/v2 keeps them out of contract,
#80114) and fatals with a stack overflow on cyclic data. `encoding/gob`
duplicates shared pointers, and its recursion bugs are documented
(#1518, #47542). CBOR's reference tags (RFC 8949, section 3.4.3) are defined
but rarely implemented, and msgpack has no in-band references at all. So
a round trip silently changes your program's semantics: two pointers to
one object come back as two objects, a mutation through one alias is no
longer visible through the other, and overlapping slice windows over
one backing array become independent memories.

gbon encodes the graph as it is — and reconstructs it on the other
side. It is the Go sibling of Python's pickle, Java serialization, or
the WHATWG Structured Clone, without their known flaws: decode budgets
and a canonical-bytes contract are core format properties, not add-ons.

## Install

```sh
go get github.com/gbon-format/gbon-go
```

Requires Go 1.27+; no codegen, no build tags, no dependencies
outside the standard library.

## Quick start

```go
data, err := gbon.Marshal(value)
if err != nil {
	log.Fatal(err)
}
var copy Value
err = gbon.Unmarshal(data, &copy)
```

Values with interface slots need a streaming decoder with the concrete
types registered (see [Interface values](#interface-values)):

```go
enc := gbon.NewEncoder(w)
dec := gbon.NewDecoder(r)
dec.Register(Rect{}, string(""), int64(0)) // resolve interface values by dynamic type
```

Runnable examples with verified output live in
[example_test.go](example_test.go) and render in the package
documentation.

## Topology: the graph round-trips

One shared object stays one object. Cycles come back as cycles.
Mutations through one reference are visible through every alias —
exactly as in memory.

```go
type Node struct {
	Name     string
	Parent   *Node
	Children []*Node
}

root := &Node{Name: "root"}
laptops := &Node{Name: "laptops", Parent: root}
root.Children = []*Node{laptops}
laptops.Children = []*Node{root} // cycle

data, _ := gbon.Marshal(root)
var cp *Node
_ = gbon.Unmarshal(data, &cp)

// cp.Children[0].Children[0] == cp            — the cycle is back
// cp.Children[0].Parent == cp                 — sharing is back
// mutate cp.Children[0].Parent.Name and both references observe it
```

Marshal graph roots **by pointer** (`Marshal(root)`, not
`Marshal(*root)`): `Marshal(&root)` preserves root-node identity (subsequent
references to the root re-use the same decoded object), while
`Marshal(root)` encodes a value copy of the root — a value has no
address to share. Decoding is symmetric: `Unmarshal(&out)` (or the
streaming `Decoder.Decode`) accepts a pointer-rooted stream into the
plain `T` target and materializes it in place, so the decoded root cell
itself is the identity anchor every backreference resolves to; a
value-form root round-trips structure only — its backreferences
materialize as value-equal duplicates, a documented degradation.

What is preserved, across every reference kind (pointers, maps, slices,
interface values):

- **Pointer identity** — N references to one object decode to N
  references to one object.
- **Map identity** — two references to one map stay one map.
- **Slice backing-array aliasing** — overlapping windows decode as
  windows over one reconstructed backing: `len`, `cap`, the
  append-reallocation threshold, and mutation visibility across
  overlapping windows all match the original. An `append` within spare
  capacity writes the shared backing, as in memory.
- **Cycles** through pointer, map, slice, and interface positions;
  encoding always terminates within the configured budgets.

Identity is cell identity: every reference-worthy record — an object,
a map, a descriptor — interns once in a single per-stream space, and a
reference is a type-erased handle into that space; the type lives on
the cell, not in the reference. Slots of any pointer depth resolve
through the one space, so rings close from any entry point — root,
interface slot, field, element, or map value — and re-encoding
reproduces the stream byte for byte.

Shared structure is stored once — sharing is compression, and identity
still holds after the trip. Also carried bit-exact: floats including
NaN payloads and signed zeros, `complex64`/`complex128`, typed nils
inside interfaces (distinct from nil interfaces), and maps with
structural keys — struct, array, pointer (by identity), and interface
keys (by dynamic type and value).

## Budgeted decoding

Untrusted input is a first-class concern. Five per-decoder budgets
bound a decode before any runaway resource is committed:

```go
dec := gbon.NewDecoder(r)
dec.SetLimits(gbon.Limits{
	MaxDepth:    32,
	MaxNodes:    10_000,
	// MaxBytes covers input + derived backings, charged
	// before the allocation happens:
	MaxBytes:    1 << 20,
	MaxMapPairs: 1_000,
	MaxSliceLen: 10_000,
})
```

A zero `Limits` value applies conservative decoding defaults
(10^4/10^6/10^8/10^6/10^8). The `MaxBytes` counter covers input bytes
plus derived backing arrays, charged as L·elemsize — implicit zero
tails included — *before* the allocation, closing the amplification
vector where a tiny crafted input forces a multi-gigabyte allocation.
`Encoder.SetLimits` mirrors the decoder with looser defaults (the
encode side trusts its input); a negative `MaxBytes` on the Encoder
removes the byte cap — an explicit producer-owned trust decision.
Exceeding any budget fails with `ErrBudget` naming it. Because encode
defaults are looser than decode defaults, values encoded at defaults
may need `SetLimits` on the decoding side.

The decoder is fuzz-hardened: arbitrary input yields a sentinel error,
never a panic or a hang. CI fuzzes three targets per PR — `FuzzDecode`,
`FuzzRT` (value round trips through registered decoders, with a
committed crasher-seed corpus), and `FuzzTrustedWindow`.

## Custom encodings: Coder

Type-specific optimal encodings through one interface, registered per
Encoder/Decoder — no global state:

```go
type u128 struct{ Hi, Lo uint64 }

type u128Coder struct{}

func (u128Coder) EncodeValue(e *gbon.Encoder, v reflect.Value) error {
	u := v.Interface().(u128)
	return e.Encode([2]uint64{u.Hi, u.Lo})
}

func (u128Coder) DecodeValue(d *gbon.Decoder, v reflect.Value) error {
	var raw [2]uint64
	if err := d.Decode(&raw); err != nil {
		return err
	}
	v.Set(reflect.ValueOf(u128{Hi: raw[0], Lo: raw[1]}))
	return nil
}

enc.RegisterCoder(u128{}, u128Coder{})
dec.RegisterCoder(u128{}, u128Coder{})
```

Sub-serialization through the handed `Encoder`/`Decoder` shares the
stream's intern space, so coder bodies participate in topology tracking
and in the stream's budgets on both sides.

Types implementing `BinaryMarshaler`/`BinaryUnmarshaler` or
`TextMarshaler`/`TextUnmarshaler` encode through automatic adapters.
`time.Time` carries a built-in coder (wall clock and zone; the
monotonic reading is dropped); its behavior across type evolution is
covered in [Known limitations](#known-limitations).

## Interface values

Concrete types behind `any` decode through a per-Decoder registry:

```go
dec := gbon.NewDecoder(r)
dec.Register(map[string]any{}, []any{}, string(""), int64(0))
```

The registry is strict: binding a name to a different type is an
error, never a silent overwrite. This is the deliberate price of having
no global registry — decode of interface values is an explicit,
inspectable decision. Unnamed pointer chains to an interface point
(`*interface {}`, `**interface {}`, …), with one slice or `map[string]`
level over the chain, are the one family that derives on a registry
miss: the descriptor name alone fixes the type, so plain `Unmarshal`
decodes them without any registration. A nil pointer-chain root follows
the same rule: it decodes as the typed nil of the chain rather than an
error. Everything else — named types
above all — still reports `ErrFormat` for interface slots; payloads with
`any` slots beyond that family need the streaming path or a
registration.

## Format properties

Design facts of the GBON wire format, independent of any measurement:

- **Graph round-trip** — cycles, pointer sharing, slice backing
  aliasing, and map identity survive encode/decode.
- **Canonical encoding** — a stream's bytes are fixed by its value
  sequence within a memory-encounter history; equal values encode to
  equal bytes, and non-canonical byte classes are rejected on decode.
- **Self-describing, versioned stream** — a versioned header and
  in-band type descriptors; unknown majors are rejected and minor
  bumps are additive.
- **Bounded structure** — five per-decoder budgets charged before
  allocation; crafted input yields an error, never a panic or a hang.
- **Single-pass, streaming decode** — the decoder consumes its input
  incrementally through a sliding window holding one in-flight record;
  large streams decode without materializing the whole input.
- **Zero-copy-friendly layout** — dense prefixes and interned
  references keep decode free of per-element dispatch on primitive
  batches.
- **Streaming Encoder/Decoder** — value-at-a-time framing over any
  `io.Reader`/`io.Writer`, with one intern space per stream.
- **Coder extension** — per-type custom encodings participate in
  topology tracking and stream budgets on both sides.

## Comparison

Qualitative property matrix against the standard-library encoders and
the common binary codecs:

|  | gbon | encoding/gob | encoding/json | fxamacker/cbor | vmihailenco/msgpack | capnproto |
|---|---|---|---|---|---|---|
| Binary | yes | yes | no (text) | yes | yes | yes |
| Self-describing | yes (versioned header + type descriptors) | yes | yes | yes | no (primitive types only) | yes (schema) |
| Cycles | **yes** | no (crash) | no (out of contract, #40756/#80114) | no | no | no (tree) |
| Pointer sharing / identity | **yes** | no (duplicates) | no (duplicates) | no (duplicates) | no (duplicates) | no (owned pointers) |
| Slice backing aliasing (len/cap/append threshold) | **yes** | no | no | no | no | no (lists carry no capacity) |
| Map identity | **yes** | no | no | no | no | no (no map type) |
| Always-canonical bytes | **yes** | no | map keys only | optional (profile-defined) | no | no (canonical form specified) |
| Rejects non-canonical input | **yes** | n/a | no | no | n/a | no |
| Decode budgets | **5 + pre-allocation accounting** | none | depth cap only | levels/elements/pairs | none | yes (traversal limit) |
| Fuzz-hardened decoder | **yes** | no (not for hostile input, per docs) | yes (stdlib) | yes (+ audit) | n/a (dormant) | n/a |
| Go value fidelity (NaN payloads, typed nils, complex, struct/array/interface map keys) | **full** | partial | partial | partial | partial | partial |

Encoding is canonical in both directions: equal values encode to equal
bytes, and non-canonical bytes (non-minimal forms, arguments, dense
prefixes; a SLICE-of-uint8 where a BLOB is canonical) are rejected on
decode. One specification caveat: pointer-carrying map keys order by
allocation sequence, so byte equality for isomorphic
but address-distinct graphs is not specified across processes; within
one encoding run it is deterministic.

`encoding/gob`'s own documentation states it is "not designed to be
hardened against adversarial inputs"; `vmihailenco/msgpack` is
unmaintained — relevant when choosing an infrastructure dependency.

## Error model

Errors follow a structural contract. Every decode or encode failure is
recoverable through `errors.As` into `*gbon.Error`: a stable
snake_case class ID (`Error.Class`), the input `Offset`, the value
`Path`, got/want detail, and the cause through `Unwrap`. Four
sentinels answer `errors.Is` — `ErrFormat` (malformed data),
`ErrBudget` (a named exhausted budget), `ErrUnsupported` (a category
with no serialized form, a coder fault, a registry conflict), and
`ErrIO`: a failed read or write on the underlying stream, distinct
from malformed data. The message text is one line carrying class and
location, interpolates no untrusted input values, and is not
contractual — class IDs are (additive, never renamed). Trusted-input
diagnostics (`Decoder.SetTrustedInput`) can additionally carry a
bounded hexdump window of the input around the failure offset, plus a
machine triple through `Error.Snippet()`. A nil or typed-nil reader or
writer is rejected with a `contract_mismatch` error, not a panic, and
the rejection is not sticky. Panic containment (`budget_alloc`) is a
decode-side guard; the encode side has no recover — a value that
panics during encoding propagates the panic.

## Wire format and versioning

The normative wire specification lives in the companion specification
repository, published separately from this implementation. It covers
the stream header, value grammar, intern space and references,
canonical encoding, decoder hygiene, versioning, and the decoder
evolution contract. The format is at 0.0 (major 0, minor 0 — the
draft era): a decoder rejects unknown majors outright; minor bumps are
additive within a major — new opcode classes, escape subclasses, and
descriptor kinds appear only through a minor bump, and an older decoder
either knows the extension or fails on the specific token, never
silently skips. The specification's consolidated external anchor map —
every cited source with its support and verification status — lives in
the specification repository at docs/references.md.

Design basis: prefix-free framing (Kraft/McMillan), varint arguments
(Elias/protobuf), graph traversal with backreferences (Schorr-Waite;
WHATWG Structured Clone), canonical-form discipline (RFC 8949 profiles),
additive evolution (Avro/protobuf).

## Known limitations

Honest list. Bugs and scope limits, not marketing:

1. **`time.Time` and other coder-backed types work in struct fields.**
   The built-in `time.Time` coder and the automatic Binary/Text adapters
   encode and decode through struct fields, slices, maps, and behind
   pointers. Dropped time.Time fields in cross-version evolution skip by
   grammar; dropped custom-coder fields reject with ErrUnsupported
   (target untouched).
2. **Cross-package type evolution** works through `RegisterAs`: bind
   both versions to a common wire name on the Encoder and Decoder.
   Without a binding, strict type-name matching applies (same package
   path across builds).
3. **Interface slots with basic Go types** (integers, floats, bool,
   string, []byte) and basic composites ([]any, map[string]any,
   []string, []int64, map[string]string) decode through the stateless
   `Unmarshal` without registration. Other concrete types need the
   streaming Decoder with `Register`.
4. **Encode is reflection-only, no codegen/SIMD.** Flat-struct
   throughput trails the leader codecs. Slot grouping is
   sort-indexed (O(s·log s)); primitive batches decode without
   per-element reflect dispatch. Work contract: encoding and decoding
   a single stream take O(N + S·log S) work — N is the total number of
   encoded value nodes, S the number of slots and components in the
   stream state. Superlinear growth of per-stream work with stream
   length is a defect.
5. **Streaming decode is record-scoped.** The Decoder reads
   record-by-record and holds the sliding window of one in-flight
   record (working memory proportional to the record, not the whole
   input), but interned state (descriptors, shared backings) is
   retained for the stream's lifetime by the identity contract — an
   unbounded stream of aliased records retains the backings subsequent views
   may reference.
6. **One projection (Go).** The core specification is
   language-independent and reserves a portable profile for other
   languages; the only projection is the Go binding. Type
   descriptors, typed nils, and registries bind to Go's type system in
   this projection.
7. **Aliasing between a slice and the array value it was sliced from is
   not preserved** — arrays travel by value.
8. **Map-key restrictions.** A pointer to a map (or to a struct
   containing maps) is rejected in key positions; values encoded
   through a `Coder` — including the automatic adapters — are
   unsupported in key positions; map keys with unhashable dynamic
   values are rejected by the encoder.
9. **Functions, channels, and unsafe pointers** are rejected with a
   typed error naming the offending field.
10. **Encode/decode default-budget scissors.** Encoding defaults exceed
    decoding defaults; values encoded at defaults may need `SetLimits`
    on the decoding side.

Concurrency: `Marshal`/`Unmarshal` are safe for concurrent use; a
single `Encoder` or `Decoder` is owned by one goroutine.

## Performance

Throughput and wire-size measurements, cross-codec comparison dumps,
and behavioral matrices live in the separate
[showcase repository](https://github.com/gbon-format/showcase),
published alongside the specification; no performance claims are made
in this README.

## Status

Pre-1.0, draft for early adopters: format 0.0 (major 0, minor 0 — the
draft era). The API may change. The wire format is versioned and
specified, but the library has one maintainer, no ecosystem tooling
yet, and no stability commitments — status is not a promise. Evaluate
it as an early adopter.

## Development

All local tooling runs in Docker:

```sh
scripts/gate.sh
```

Runs: `go build`, `go vet`, a `gofmt` gate, `go test -race` with
coverage threshold, the streaming heap property (≤2× wire, without
`-race`/coverage where it self-skips), import-isolation check for
tests, `golangci-lint`, a deadcode gate, and a bench smoke — inside
the pinned container, under an explicit memory cap.

CI (per PR) mirrors the gate and adds a 20s-per-target fuzz smoke
(`FuzzDecode`, `FuzzRT` — with the committed crasher-seed corpus — and
`FuzzTrustedWindow`).

## License

MIT — see [LICENSE](LICENSE).
