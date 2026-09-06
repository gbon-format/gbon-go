# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.0.1] - 2026-09-06

### Added

- Go reference implementation of the GBON format: object graphs round-trip
  intact — cycles, pointer sharing, slice backing aliasing, map identity.
- Always-canonical encoding: equal values encode to canonical bytes.
- Budgeted decoding: fuzzed, hostile, or corrupted streams yield errors,
  never panics, hangs, or surprise allocations.
- Stdlib-only single module `gbon` (codec, `internal/wire`).

[Unreleased]: https://github.com/gbon-format/gbon-go/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/gbon-format/gbon-go/releases/tag/v0.0.1
