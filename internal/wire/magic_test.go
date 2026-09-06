package wire

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// TestMagicNoCollision: the stream magic 67 62 6F 6E must
// not collide with known file-format signatures — hex comparison against a
// list of offset-0 signatures; any shared prefix is a failure.
//
// Formats with no magic (gob, CBOR, protobuf) are not sniffable at offset 0
// and cannot appear in this list by construction; they are documented here
// as the reason for their absence.

// knownSignatures lists offset-0 file signatures (hex prefixes).
var knownSignatures = []struct {
	name string
	hex  string
}{
	{"PNG", "89 50 4E 47"},
	{"GIF87a", "47 49 46 38 37"},
	{"GIF89a", "47 49 46 38 39"},
	{"ZIP", "50 4B 03 04"},
	{"ZIP-EOCD", "50 4B 05 06"},
	{"ELF", "7F 45 4C 46"},
	{"PDF", "25 50 44 46"},
	{"JPEG", "FF D8 FF"},
	{"RIFF", "52 49 46 46"},
	{"BMP", "42 4D"},
	{"gzip", "1F 8B"},
	{"bzip2", "42 5A 68"},
	{"xz", "FD 37 7A 58"},
	{"7z", "37 7A BC AF"},
	{"RAR", "52 61 72 21"},
	{"SQLite", "53 51 4C 69"},
	{"XML", "3C 3F 78 6D"},
	{"UTF8-BOM", "EF BB BF"},
	{"UTF16LE-BOM", "FF FE"},
	{"UTF16BE-BOM", "FE FF"},
	{"JSON-object", "7B"},
	{"JSON-array", "5B"},
}

// collides reports whether magic shares a prefix with any known signature.
func collides(magic []byte, sigs []struct {
	name string
	hex  string
}) (string, bool) {
	for _, s := range sigs {
		sig := hexToBytes(s.hex)
		n := min(len(magic), len(sig))
		if bytes.Equal(magic[:n], sig[:n]) {
			return s.name, true
		}
	}
	return "", false
}

func TestMagicNoCollision(t *testing.T) {
	magic := []byte(Magic)
	if len(magic) != 4 || !bytes.Equal(magic, []byte{0x67, 0x62, 0x6F, 0x6E}) {
		t.Fatalf("magic constant drift: % x", magic)
	}
	if name, hit := collides(magic, knownSignatures); hit {
		t.Errorf("magic 67 62 6F 6E collides with %s signature", name)
	}
	// Negative verification of the checker itself: a ZIP magic must be
	// detected (a pass alone proves nothing about the check).
	if _, hit := collides([]byte{0x50, 0x4B, 0x03, 0x04}, knownSignatures); !hit {
		t.Error("checker defect: ZIP magic not detected as colliding")
	}
	if _, hit := collides([]byte{0x89, 0x50, 0x4E, 0x47}, knownSignatures); !hit {
		t.Error("checker defect: PNG magic not detected as colliding")
	}
}

// hexToBytes decodes a space-separated hex literal.
func hexToBytes(s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		panic("bad hex literal: " + err.Error())
	}
	return b
}
