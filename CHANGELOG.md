# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/gbon-format/gbon-go/compare/v0.0.2...HEAD
[0.0.2]: https://github.com/gbon-format/gbon-go/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/gbon-format/gbon-go/releases/tag/v0.0.1
