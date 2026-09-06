package gbon_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The complexity work contract must stay in the package doc and the
// README encode section, word for word: doc drift is a regression of
// the declared encoder contract, not just prose.
func TestWorkContractDocSync(t *testing.T) {
	sentences := []string{
		"encoding and decoding a single stream take O(N + S·log S) work",
		"Superlinear growth of per-stream work with stream length is a defect.",
	}
	ws := regexp.MustCompile(`\s+`)
	for _, path := range []string{"doc.go", "README.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		flat := strings.ReplaceAll(string(raw), "//", " ")
		flat = ws.ReplaceAllString(flat, " ")
		for _, s := range sentences {
			if !strings.Contains(flat, s) {
				t.Errorf("%s: work-contract sentence missing: %q", path, s)
			}
		}
	}
}
