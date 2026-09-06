package gbon_test

import (
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

// refSectionWhitelist pins the exact §-forms that remain legal.
// Empty: no spec documents live in this repository, so no §-form
// has a carrier. Extension requires a justification entry in this
// repository's change history. The forms are written with \u00A7 escapes so this
// file carries no literal §-token for its own gate to reject.
var refSectionWhitelist = []string{}

// refWhitelistTokens is the set of §-form tokens that remain legal,
// extracted from the whitelist entries: a token is whitelisted by exact
// match, never by the line it happens to sit in (a whitelisted marker
// must not legalize any other token sharing its line).
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

// lintSectionRefs rejects §-form references: in Go sources
// always; elsewhere every token must match the whitelist exactly
// (per-token check — a whitelisted marker on the same line does not
// legalize an alien token).
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

// implVocabRe is the closed vocabulary of implementation identifiers
// that have no legal occurrence in spec documents; the pointing role is
// carried by GO-n citations. Extension requires a justification entry
// in this repository's change history (same canon as refSectionWhitelist).
var implVocabRe = regexp.MustCompile(`\b(?:RegisterAs|ErrUnsupported|ErrFormat|ErrBudget)\b`)

// markerSpanRe matches the single legal form of a spec-to-impl file
// reference: an explicit non-normative pointer. Only the parenthetical
// target region is a legalized span — never the rest of the line.
var markerSpanRe = regexp.MustCompile(`\(non-normative: see ([^)]+)\)`)

// isSpecDoc is the directional guard's file set: tracked docs/*.md.
func isSpecDoc(path string) bool {
	return strings.HasPrefix(path, "docs/") && strings.HasSuffix(path, ".md")
}

// lintSpecDirection is the directional guard (spec→impl) over spec
// documents. File-level: a .go reference is legal only when the whole
// token sits inside a non-normative marker's target region (per-token
// check — a marker does not legalize another token sharing its line).
// Vocabulary: the closed identifier set is rejected outright.
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
// directional guard — a green run on the real tree proves nothing unless
// the guard demonstrably rejects known violations of both classes and
// stays silent on the adversarial near-miss forms.
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

// Narrative-taxonomy gate (docsync family): five classes of process
// narrative — stage/origin, considered/rejected, counterfactual,
// future markers, temporal state — have no legal channel in tracked
// text. The dictionary mirrors the specification repository's gate
// branch byte-for-byte (single source); every word is split across
// adjacent literals so this file carries no matchable copy of its own
// patterns (same canon as the section whitelist above). CHANGELOG.md,
// LICENSE, and testdata/ stay outside the contour by design.
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

// TestNarrativeLintNegativeFixtures: negative verification of the
// narrative gate — one red fixture per taxonomy class (the words are
// assembled at run time, so this file stays outside its own scan), the
// exemption greens (canonical positioning text, the draft-status clause,
// the package header, a composite identifier), and the contour bounds
// (CHANGELOG/LICENSE/testdata outside the scan).
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
