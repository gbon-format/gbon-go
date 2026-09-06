package gbon_test

// Structured-log rendering of the error contract: *Error is a
// slog.LogValuer producing a group of exactly class, path, offset —
// class always, path and offset by presence (the same presence rules as
// the one-line text). The shape is machine-pinned through a JSON handler
// over the full crafted corpus.

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

// TestErrorSlogGroup: the error group carries class always, path iff the
// error has one, offset iff the error carries an input offset — nothing
// else — with the key order class, path, offset in the raw JSON.
func TestErrorSlogGroup(t *testing.T) {
	for _, tc := range snippetCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			var ae *gbon.Error
			if !errors.As(tc.err, &ae) {
				t.Fatalf("not As-recoverable: %v", tc.err)
			}
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			log.Error("decode failed", "err", tc.err)
			raw := buf.String()

			var doc struct {
				Level string         `json:"level"`
				Msg   string         `json:"msg"`
				Err   map[string]any `json:"err"`
			}
			if err := json.Unmarshal([]byte(raw), &doc); err != nil {
				t.Fatalf("not valid JSON: %v\n%s", err, raw)
			}
			if len(doc.Err) == 0 {
				t.Fatalf("err attribute missing or not a group: %s", raw)
			}
			want := map[string]any{"class": tc.class}
			if ae.Path != "" {
				want["path"] = ae.Path
			}
			if ae.Offset >= 0 {
				want["offset"] = float64(ae.Offset)
			}
			if len(doc.Err) != len(want) {
				t.Fatalf("group key set %v, want %v (%s)", doc.Err, want, raw)
			}
			for k, v := range want {
				got, ok := doc.Err[k]
				if !ok || got != v {
					t.Fatalf("group key %q = %v, want %v (%s)", k, got, v, raw)
				}
			}
			// fixed attr order in the raw text: class, then path, then offset
			pi := strings.Index(raw, `"path":`)
			oi := strings.Index(raw, `"offset":`)
			if pi >= 0 && oi >= 0 && pi > oi {
				t.Fatalf("path renders after offset: %s", raw)
			}
			if ci := strings.Index(raw, `"class":`); ci < 0 || (pi >= 0 && ci > pi) || (oi >= 0 && ci > oi) {
				t.Fatalf("class is not first: %s", raw)
			}
		})
	}
}
