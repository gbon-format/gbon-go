package gbon_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// Reference-lint gate (docsync family): no section-ID token (WF-n /
// GO-n / SA-n) may occur in any tracked file — the spec documents live
// outside this repository, so their markers have no legal channel in
// its content; §-form section references are rejected in Go
// sources and everywhere else (empty whitelist).
//
// Directional guard: spec documents (docs/*.md) carry the normative
// layer alone — a reference to an implementation source file is legal
// only inside an explicit non-normative pointer,
// "(non-normative: see <target>)", and the closed set of implementation
// identifiers (RegisterAs, ErrUnsupported, ErrFormat, ErrBudget) has no
// legal channel in a spec document. The guard stays armed for the day
// spec documents re-enter the tree.

var refIDRe = regexp.MustCompile(`\b(?:WF|GO|SA)-[0-9]+\b`)

// refTokenRe matches a full §-form section token, subsection included.
var refTokenRe = regexp.MustCompile(`§[0-9]+(?:\.[0-9]+)?`)

// refHeadingRe matches document headings that define a section ID.
var refHeadingRe = regexp.MustCompile(`(?m)^## .* \[(WF|GO|SA)-([0-9]+)\]\s*$`)

// refSectionWhitelist pins the exact §-forms that remain legal; empty (no spec
// documents live here, and \u00A7 escapes keep this file outside its own gate).
// Extension requires a justification entry in this repository's change history.
var refSectionWhitelist = []string{}

// refWhitelistTokens is the set of §-form tokens legal by exact match, never by
// the line they sit in (a whitelisted marker must not legalize any other token
// sharing its line).
var refWhitelistTokens = func() map[string]bool {
	m := make(map[string]bool)
	for _, w := range refSectionWhitelist {
		for _, tok := range refTokenRe.FindAllString(w, -1) {
			m[tok] = true
		}
	}
	return m
}()

// buildRefDict computes the ID dictionary from document headings.
// A duplicate definition in one document and reuse across documents are
// errors.
func buildRefDict(docs map[string]string) (map[string]string, error) {
	dict := make(map[string]string)
	for path, text := range docs {
		for _, m := range refHeadingRe.FindAllStringSubmatch(text, -1) {
			id := m[1] + "-" + m[2]
			if prev, dup := dict[id]; dup {
				kind := "reused across documents"
				if prev == path {
					kind = "duplicated within one document"
				}
				return nil, fmt.Errorf("section ID %s %s: %s and %s", id, kind, prev, path)
			}
			dict[id] = path
		}
	}
	return dict, nil
}

// lintRefTokens rejects any ID token in text that is not in the dict.
func lintRefTokens(path, text string, dict map[string]string) error {
	for _, tok := range refIDRe.FindAllString(text, -1) {
		if _, ok := dict[tok]; !ok {
			return fmt.Errorf("%s: unresolved section reference %s", path, tok)
		}
	}
	return nil
}

// lintSectionRefs rejects §-form references: always in Go sources; elsewhere
// every token must match the whitelist exactly (per-token check — a whitelisted
// marker on the same line does not legalize an alien token).
func lintSectionRefs(path, text string) error {
	if !strings.Contains(text, "§") {
		return nil
	}
	isGo := strings.HasSuffix(path, ".go")
	for i, ln := range strings.Split(text, "\n") {
		toks := refTokenRe.FindAllString(ln, -1)
		if len(toks) == 0 {
			continue
		}
		if isGo {
			return fmt.Errorf("%s:%d: §-form section reference in Go source: %q", path, i+1, strings.TrimSpace(ln))
		}
		for _, tok := range toks {
			if !refWhitelistTokens[tok] {
				return fmt.Errorf("%s:%d: §-form section reference outside whitelist: %s in %q", path, i+1, tok, strings.TrimSpace(ln))
			}
		}
	}
	return nil
}

// implFileRe matches a reference to an implementation source file: a
// path-like token ending in ".go" at a word boundary ("go.md",
// "`go test`", "golang.org" carry no such token).
var implFileRe = regexp.MustCompile(`[A-Za-z0-9_./-]+\.go\b`)

// implVocabRe is the closed vocabulary of implementation identifiers with no
// legal occurrence in spec documents; the pointing role is carried by GO-n
// citations. Extension requires a justification entry (same canon as above).
var implVocabRe = regexp.MustCompile(`\b(?:RegisterAs|ErrUnsupported|ErrFormat|ErrBudget)\b`)

// markerSpanRe matches the single legal form of a spec-to-impl file
// reference: an explicit non-normative pointer. Only the parenthetical
// target region is a legalized span — never the rest of the line.
var markerSpanRe = regexp.MustCompile(`\(non-normative: see ([^)]+)\)`)

// isSpecDoc is the directional guard's file set: tracked docs/*.md.
func isSpecDoc(path string) bool {
	return strings.HasPrefix(path, "docs/") && strings.HasSuffix(path, ".md")
}

// lintSpecDirection is the directional guard (spec→impl) over spec documents: a
// .go reference is legal only inside a non-normative marker's target region
// (per-token check); the closed identifier vocabulary is rejected outright.
func lintSpecDirection(path, text string) error {
	for i, ln := range strings.Split(text, "\n") {
		spans := markerSpanRe.FindAllStringSubmatchIndex(ln, -1)
		for _, m := range implFileRe.FindAllStringIndex(ln, -1) {
			legal := false
			for _, s := range spans {
				if m[0] >= s[2] && m[1] <= s[3] {
					legal = true
					break
				}
			}
			if !legal {
				return fmt.Errorf("%s:%d: spec→impl file reference outside non-normative marker: %s", path, i+1, ln[m[0]:m[1]])
			}
		}
		if tok := implVocabRe.FindString(ln); tok != "" {
			return fmt.Errorf("%s:%d: implementation identifier in spec document: %s", path, i+1, tok)
		}
	}
	return nil
}

// trackedFiles enumerates tracked files at check time (git ls-files;
// safe.directory=* keeps the gate runnable from containers where the
// worktree owner differs from the invoking user).
func trackedFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-c", "safe.directory=*", "ls-files").Output()
	if err != nil {
		t.Fatalf("reference-lint: git ls-files: %v", err)
	}
	var files []string
	for ln := range strings.SplitSeq(string(out), "\n") {
		if ln != "" {
			files = append(files, ln)
		}
	}
	return files
}

// refDocs are the documents whose headings define the ID dictionary.
// Empty: the spec documents live in a separate repository; the gate
// therefore rejects any section-ID token outright.
var refDocs = []string{}

func TestReferenceLint(t *testing.T) {
	docs := make(map[string]string, len(refDocs))
	for _, p := range refDocs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reference-lint: read %s: %v", p, err)
		}
		docs[p] = string(b)
	}
	dict, err := buildRefDict(docs)
	if err != nil {
		t.Fatalf("reference-lint: %v", err)
	}
	// No spec documents in this repository: the ID dictionary is empty
	// and every section-ID token in the tree is a violation.
	if len(dict) != 0 {
		t.Fatalf("reference-lint: ID dictionary has %d entries, want 0", len(dict))
	}
	files := trackedFiles(t)
	if len(files) == 0 {
		t.Fatal("reference-lint: no tracked files found (not a git worktree?)")
	}
	for _, p := range files {
		text, ok := docs[p]
		if !ok {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("reference-lint: read %s: %v", p, err)
			}
			if !utf8.Valid(b) {
				continue // binary
			}
			text = string(b)
		}
		if err := lintRefTokens(p, text, dict); err != nil {
			t.Errorf("reference-lint: %v", err)
		}
		if err := lintSectionRefs(p, text); err != nil {
			t.Errorf("reference-lint: %v", err)
		}
		if isSpecDoc(p) {
			if err := lintSpecDirection(p, text); err != nil {
				t.Errorf("reference-lint: %v", err)
			}
		}
		if narrScanned(p) {
			if err := lintNarrative(p, text); err != nil {
				t.Errorf("reference-lint: %v", err)
			}
		}
		if strings.HasSuffix(p, ".go") {
			if err := lintCommentWidth(p, text); err != nil {
				t.Errorf("reference-lint: %v", err)
			}
		}
	}
}

// TestReferenceLintCheckerNegativeFixtures: negative verification of the
// checker itself — a passing run on the real tree proves nothing unless
// the checker demonstrably rejects known violations.
func TestReferenceLintCheckerNegativeFixtures(t *testing.T) {
	// Fixture IDs are assembled at run time ("WF-" + "1") so the gate's
	// own source carries no unresolved token literal.
	dict := map[string]string{
		"WF-" + "1": "docs/wire-format.md",
		"GO-" + "1": "docs/bindings/go.md",
	}
	// Unknown-ID fixtures are assembled at run time ("WF-" + "99") so the
	// gate's own source carries no unresolved token literal.
	unknownWF := "WF-" + "99"
	unknownGO := "GO-" + "99"
	for _, tc := range []struct {
		name string
		text string
		bad  bool
	}{
		{"resolved-id", "see WF-" + "1 and GO-" + "1", false},
		{"unknown-wf-id", "see " + unknownWF, true},
		{"unknown-go-id", "see " + unknownGO, true},
		{"id-in-heading-brackets", "## 4. Title (core) [" + unknownWF + "]", true},
	} {
		err := lintRefTokens("f.md", tc.text, dict)
		if tc.bad && err == nil {
			t.Errorf("reference-lint checker misses known violation: %s", tc.name)
		}
		if !tc.bad && err != nil {
			t.Errorf("reference-lint checker false positive on %s: %v", tc.name, err)
		}
	}
	if _, err := buildRefDict(map[string]string{
		"a.md": "## 1. X (core) [WF-" + "1]\n## 2. Y (core) [WF-" + "1]\n",
	}); err == nil {
		t.Error("reference-lint checker misses duplicate ID within one document")
	}
	if _, err := buildRefDict(map[string]string{
		"a.md": "## 1. X (core) [WF-" + "1]\n",
		"b.md": "## 1. Y (core) [WF-" + "1]\n",
	}); err == nil {
		t.Error("reference-lint checker misses reused ID across documents")
	}
	if _, err := buildRefDict(map[string]string{
		"a.md": "## 1. X (core) [WF-" + "1]\n",
		"b.md": "## 1. Y (core) [GO-" + "1]\n",
	}); err != nil {
		t.Errorf("reference-lint checker false positive on distinct IDs: %v", err)
	}
	if err := lintSectionRefs("x.go", "package a\n\n// see \u00A73 for details\n"); err == nil {
		t.Error("reference-lint checker misses §-form \u00A7 reference in Go source")
	}
	if err := lintSectionRefs("docs/other.md", "see \u00A77\n"); err == nil {
		t.Error("reference-lint checker misses §-form \u00A7 reference outside whitelist")
	}
	// Combined line: every token is checked per line, not per file, so
	// mixed forms cannot pass as a block (per-token check, not line
	// match; whitelist is empty, so any §-form is a violation).
	if err := lintSectionRefs("README.md", "see section 17 aka \u00A717 and RFC 8949 \u00A74.2 caveats\n"); err == nil {
		t.Error("reference-lint checker misses \u00A7 tokens on a mixed line")
	}
	if err := lintSectionRefs("docs/wire-format.md", "(\u00A73.2) and the degradation ladder (\u00A75) plus a stray \u00A713\n"); err == nil {
		t.Error("reference-lint checker misses \u00A7 tokens on a mixed line")
	}
}

// TestSpecDirectionGuardNegativeFixtures: negative verification of the
// directional guard — the guard demonstrably rejects known violations of both
// classes and stays silent on the adversarial near-miss forms.
func TestSpecDirectionGuardNegativeFixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		bad  bool
	}{
		{"bare-file-ref", "see codec_encode.go for details", true},
		{"path-file-ref", "implemented in internal/wire/wire.go", true},
		{"marker-covers-only-its-target", "the checker (non-normative: see referencelint_test.go) plus gbon.go", true},
		{"vocab-registeras", "binds the name through RegisterAs (WF-" + "18)", true},
		{"vocab-errunsupported", "rejected with ErrUnsupported", true},
		{"vocab-errformat", "rejected with ErrFormat", true},
		{"vocab-errbudget", "surfaces as ErrBudget", true},
		{"backtick-go-test", "run (`go test ./...`) to verify", false},
		{"binding-doc-path", "see docs/bindings/go.md", false},
		{"golang-org-url", "see https://golang.org/ref/spec", false},
		{"marker-legal", "the gate semantics (non-normative: see referencelint_test.go) enforce this", false},
		{"core-to-binding-cite", "per GO-" + "2 and WF-" + "12", false},
	} {
		err := lintSpecDirection("docs/fixture.md", tc.text+"\n")
		if tc.bad && err == nil {
			t.Errorf("directional guard misses known violation: %s", tc.name)
		}
		if !tc.bad && err != nil {
			t.Errorf("directional guard false positive on %s: %v", tc.name, err)
		}
	}
}

// Narrative-taxonomy gate (docsync family): five classes of process narrative have no legal
// channel in tracked text; the dictionary mirrors the spec repository's gate branch byte-for-byte,
// split literals keep this file outside its own scan; CHANGELOG.md, LICENSE, and testdata/ are outside.
var narrRe = regexp.MustCompile("(?i)" + narrDict)

// narrDict-begin
var narrDict = "origi" + "nally" + "|" + "init" + "ially" + "|" + "previ" + "ously" + "|" + "form" + "erly" + "|" + "histor" + "ically" + "|" + "a" + "t" + " " + "fi" + "rst" + "|" + "w" + "as" + " " + "fi" + "rst" + " (" + "imple" + "mented" + "|" + "intro" + "duced" + "|" + "ad" + "ded" + "|" + "desi" + "gned" + "|" + "bu" + "ilt" + ")|" + "w" + "as" + " (" + "ad" + "ded" + "|" + "intro" + "duced" + "|" + "cre" + "ated" + ") (" + "i" + "n" + "|" + "la" + "ter" + "|" + "wi" + "th" + ")|" + "ad" + "ded" + " " + "i" + "n" + " (" + "mi" + "nor" + "|" + "ver" + "sion" + "|" + "cy" + "cle" + ")|" + "intro" + "duced" + " " + "i" + "n" + " (" + "mi" + "nor" + "|" + "ver" + "sion" + ")|" + "pr" + "ior" + " " + "t" + "o" + " (" + "ver" + "sion" + "|" + "mi" + "nor" + ")|" + "p" + "re" + "-" + "da" + "tes" + "|" + "da" + "tes" + " " + "ba" + "ck" + "|" + "w" + "as" + " (" + "consi" + "dered" + "|" + "reje" + "cted" + "|" + "dism" + "issed" + "|" + "disc" + "ussed" + "|" + "eval" + "uated" + "|" + "expl" + "ored" + ")|" + "consi" + "dered" + " (" + "a" + "nd" + "|" + "b" + "ut" + ") (" + "reje" + "cted" + "|" + "dism" + "issed" + "|" + "disc" + "arded" + ")|" + "ch" + "ose" + " [a-z ]+ (" + "ov" + "er" + "|" + "ins" + "tead" + ")|" + "w" + "as" + " " + "cho" + "sen" + " " + "ov" + "er" + "|" + "dec" + "ided" + " (" + "t" + "o" + "|" + "aga" + "inst" + "|" + "n" + "ot" + " " + "t" + "o" + ")|a " + "deci" + "sion" + " " + "w" + "as" + " " + "ma" + "de" + "|" + "op" + "ted" + " (" + "f" + "or" + "|" + "aga" + "inst" + ")" + "|" + "co" + "uld" + " (" + "ha" + "ve" + "|" + "wo" + "uld" + "|" + "h" + "ad" + ")|" + "wo" + "uld" + " " + "ha" + "ve" + " " + "be" + "en" + "|" + "mi" + "ght" + " " + "ha" + "ve" + " " + "be" + "en" + "|" + "w" + "as" + " (" + "pla" + "nned" + "|" + "inte" + "nded" + "|" + "supp" + "osed" + ") " + "t" + "o" + "|" + "h" + "ad" + " " + "be" + "en" + " (" + "pla" + "nned" + "|" + "inte" + "nded" + ")|" + "i" + "n" + " " + "a" + "n" + " " + "ear" + "lier" + " (" + "dr" + "aft" + "|" + "des" + "ign" + "|" + "ver" + "sion" + ")" + "|" + "TO" + "DO" + "|" + "FI" + "XME" + "|" + "X" + "XX" + "\\b|" + "HA" + "CK" + "\\b|" + "fut" + "ure" + " (" + "wo" + "rk" + "|" + "ver" + "sion" + "|" + "rel" + "ease" + "|" + "exte" + "nsion" + "|" + "dire" + "ction" + ")|" + "t" + "o" + " " + "b" + "e" + " (" + "ad" + "ded" + "|" + "imple" + "mented" + "|" + "def" + "ined" + "|" + "spec" + "ified" + "|" + "la" + "ter" + ")|" + "roa" + "dmap" + "|" + "curr" + "ently" + "|" + "f" + "or" + " " + "n" + "ow" + "|" + "a" + "s" + " " + "o" + "f" + "|" + "a" + "t" + " " + "pre" + "sent" + "|" + "n" + "ot" + " " + "y" + "et" + "|" + "st" + "ill" + " " + "un" + "der" + "|" + "wo" + "rk" + " " + "i" + "n" + " " + "prog" + "ress" + "|" + "i" + "n" + " " + "prog" + "ress" + "|" + "un" + "der" + " " + "act" + "ive" + " " + "devel" + "opment" + "|" + "rema" + "ining" + " " + "t" + "o" + "|" + "y" + "et" + " " + "t" + "o" + " " + "b" + "e"

// narrDict-end

// narrScanned reports whether path sits inside the narrative-scan
// contour: tracked text outside CHANGELOG.md, LICENSE, and testdata/.
func narrScanned(path string) bool {
	if path == "CHANGELOG.md" || path == "LICENSE" {
		return false
	}
	return !strings.HasPrefix(path, "testdata/")
}

// lintNarrative rejects narrative-taxonomy hits in one file's text,
// line by line (the match report names the whole offending line).
func lintNarrative(path, text string) error {
	for i, ln := range strings.Split(text, "\n") {
		if m := narrRe.FindString(ln); m != "" {
			return fmt.Errorf("%s:%d: narrative-taxonomy hit: %s", path, i+1, strings.TrimSpace(ln))
		}
	}
	return nil
}

// TestNarrativeLintNegativeFixtures: negative verification of the narrative gate —
// one red fixture per taxonomy class (assembled at run time, keeping this file
// outside its own scan), the exemption greens, and the contour bounds.
func TestNarrativeLintNegativeFixtures(t *testing.T) {
	narrProbeNR1 := "pr" + "obe" + " " + "origi" + "nally" + " " + "ad" + "ded"
	narrProbeNR2 := "pr" + "obe" + " " + "w" + "as" + " " + "consi" + "dered" + " " + "a" + "nd" + " " + "reje" + "cted"
	narrProbeNR3 := "pr" + "obe" + " " + "co" + "uld" + " " + "ha" + "ve" + " " + "be" + "en"
	narrProbeNR4 := "pr" + "obe" + " " + "TO" + "DO" + ": " + "ext" + "end"
	narrProbeNR5 := "pr" + "obe" + " " + "curr" + "ently" + " " + "un" + "der"
	g1 := "gbon-go is the reference implementation of GBON for Go: the full Go value model — aliasing, cycles, NaN payloads, typed nils — round-trips bit-exact, single-pass streamed, under hard decode budgets."
	draftClause := "Pre-1.0, draft for early adopters: format 0.0 (major 0, minor 0 — the draft era). The API may change."
	docHeader := "// Package gbon serializes and deserializes Go values to a compact binary"
	for _, tc := range []struct {
		name string
		text string
		bad  bool
	}{
		{"nr1-origin", narrProbeNR1, true},
		{"nr2-rejected", narrProbeNR2, true},
		{"nr3-counterfactual", narrProbeNR3, true},
		{"nr4-marker", narrProbeNR4, true},
		{"nr5-temporal", narrProbeNR5, true},
		{"ex2-g1-verbatim", g1, false},
		{"ex1-draft-clause", draftClause, false},
		{"doc-go-header", docHeader, false},
		{"ex3-composite", "GBON specification probe", false},
	} {
		err := lintNarrative("f.md", tc.text+"\n")
		if tc.bad && err == nil {
			t.Errorf("narrative gate misses known violation: %s", tc.name)
		}
		if !tc.bad && err != nil {
			t.Errorf("narrative gate false positive on %s: %v", tc.name, err)
		}
	}
	// Contour bounds: the exemption surfaces are not scanned.
	for _, p := range []string{"CHANGELOG.md", "LICENSE", "testdata/fuzz/FuzzDecode/x", "testdata/vectors.json"} {
		if narrScanned(p) {
			t.Errorf("narrative contour wrongly includes exempt surface: %s", p)
		}
	}
	for _, p := range []string{"README.md", "codec_encode.go", "codec_rt_test.go", "scripts/gate.sh"} {
		if !narrScanned(p) {
			t.Errorf("narrative contour wrongly excludes tracked surface: %s", p)
		}
	}
	// Self-exclusion: this file carries no matchable copy of the
	// dictionary it assembles.
	b, err := os.ReadFile("referencelint_test.go")
	if err != nil {
		t.Fatalf("narrative gate self-exclusion read: %v", err)
	}
	if m := narrRe.FindString(string(b)); m != "" {
		t.Errorf("narrative dictionary matches its own carrier file: %q", m)
	}
}

// --- text-form guards: doc-band ratchet, comment width, classids sync ---

var (
	classTableIDRe = regexp.MustCompile(`^//\s+(\w+)`)
	classConstIDRe = regexp.MustCompile(`^\tclass\w+\s*=\s*"(\w+)"`)
)

const (
	maxDocBlockLines = 3   // doc-block lines a declaration may carry
	maxCommentRunes  = 120 // runes per full-line comment
)

var topLevelDeclRe = regexp.MustCompile(`^(func|type|const|var)`)

// scanLongDocBlocks returns the start lines of doc blocks longer than
// maxDocBlockLines: a maximal run of comment lines directly before a
// top-level declaration; an empty comment line does not break the run.
func scanLongDocBlocks(text string) []int {
	var sites []int
	lines := strings.Split(text, "\n")
	runStart, runLen := 0, 0
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimLeft(ln, " \t"), "//") {
			if runStart == 0 {
				runStart = i + 1
			}
			runLen++
			continue
		}
		if runStart != 0 && topLevelDeclRe.MatchString(ln) && runLen > maxDocBlockLines {
			sites = append(sites, runStart)
		}
		runStart, runLen = 0, 0
	}
	return sites
}

// baselineDocBlocks pins the per-file count of long doc blocks at guard
// introduction (the sweep legacy of the tree): growth in any file fails,
// shrinkage is silent, and future sweeps revise the map downward.
var baselineDocBlocks = map[string]int{
	"alloc_bound_test.go":              1,
	"codec_bench_test.go":              1,
	"codec_budget_test.go":             9,
	"codec_coder_test.go":              6,
	"codec_containers_test.go":         2,
	"codec_decode.go":                  28,
	"codec_desc.go":                    15,
	"codec_desccache_internal_test.go": 5,
	"codec_emitindex_internal_test.go": 1,
	"codec_encode.go":                  42,
	"codec_firstfit_internal_test.go":  3,
	"codec_fuzz_stream_test.go":        5,
	"codec_fuzz_test.go":               14,
	"codec_golden_test.go":             2,
	"codec_grouping_internal_test.go":  10,
	"codec_interface_test.go":          4,
	"codec_maporder_internal_test.go":  11,
	"codec_mirror_test.go":             2,
	"codec_pool_internal_test.go":      3,
	"codec_prop_test.go":               11,
	"codec_root_test.go":               1,
	"codec_rt_test.go":                 3,
	"codec_scaling_probe_test.go":      1,
	"codec_stream_test.go":             3,
	"codec_work_internal_test.go":      2,
	"crafted_gen_test.go":              5,
	"errors.go":                        9,
	"errors_api_test.go":               1,
	"errors_oracle_test.go":            2,
	"errors_snippet_corpus_test.go":    2,
	"example_test.go":                  1,
	"gbon.go":                          19,
	"gbon_test.go":                     1,
	"internal/wire/arg.go":             1,
	"internal/wire/container.go":       3,
	"internal/wire/desc.go":            6,
	"internal/wire/value.go":           6,
	"internal/wire/wire.go":            11,
	"race_enabled_test.go":             1,
	"register_internal_test.go":        1,
	"reserved.go":                      1,
	"specvectors_test.go":              1,
	"topo_gen_test.go":                 5,
}

// growingFiles returns baseline entries whose current count exceeds the
// pinned value; entries absent from the baseline count as growth.
func growingFiles(baseline, current map[string]int) map[string]int {
	grew := map[string]int{}
	for f, n := range current {
		if n > baseline[f] {
			grew[f] = n - baseline[f]
		}
	}
	return grew
}

// TestDocBlockBandRatchet: the per-file count of over-band doc blocks
// never exceeds the pinned baseline; a failure names the grown files.
func TestDocBlockBandRatchet(t *testing.T) {
	current := map[string]int{}
	sites := map[string][]int{}
	for _, p := range trackedFiles(t) {
		if !strings.HasSuffix(p, ".go") || strings.HasPrefix(p, "testdata/") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("doc-band: read %s: %v", p, err)
		}
		if s := scanLongDocBlocks(string(b)); len(s) > 0 {
			current[p] = len(s)
			sites[p] = s
		}
	}
	for f, delta := range growingFiles(baselineDocBlocks, current) {
		t.Errorf("doc-band: %s grew by %d long doc blocks (now %d, baseline %d) at lines %v",
			f, delta, current[f], baselineDocBlocks[f], sites[f])
	}
}

// TestDocBlockBandNegativeFixtures: the band scanner and the ratchet
// demonstrably reject known growth and stay silent on legal forms.
func TestDocBlockBandNegativeFixtures(t *testing.T) {
	three := "// a\n// b\n// c\n"
	four := three + "// d\n"
	if s := scanLongDocBlocks(three + "func f() {}\n"); len(s) != 0 {
		t.Fatalf("3-line block must pass, got %v", s)
	}
	if s := scanLongDocBlocks(four + "func f() {}\n"); len(s) != 1 || s[0] != 1 {
		t.Fatalf("4-line block must fail at line 1, got %v", s)
	}
	// an empty comment line does not break the run
	if s := scanLongDocBlocks(four + "//\n// e\nfunc f() {}\n"); len(s) != 1 || s[0] != 1 {
		t.Fatalf("empty comment line must not break the run, got %v", s)
	}
	// a blank line breaks the run: two legal blocks
	if s := scanLongDocBlocks(three + "\n" + three + "func f() {}\n"); len(s) != 0 {
		t.Fatalf("blank line must break the run, got %v", s)
	}
	// inline comment after code is not a doc block
	if s := scanLongDocBlocks("x := 1 // trailing\n// a\n// b\n// c\n// d\n_ = x\n"); len(s) != 0 {
		t.Fatalf("trailing comment must not be a doc block, got %v", s)
	}
	// go:build lines are ordinary comment runs
	if s := scanLongDocBlocks("//go:build linux\n\n// a\n// b\n// c\n// d\nconst c = 1\n"); len(s) != 1 || s[0] != 3 {
		t.Fatalf("go:build-following run must be counted at line 3, got %v", s)
	}
	// ratchet: growth fails, equality and shrinkage pass
	base := map[string]int{"a.go": 2, "b.go": 1}
	if g := growingFiles(base, map[string]int{"a.go": 3, "b.go": 1}); len(g) != 1 || g["a.go"] != 1 {
		t.Fatalf("growth must be attributed: %v", g)
	}
	if g := growingFiles(base, map[string]int{"a.go": 2, "b.go": 0}); len(g) != 0 {
		t.Fatalf("equality and shrinkage must pass: %v", g)
	}
	if g := growingFiles(base, map[string]int{"a.go": 1, "b.go": 1, "new.go": 4}); len(g) != 1 || g["new.go"] != 4 {
		t.Fatalf("unbaselined file counts as growth: %v", g)
	}
}

// lintCommentWidth rejects full-line comments wider than maxCommentRunes (runes,
// not bytes); a region from an `// Output:` line to the closing brace of the
// enclosing Example is exempt (go vet fixture format); trailing comments are not checked.
func lintCommentWidth(path, text string) error {
	var b strings.Builder
	inOutput := false
	for i, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(ln, "}") {
			inOutput = false
		}
		trimmed := strings.TrimLeft(ln, " \t")
		if !strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "// Output:") {
			inOutput = true
			continue
		}
		if inOutput {
			continue
		}
		if n := len([]rune(ln)); n > maxCommentRunes {
			fmt.Fprintf(&b, "\n%s:%d: %d runes", path, i+1, n)
		}
	}
	if b.Len() > 0 {
		return errors.New("comment width" + b.String())
	}
	return nil
}

// TestCommentWidthNegativeFixtures: the width checker rejects an over-wide
// full-line comment and stays silent inside Output regions, on trailing
// comments, and at the exact threshold.
func TestCommentWidthNegativeFixtures(t *testing.T) {
	wide := "// " + strings.Repeat("x", 120) + "\n"
	if err := lintCommentWidth("a.go", wide); err == nil {
		t.Fatal("over-wide comment must fail")
	}
	exact := "// " + strings.Repeat("x", 117) + "\n" // 120 runes: the threshold is strictly greater
	if err := lintCommentWidth("a.go", exact); err != nil {
		t.Fatalf("exact threshold must pass: %v", err)
	}
	if err := lintCommentWidth("a.go", "func Example_x() {\n// Output:\n"+wide+"}\n"); err != nil {
		t.Fatalf("Output region must be exempt: %v", err)
	}
	if err := lintCommentWidth("a.go", "x := 1 "+wide); err != nil {
		t.Fatalf("trailing comment is outside the check: %v", err)
	}
}

// classIDTable extracts the class IDs of the doc.go classids table
// (between the classids markers, one ID per comment line).
func classIDTable(text string) map[string]bool {
	ids := map[string]bool{}
	in := false
	for ln := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.Contains(ln, "classids-begin"):
			in = true
		case strings.Contains(ln, "classids-end"):
			in = false
		case in:
			if m := classTableIDRe.FindStringSubmatch(ln); m != nil {
				ids[m[1]] = true
			}
		}
	}
	return ids
}

// constClassIDs extracts the class IDs of the errors.go sentinel table
// (the string literals of the class constants).
func constClassIDs(text string) map[string]bool {
	ids := map[string]bool{}
	for ln := range strings.SplitSeq(text, "\n") {
		if m := classConstIDRe.FindStringSubmatch(ln); m != nil {
			ids[m[1]] = true
		}
	}
	return ids
}

// classIDDrift diffs the two ID sets: the IDs present in only one of
// them (table-only first, code-only second).
func classIDDrift(table, consts map[string]bool) (extraTable, extraConsts []string) {
	for id := range table {
		if !consts[id] {
			extraTable = append(extraTable, id)
		}
	}
	for id := range consts {
		if !table[id] {
			extraConsts = append(extraConsts, id)
		}
	}
	return extraTable, extraConsts
}

// TestClassIDSync: the doc.go classids table and the errors.go class
// constants are one set — any drift fails with the both-side diff.
func TestClassIDSync(t *testing.T) {
	doc, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatalf("classids: read doc.go: %v", err)
	}
	code, err := os.ReadFile("errors.go")
	if err != nil {
		t.Fatalf("classids: read errors.go: %v", err)
	}
	table, consts := classIDTable(string(doc)), constClassIDs(string(code))
	extraTable, extraConsts := classIDDrift(table, consts)
	if len(extraTable) > 0 || len(extraConsts) > 0 {
		t.Fatalf("classids drift: in table only %v; in code only %v", extraTable, extraConsts)
	}
	if len(table) == 0 {
		t.Fatal("classids: both sides empty — the extraction is broken")
	}
}

// TestClassIDSyncNegativeFixtures: the sync check demonstrably rejects
// drift in either direction and passes on equal sets in any order.
func TestClassIDSyncNegativeFixtures(t *testing.T) {
	table := "// classids-begin\n// bad_ref — text\n// bad_view — text\n// classids-end\n"
	code := "const (\n\tclassBadRef = \"bad_ref\"\n\tclassBadView = \"bad_view\"\n)\n"
	if classIDTable(table)["bad_ref"] != true || len(classIDTable(table)) != 2 {
		t.Fatal("table extraction broken")
	}
	if constClassIDs(code)["bad_view"] != true || len(constClassIDs(code)) != 2 {
		t.Fatal("code extraction broken")
	}
	driftedCode := "const (\n\tclassBadRef = \"bad_ref\"\n)\n"
	if et, ec := classIDDrift(classIDTable(table), constClassIDs(driftedCode)); len(et) != 1 || len(ec) != 0 {
		t.Fatalf("drift in code must be detected: %v %v", et, ec)
	}
	driftedTable := "// classids-begin\n// bad_ref — text\n// classids-end\n"
	if et, ec := classIDDrift(classIDTable(driftedTable), constClassIDs(code)); len(et) != 0 || len(ec) != 1 {
		t.Fatalf("drift in table must be detected: %v %v", et, ec)
	}
	// equal sets in any textual order pass (the pin is the set)
	reordered := "// classids-begin\n// bad_view — text\n// bad_ref — text\n// classids-end\n"
	if et, ec := classIDDrift(classIDTable(reordered), constClassIDs(code)); len(et) != 0 || len(ec) != 0 {
		t.Fatalf("equal sets in any order must pass: %v %v", et, ec)
	}
}
