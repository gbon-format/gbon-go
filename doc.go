// Package gbon serializes and deserializes Go values to a compact binary
// format, preserving value-graph topology.
//
// Supported values: primitives (bit-exact floats, including NaN payloads and
// signed zeros), arbitrary-precision integers (math/big, the built-in
// BIGINT kind), complex numbers, strings, byte slices, slices, arrays, maps
// with structural keys (struct, array, pointer, and interface keys), structs
// (exported fields), pointers (dereference and identity), and interface
// values — concrete dynamic types with typed nils distinct from nil
// interfaces. The per-Decoder registry includes the basic Go types and
// composites ([]any, map[string]any, []string, []int64, map[string]string)
// by default; other concrete types are registered through Decoder.Register
// or bound to a wire name through Decoder.RegisterAs. Unnamed pointer
// chains to an interface point (with one slice or map[string] level over
// the chain) derive from their descriptor name on a registry miss, in
// both Decoder and stateless Unmarshal scopes. Functions, channels,
// and unsafe pointers are rejected with a typed error naming the offending
// field.
//
// Round trips preserve cycles and sharing across every reference kind.
// Identity is cell identity: every reference-worthy record — object,
// map, or descriptor — interns in one per-stream space, references are
// type-erased handles, and the type lives on the cell, so a slot of any
// pointer depth resolves through the same space and the decoded graph
// re-encodes byte-for-byte.
// Pointer identity, map identity (a mutation through one reference is
// visible through every alias), and slice backing-array aliasing: slices
// whose windows observe shared memory decode as windows over one
// reconstructed backing, so len, cap, the append reallocation threshold,
// and mutation visibility across overlapping windows all match the
// original — an append within spare capacity writes the shared backing,
// exactly as in memory. Backing sharing holds across separately encoded
// values of one stream. Encoding terminates on any supported value graph
// within the configured budgets, including cycles through pointer, map,
// slice, and interface positions. One documented drop-out: aliasing
// between a slice and an array value it was sliced from is not preserved
// (arrays travel by value). Root identity follows the calling shape:
// Marshal(&root) preserves the root node's identity (subsequent references to
// the root re-use the same decoded object), Marshal(root) encodes a value
// copy of the root.
//
// Both sides are bounded by budgets. A zero Limits value applies
// conservative decoding defaults (MaxDepth 10^4, MaxNodes 10^6, MaxBytes
// 10^8, MaxMapPairs 10^6, MaxSliceLen 10^8) and looser encoding defaults
// (MaxDepth 10^6, MaxNodes 10^7, MaxBytes 10^9 — the encode side trusts
// its input and caps derived resources: frames, output nodes, output
// bytes). Decoder.SetLimits and Encoder.SetLimits configure them per
// stream. On the decode side MaxBytes is a single counter for input bytes
// plus derived backing allocations, charged as L·elemsize (implicit zero
// tails included) before the allocation happens. Exceeding a budget
// fails with ErrBudget naming it; on the Encoder a negative MaxBytes
// removes the byte cap. The decoders are fuzz-hardened: arbitrary input
// yields a sentinel error, never a panic or a hang. The decoder consumes
// its input incrementally through a sliding window holding one in-flight
// record: large streams decode without materializing the whole input.
//
// Work contract: encoding and decoding a single stream take O(N + S·log S)
// work — N is the total number of encoded value nodes, S the number of
// slots and components in the stream state. Superlinear growth of
// per-stream work with stream length is a defect.
//
// Decoding is atomic with respect to the target: on any error from
// Unmarshal or Decoder.Decode, the value pointed to by v is left exactly
// as it was before the call. Values stage in codec-owned storage and
// publish through a single assignment; a partially decoded value is never
// exposed. After a decode error the Decoder is invalid and never resumes,
// so the failed attempt's interned scratch state stays unobservable.
//
// Interned state lives for the stream on both sides. The decoder retains
// materialized backings, interned strings, and type descriptors for the
// whole stream, each materialization charged against the MaxBytes budget
// of its value at production. The encoder retains grouping slots — live
// references to the producer's backing memory — for the whole stream,
// the price of address stability without reuse hazards.
//
// Encoding is always canonical: a stream's bytes are fixed by its value
// sequence within a memory-encounter history (the canonical-order section
// of the wire specification covers the cross-value nuance). The wire
// format is self-describing and carries a versioned header.
//
// Custom encodings for specific types are provided through the Coder
// interface, registered per Encoder and Decoder; coder-covered types
// (including time.Time and automatic adapter types) work in struct fields,
// slices, maps, and interface positions. A wire name bound through
// Encoder.RegisterAs or Decoder.RegisterAs renames a type's descriptor
// identity without changing the encoding — cross-package evolution pairs
// bind both versions to a common name. Types implementing both
// BinaryMarshaler and BinaryUnmarshaler, or both TextMarshaler and
// TextUnmarshaler, encode through automatic adapters. time.Time carries a
// built-in coder (wall clock and zone; the monotonic reading is dropped).
//
// The package API is stateless: there is no global type registry.
//
// Errors are a contract of structure, not text: diagnostic messages are
// rendering and may change between releases, while the stable surface is the class ID (Error.Class, snake_case),
// the sentinel family (errors.Is against ErrFormat, ErrBudget,
// ErrUnsupported, ErrIO, ErrInternal), and the fields reachable through
// errors.As into *Error (offset, path, got, want, the cause through
// Unwrap, and the recovered-panic stack through Stack). The six
// attribution families map onto the sentinels: data/format to
// ErrFormat, budget to ErrBudget, code and contract to ErrUnsupported,
// env to ErrIO, internal to ErrInternal. Decode errors carry a byte
// offset; errors with a value location carry a path. A nil or typed-nil
// reader or writer is rejected with a contract_mismatch error, not a
// panic, and the rejection is not sticky. Panic containment is a
// decode-side tripwire, forever: an allocation panic of the make/grow
// family under raised limits maps to budget_alloc; any other panic
// decodes to internal_panic with the panic value in Got and a bounded
// stack through Stack. The encode path has no recover, so a value that
// panics during encoding propagates the panic.
//
// The %v form is one line — gbon: <class>, then the path and offset
// segments when the class carries that context — and the %+v form
// appends the structured fields. The redaction boundary:
// the text interpolates no untrusted input — decoded values, keys, and
// stream names surface only through the got/want fields of %+v, never
// in the message; caller arguments (registry names, Go types,
// configured limits) are trusted and stay in the text; path segments
// render boundedly — printable map keys of up to 16 runes render quoted
// (["k"]), longer or non-printable keys render as a shape marker with
// the rune count ([<key 17 B>]) — an address segment never carries a
// full untrusted value. The hex context is gated behind the
// trust flag: decode diagnostics may include a bounded hexdump window of
// the input around the failure offset — never a full untrusted value —
// and only when Decoder.SetTrustedInput was set; the default rendering
// carries no input bytes. The window is also available in machine form
// (Error.Snippet: byte offset, byte length, standard base64), and the
// error renders as a structured slog group (class, path, offset) through
// slog.LogValuer.
//
// Error class IDs (public contract, additive only — new classes may
// appear, IDs are never renamed or reused):
//
// classids-begin
//
//	bad_magic         data/format: wrong stream magic
//	truncated         data/format: input ended mid-value
//	malformed_op      data/format: unknown or misplaced op byte
//	malformed_arg     data/format: malformed op argument
//	overflow_value    data/format: value out of the target range
//	duplicate_key     data/format: duplicate map key
//	bad_ref           data/format: unresolvable or misused reference
//	bad_view          data/format: malformed view record
//	type_mismatch     data/format: stream kind does not fit the target
//	unknown_name      data/format: unregistered wire name
//	budget_depth      budget: depth budget exhausted
//	budget_nodes      budget: node budget exhausted
//	budget_bytes      budget: byte budget exhausted
//	budget_alloc      budget: allocation survived limits but panicked
//	unsupported_kind  code: value kind has no serialized form
//	register_conflict code: registry name or type conflict
//	coder_error       code: custom coder failed
//	coder_recursion   code: custom coder re-entered the codec
//	contract_mismatch contract: decode target breaks the evolution contract
//	io_read           env: underlying reader failed
//	io_write          env: underlying writer failed
//	internal_panic    internal: foreign panic recovered by the tripwire
//
// classids-end
package gbon
