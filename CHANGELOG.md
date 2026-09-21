# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0]

### Added

- Interior-slot tracking bound: the encode pre-scan registers
  addressable interior slots up to `slotPopMax` (4,194,304) per
  value; beyond the bound per-slot registration stops and excess
  interior pointers encode as value copies (wires stay valid and
  byte-stable). Lazy weak handles for forcing keep factory-scale
  shared-container graphs linear.
- Positional-path references — the format 0.2 amendment
  (wire-format.md 7.2): interior slots — struct fields, nested
  value-struct fields, slice- and array-element interiors,
  blob-element interiors — carry
  identity through a REF path argument (marker/terminator framing
  from the reserved selectors, field steps naming fields through the
  intern space, element steps over backing records); positions
  derivable by the canonical descent keep the bare-REF form
  (mandatory elision). The encoder pre-scan fixes path names and
  record forcings — an untracked container addressed at an interior
  opens as a record immediately before its first dependent carrier
  in the canonical field order — and every stream header carries
  minor 02 (the amendment rides the minor; a 0.1 decoder fails
  loudly at the path marker, `malformed_op`). Decode is
  container-first with the slot-storage invariant: the named
  record's storage materializes before its interior handles bind,
  and each slot's value is read exactly once, at its own token.
- Error classes `bad_path` (data/format) and
  `evolution_ref_unmaterialized` (data/format), additive.
  `bad_path`: unresolvable or non-canonical path arguments —
  zero-step paths, a path spelling the derivable descent, wrong
  rooting (a step traversing a view, interface, or map-cell
  position; element indices outside the backing's declared index
  space), indices beyond the backing, out-of-family grains,
  coder-backed steps, zero-size terminals.
  `evolution_ref_unmaterialized`: the evolution-narrowing carve-out
  — a kept REF or view resolving a record the skipped field left
  unmaterialized rejects with its own class (previously a generic
  `bad_ref`/`type_mismatch` rendering; without a skipped field the
  same shape stays plain corruption, `bad_ref`).
- Conformance leg for the amendment: 16 corpus vectors
  (V-118..V-133; specification corpus 117 → 133) with per-vector
  decode/parity oracles, path-grammar property tests, and fuzz
  seeds over path-carrying streams.

### Changed

- Decoder budget accounting aligned to the per-value scope of
  wire-format.md 9.1 (0.2): MaxBytes charges each value against the
  absolute stream position — the whole-stream start anchor of the
  earlier reading is gone.
- scripts/gate.sh: the specification corpus resolves from
  GBON_SPEC_SRC (with GBON_SPEC_VECTORS passthrough); the default
  ../../spec mount is preserved, so the gate runs unchanged against
  the legacy corpus and against the staged 133-vector corpus.

## [0.1.1] - 2026-09-19

### Fixed

- Decoder budget accounting is absolute: `MaxBytes` now counts
  input and charged allocation bytes against the true stream position
  (intra-record window resets no longer under-count; strings and
  bigints are budget-guarded before the read on both the value and the
  skip path), and error `Offset`s on multi-record streams are the
  documented absolute positions (was: window-relative after the first
  record boundary).
- Registration closure: chained pointer implication (both directions)
  and a canonical-name bridge in the decoder's name resolution — a
  `**T` root decodes through `Register((**T)(nil))` (the typed
  slot-root shape), and `RegisterAs` value-form registrations resolve
  over any-slot payloads (encoder value/decoder value and value/pointer
  combos round-trip; pointer-name/struct-target stays a typed
  `type_mismatch`). Family collisions of chained registrations are
  rejected loudly (`register_conflict`); wire names equal up to
  leading stars are one chain name.
- Skipping a narrowed field no longer mis-parses when the skipped value
  is a shared map record (`skipValue` consumes the leading reference
  and mirrors the decode path; wrong sorts fail typed). The wire-format
  conformance section gains an explicit carve-out: decode-into-narrower
  over aliased map-records and shared backings rejects loudly instead
  of materializing skipped substructure (spec vectors V-115..117).
- Never-panic coverage: an `Encoder` whose `Encode` panicked is
  permanently broken (sticky typed error before any further write; the
  panic itself still propagates unchanged), and the decode tripwire is
  armed before the first stream read (a reader failing inside the very
  first fill is now classified, not escaping).

### Added

- Error-golden corpus (13 rows) pinning the touched error surfaces
  (budget/absolute offsets; `unknown_name` trusted-mode detail with the
  missing name — untrusted rendering unchanged; `register_conflict`;
  parity carve-out rejects; never-panic window classes) against
  class/offset/path drift.
- Stable mode (encoder guard): `MarshalStable` and `Encoder.SetStable`
  reject values outside the stable class — map pairs tied in key skeleton
  and value bytes (pointer tie-break order) and zero float map keys of
  either sign — with the additive error classes `unstable_tie_break` and
  `unstable_zero_float_key` (contract family, `ErrUnsupported`), naming
  the path of the offending map pair. Inside the class the bytes are
  identical to the default mode; with the mode off the encoder is
  byte-identical to the previous release.
- Isomorphic-pair differential tests: independently allocated congruent
  graphs encode byte-identically under map-iteration seeds, including
  map-carried aliased backing arrays (overlapping windows and
  array-pointer views over one backing).

## [0.1.0] - 2026-09-16

The canonical-grain revision (minor 1, draft era): the record's grain is
the coarsest among the tracked grains of its address, computed by an
encoder pre-scan; every record opens at its canonical grain.

### Added

- Encoder pre-scan of pointer-graph grains with layout normalization
  (named conversions of identical underlying layout share one record)
  and the zero-size axiom: non-nil pointers to zero-size pointees
  encode as the marker selector `0x04`, never interned by address.
- The grain tag on differing-grain openings (a REF or descriptor
  literal naming the record's grain before the body), the self-tag on
  pointer-grain records, and elision at grain equality for non-pointer
  grains; repeats are naked REFs to open records (the started-body
  invariant holds by construction with an always-on encoder assert).
- The zero-size marker selector graduates the reserved NIL-class
  selector 4 (decode materializes a fresh zero-size allocation).
- The bounded-exhaustive oracle (`internal/canongrain`): small pointer
  graphs over a fixed grammar checked for decode success, round-trip
  identity (zero-size, slice-view, nil-merge, and degenerate-form
  carve-outs), and byte idempotence.
- 0.0-stream compatibility: a 0.1 decoder reads 0.0 streams (the
  version rides the header); interface slots of 0.0 streams keep the
  container-grain reading through the uniform resolver.

### Changed

- Wire version is 0.1 (header `67 62 6f 6e 00 01`); golden pins
  carry the current encoder's bytes. The transitional corpus-drift
  expectation layer was a CYC-G-window mechanism only: it carried the
  current encoder's bytes for both-vectors whose corpus bytes predated
  the canonical-grain revision, and it was removed in full when the
  spec corpus was rebaselined to 0.1 (the released tree carries no
  such layer).

### Removed

- The decoder's enumeration bridge ladder (`pointerBackref`,
  `interiorOffsetZero`, `decodePointerCell`, `decodeContainerGrainCell`,
  `slotGrainContainer`): reference resolution is a single uniform path
  (record sort, grain tag, offset-zero descent).

## [0.0.5] - 2026-09-13

### Changed

- Decoder conforms to the slot-rooted record reference of the cell
  model: a whole-value REF from a `*T` pointer position naming the slot cell of
  a named record whose leading field carries the interface grain
  decodes through the container grain (the cell materializes as `*T`,
  the slot is the leading field's storage), closing named slot-root
  rings that previously failed `bad_ref`; the encoder and existing
  streams are untouched, and the conformance corpus grows six vectors
  (V-100..V-105) with the reader-side builder binding.

One optimization program, wire format unchanged throughout: encoded
output is byte-identical across every change below, while encoding
allocates less across the corpus, primitive-heavy shapes encode
faster, and long-lived streams hold far less memory.

- Encoder and decoder compile a type plan once per type (and
  coder-registration epoch) and the value pass reuses it: the scan
  walk rides the plan graph, values without sharing sources skip the
  scan machinery entirely, and all-primitive structs decode through a
  one-pass staging tape (the flat-struct field cap is removed).
- Generic-path map encoding sorts key pairs by integer rank cells
  that mirror each key's skeleton bytes (a reflect walk, no
  rendering), replacing the per-pair skeleton marshal on the sort
  path; cells live in a per-encoder reused arena, so the sort
  allocates per key no longer. The encoder-side skeleton scratch is
  gone; the decoder keeps its skeleton walk for duplicate-key
  detection.
- The writer's per-stream interning table (strings, descriptor names,
  array and blob identities) is an open hash table with stored
  hashes: exact membership, growth without re-hashing the strings,
  and a slot pool retained across resets.
- The encoder's identity intern tables no longer pin interned
  objects: entries hold a weak pointer whose liveness is the sole
  identity authority on lookup, a per-Encode stamp avoids re-checking
  an object within one call, and an amortized sweep with constant
  per-insert work evicts dead entries. Long-lived streams stop
  retaining dead objects; as a profile-dependent trade, one-shot
  encoding of sharing-heavy graphs pays a small constant overhead
  (a bimodal encoder variant is a recorded follow-up).
- The scan arena stores grouped members as structure-of-arrays
  columns (GC-visible pinned pointers, pointer columns swept on
  reset, no cross-stream residual references), and primitive emission
  reads field offsets and element strides compiled into the type
  plan; nil slices keep their zero-bit distinction. The scan walk and
  custom coders stay on reflect by design.

### Fixed

- `Encoder.RegisterAs` after the encoder has encoded a value of a
  non-struct type now rejects (the warm-up gate keys on every encoded
  type, not only struct descriptors).

## [0.0.4] - 2026-09-11

### Added

- Error class `internal_panic` with the `ErrInternal` sentinel and the
  `Error.Stack()` accessor: a foreign panic recovered at the decode
  boundary is attributed as an internal defect (panic value in `Got`,
  bounded stack), never as a budget error. Allocation panics of the
  make/grow family under raised limits keep the `budget_alloc` class.

### Fixed

- Stream-mode lookahead over the sliding window: a REF token peeked at
  the `largeRead` compaction boundary could leave a negative cursor
  (`PeekRef` absolute-position restore raced a mid-token window
  compact), panicking on the next direct buffer read. Peek now arms a
  single-token lookahead drained by the next read; the observable
  position never moves. Releases v0.0.2 and v0.0.3 carry the defect.

### Changed

- Decoder reference resolution on the cell model: references are
  type-erased handles resolved through one intern space with the type
  on the cell. Reference graphs round-trip exactly — rings and shared
  edges through pointer, map, slice, and interface positions, at any
  pointer depth and any entry point (root, interface slot, field,
  slice element, map value) — and re-encoding is byte-identical.
- A reference the named cell's sort or type cannot serve rejects with
  a cell-level bad_ref error; nil pointer-chain roots decode as typed
  nils; unnamed interface-pointer chains derive on a registry miss in
  both Decoder and stateless Unmarshal scopes.
- Conformance corpus reader covers 99 vectors (reference chains over
  interface points included).

## [0.0.3] - 2026-09-09

### Fixed

- Decode of pointer chains of any depth reaching an interface pointee:
  a leading REF resolves by intern-record sort (dynamic-type tag vs
  cycle ref) at every chain level, and nil selectors resolve through
  the chain with outer-nil normalization (previously `**any` and
  deeper chains failed round-trip with `bad_ref`/`malformed_op`).

## [0.0.2] - 2026-09-09

### Fixed

- Decode of pointer-to-interface positions: a self-referential interface
  value, a nil-interface pointee, and a typed-nil pointee round-trip
  bit-exact (previously rejected as `bad_ref`/`malformed_op`).
- Encode failures of the underlying writer are now part of the error
  contract: a failed `Write` during flush returns `io_write` (`ErrIO`)
  with the fault as the cause (previously the raw writer error escaped
  the class/sentinel contract).
- A nil or typed-nil reader or writer returns `contract_mismatch`
  instead of panicking; the rejection is not sticky.

## [0.0.1] - 2026-09-06

### Added

- Go reference implementation of the GBON format: object graphs round-trip
  intact — cycles, pointer sharing, slice backing aliasing, map identity.
- Always-canonical encoding: equal values encode to canonical bytes.
- Budgeted decoding: fuzzed, hostile, or corrupted streams yield errors,
  never panics, hangs, or surprise allocations.
- Stdlib-only single module `gbon` (codec, `internal/wire`).

[Unreleased]: https://github.com/gbon-format/gbon-go/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/gbon-format/gbon-go/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/gbon-format/gbon-go/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gbon-format/gbon-go/compare/v0.0.5...v0.1.0
[0.0.5]: https://github.com/gbon-format/gbon-go/compare/v0.0.4...v0.0.5
[0.0.4]: https://github.com/gbon-format/gbon-go/compare/v0.0.3...v0.0.4
[0.0.3]: https://github.com/gbon-format/gbon-go/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/gbon-format/gbon-go/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/gbon-format/gbon-go/releases/tag/v0.0.1
