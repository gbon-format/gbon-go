package gbon

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// renderStringSkel renders the STRING literal token of a map key from
// the wire grammar itself (class 0x6, minimal ARG form, big-endian
// payload, raw bytes) — independent of the wire package, so the probe
// checks the comparator against the format, not against shared code.
func renderStringSkel(s string) []byte {
	n := uint64(len(s))
	out := []byte{0x60}
	var form byte
	switch {
	case n <= 11:
		form = byte(n)
	case n <= 0xFF:
		form = 0x0C
	case n <= 0xFFFF:
		form = 0x0D
	case n <= 0xFFFFFFFF:
		form = 0x0E
	default:
		form = 0x0F
	}
	out[0] |= form
	be := func(v uint64, w int) {
		for i := w - 1; i >= 0; i-- {
			out = append(out, byte(v>>(8*i)))
		}
	}
	switch form {
	case 0x0C:
		be(n, 1)
	case 0x0D:
		be(n, 2)
	case 0x0E:
		be(n, 4)
	case 0x0F:
		be(n, 8)
	}
	return append(out, s...)
}

// TestCmpLenPrefixMatchesRender: the rendered-token order and the
// comparator order are the same total order on every pair — the typed
// map path sorts by cmpLenPrefix instead of materializing skeleton
// bytes, so the two must agree byte for byte.
func TestCmpLenPrefixMatchesRender(t *testing.T) {
	var strs []string
	// boundary lengths around every ARG form edge
	for _, n := range []int{0, 1, 2, 10, 11, 12, 13, 254, 255, 256, 257,
		65534, 65535, 65536, 65537} {
		strs = append(strs, strings.Repeat("a", n), strings.Repeat("b", n), strings.Repeat("\x00", n))
	}
	// shared prefixes across form edges
	strs = append(strs, "ab", strings.Repeat("a", 12)[:11]+"c")
	// random strings with random lengths and alphabets
	rng := rand.New(rand.NewSource(42))
	for range 500 {
		n := rng.Intn(70000)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(1 + rng.Intn(255))
		}
		strs = append(strs, string(b))
	}
	for i := 0; i < len(strs); i++ {
		for j := 0; j < len(strs); j++ {
			a, b := strs[i], strs[j]
			want := strings.Compare(string(renderStringSkel(a)), string(renderStringSkel(b)))
			got := cmpLenPrefix(a, b)
			if (got < 0) != (want < 0) || (got > 0) != (want > 0) {
				t.Fatalf("order mismatch len(%d) vs len(%d): cmp=%d rendered=%d", len(a), len(b), got, want)
			}
		}
	}
}

// stringMapStream runs the typed map[string]string body path on a fresh
// encoder and returns the bare map stream ([map header][pairs]) — no
// stream magic, no type descriptors: the unit's own bytes.
func stringMapStream(t *testing.T, m map[string]string) []byte {
	t.Helper()
	e := newCodecEncoder()
	if err := e.encodeStringMap(reflect.ValueOf(m), pathNode{idx: -1}); err != nil {
		t.Fatalf("typed map stream: %v", err)
	}
	return e.w.Bytes()
}

// genericMapStream runs the generic map body path (skeleton-sorted three
// phase order) on a fresh encoder; nil interface values keep the pair
// slots descriptor-free, so the bare stream parses with the same token
// grammar as the typed path.
func genericMapStream(t *testing.T, m map[string]any) []byte {
	t.Helper()
	e := newCodecEncoder()
	if err := e.encodeMap(reflect.ValueOf(m), pathNode{idx: -1}); err != nil {
		t.Fatalf("generic map stream: %v", err)
	}
	return e.w.Bytes()
}

// argLen decodes an ARG-prefixed length (minimal form,
// big-endian payload) at b[at] and returns the value plus the bytes
// consumed.
func argLen(b []byte, at int) (uint64, int) {
	form := b[at] & 0x0F
	switch form {
	case 0x0C:
		return uint64(b[at+1]), 2
	case 0x0D:
		return uint64(b[at+1])<<8 | uint64(b[at+2]), 3
	case 0x0E:
		return uint64(b[at+1])<<24 | uint64(b[at+2])<<16 | uint64(b[at+3])<<8 | uint64(b[at+4]), 5
	case 0x0F:
		v := uint64(0)
		for i := 1; i <= 8; i++ {
			v = v<<8 | uint64(b[at+i])
		}
		return v, 9
	default:
		return uint64(form), 1
	}
}

// parseMapKeyTokens reads a bare map stream and returns the key tokens
// (first slot of each pair) as the stream's own bytes, in emission
// order. Value slots are STRING, BLOB, VIEW, REF (interned repeat)
// tokens on the typed path, NAMED descriptors on the generic path, and
// nil-interface tokens where the generic fixture uses nil values;
// anything else is a parse failure, never a skip.
func parseMapKeyTokens(t *testing.T, b []byte) [][]byte {
	t.Helper()
	if b[0]>>4 != 0xA {
		t.Fatalf("map stream must start with a map token, got %02X", b[0])
	}
	n, size := argLen(b, 0)
	at := size
	var toks [][]byte
	for i := range n {
		if b[at]>>4 != 0x6 {
			t.Fatalf("pair %d key slot must be a STRING token, got %02X", i, b[at])
		}
		klen, ksize := argLen(b, at)
		end := at + ksize + int(klen)
		if end > len(b) {
			t.Fatalf("pair %d key token overruns stream", i)
		}
		toks = append(toks, b[at:end])
		at = end
		switch {
		case b[at]>>4 == 0x0:
			at++ // NIL selector byte (nil-interface on generic, nil-slice/map on typed)
		case b[at]>>4 == 0x6:
			vlen, vsize := argLen(b, at)
			at += vsize + int(vlen)
		case b[at]>>4 == 0x7:
			vlen, vsize := argLen(b, at)
			at += vsize + int(vlen)
		case b[at]>>4 == 0x9:
			vform := b[at] & 0x0F
			at++
			args := map[byte]int{0: 1, 1: 3, 2: 4}[vform]
			if args == 0 {
				t.Fatalf("unknown view form %d at %02X", vform, b[at-1])
			}
			for range args {
				_, vsize := argLen(b, at)
				at += vsize
			}
		case b[at]>>4 == 0xC:
			_, vsize := argLen(b, at)
			at += vsize
		default:
			t.Fatalf("pair %d value slot must be STRING, REF or nil, got %02X", i, b[at])
		}
	}
	if at != len(b) {
		t.Fatalf("trailing bytes after last pair: %d of %d", at, len(b))
	}
	return toks
}

// modelKeyOrder returns the expected token sequence: the keys sorted by
// the cmpLenPrefix comparator (the reference skeleton order) and rendered
// independently.
func modelKeyOrder(keys []string) [][]byte {
	sorted := append([]string(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return cmpLenPrefix(sorted[i], sorted[j]) < 0 })
	out := make([][]byte, len(sorted))
	for i, k := range sorted {
		out[i] = renderStringSkel(k)
	}
	return out
}

func assertTokenOrder(t *testing.T, name string, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: token count %d, want %d", name, len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("%s: token %d = %x, want %x", name, i, got[i], want[i])
		}
	}
}

// TestStringMapKeyTokenOrder pins the emitted key token bytes of the
// typed map path: equal-length ties, a prefix tie, a raw-byte inversion
// and both ARG-form boundaries (11/12, 255/256). The pinned bytes are
// the stream's own (class 0x6 nibble, minimal ARG form, big-endian
// payload, raw key bytes), not a re-render.
func TestStringMapKeyTokenOrder(t *testing.T) {
	a11 := strings.Repeat("a", 11)
	a12 := strings.Repeat("a", 12)
	a255 := strings.Repeat("a", 255)
	a256 := strings.Repeat("a", 256)
	b12 := strings.Repeat("b", 12)
	cases := []struct {
		name string
		keys []string
		want [][]byte
	}{
		{"equal-length ties", []string{b12, a12}, [][]byte{
			append([]byte{0x6C, 0x0C}, a12...),
			append([]byte{0x6C, 0x0C}, b12...),
		}},
		{"prefix tie", []string{"ba", "b"}, [][]byte{
			{0x61, 'b'},
			{0x62, 'b', 'a'},
		}},
		{"raw-byte inversion", []string{"aa", "b"}, [][]byte{
			{0x61, 'b'},
			{0x62, 'a', 'a'},
		}},
		{"form boundary 11/12", []string{a12, a11}, [][]byte{
			append([]byte{0x6B}, a11...),
			append([]byte{0x6C, 0x0C}, a12...),
		}},
		{"form boundary 255/256", []string{a256, a255}, [][]byte{
			append([]byte{0x6C, 0xFF}, a255...),
			append([]byte{0x6D, 0x01, 0x00}, a256...),
		}},
	}
	for _, tc := range cases {
		m := make(map[string]string, len(tc.keys))
		for _, k := range tc.keys {
			m[k] = "v"
		}
		stream := stringMapStream(t, m)
		got := parseMapKeyTokens(t, stream)
		assertTokenOrder(t, tc.name, got, tc.want)
	}
}

// fixedKeys is the shared controlled key set: equal-length ties, a
// prefix tie, a raw-byte inversion and both ARG-form boundaries.
func fixedKeys() []string {
	return []string{
		"b", "ba", "aa",
		strings.Repeat("a", 11), strings.Repeat("a", 12),
		strings.Repeat("c", 12), strings.Repeat("d", 12),
		strings.Repeat("e", 5), strings.Repeat("f", 5),
		strings.Repeat("a", 255), strings.Repeat("a", 256),
	}
}

// TestStringMapTypedVsGenericKeyOrder runs one key set through both map
// paths — the typed map[string]string fast path and the generic
// skeleton-sorted three-phase path (nil interface values keep the value
// slots descriptor-free) — and requires both key token sequences to
// match the cmpLenPrefix model order.
func TestStringMapTypedVsGenericKeyOrder(t *testing.T) {
	keys := fixedKeys()
	fast := make(map[string]string, len(keys))
	gen := make(map[string]any, len(keys))
	for _, k := range keys {
		fast[k] = "v"
		gen[k] = any(nil)
	}
	want := modelKeyOrder(keys)
	assertTokenOrder(t, "typed", parseMapKeyTokens(t, stringMapStream(t, fast)), want)
	assertTokenOrder(t, "generic", parseMapKeyTokens(t, genericMapStream(t, gen)), want)
}

// TestStringMapMultiSeedStable rebuilds the same map content from 16
// insertion permutations: the stream must be byte-identical across all
// seeds (the pair order comes from the sort alone, never from map
// iteration or insertion), and the key tokens must follow the model
// order.
func TestStringMapMultiSeedStable(t *testing.T) {
	keys := fixedKeys()
	vals := make(map[string]string, len(keys))
	for i, k := range keys {
		vals[k] = "v" + strconv.Itoa(i)
	}
	var first []byte
	for seed := range int64(16) {
		rng := rand.New(rand.NewSource(seed))
		perm := rng.Perm(len(keys))
		m := make(map[string]string, len(keys))
		for _, i := range perm {
			m[keys[i]] = vals[keys[i]]
		}
		b := stringMapStream(t, m)
		if first == nil {
			first = b
			assertTokenOrder(t, "multi-seed", parseMapKeyTokens(t, b), modelKeyOrder(keys))
			continue
		}
		if !bytes.Equal(b, first) {
			t.Fatalf("seed %d: stream diverges from seed 0 (%d vs %d bytes)", seed, len(b), len(first))
		}
	}
}

// TestStringMapSortMatchesModels checks triple equality on random key
// sets: the stream's key token order, the cmpLenPrefix model order and
// the rendered-token bytewise model order must all coincide. Raw bytes
// of the generated keys stay >= 0x80, so a key token needle cannot hide
// inside another key's raw bytes; lengths concentrate on the ARG-form
// boundaries (11/12, 255/256, 65535/65536).
func TestStringMapSortMatchesModels(t *testing.T) {
	rng := rand.New(rand.NewSource(20260901))
	for iter := range 200 {
		n := 5 + rng.Intn(40)
		seen := make(map[string]bool, n)
		keys := make([]string, 0, n)
		for len(keys) < n {
			ln := rng.Intn(14)
			switch r := rng.Intn(40); {
			case r == 0:
				ln = 250 + rng.Intn(12)
			case r < 3:
				ln = rng.Intn(300)
			case r == 20:
				ln = 65530 + rng.Intn(10)
			}
			b := make([]byte, ln)
			for j := range b {
				b[j] = byte(0x80 + rng.Intn(128))
			}
			s := string(b)
			if !seen[s] {
				seen[s] = true
				keys = append(keys, s)
			}
		}
		rendered := append([]string(nil), keys...)
		sort.Slice(rendered, func(i, j int) bool {
			return bytes.Compare(renderStringSkel(rendered[i]), renderStringSkel(rendered[j])) < 0
		})
		ro := make([][]byte, len(rendered))
		for i, k := range rendered {
			ro[i] = renderStringSkel(k)
		}
		mo := modelKeyOrder(keys)
		assertTokenOrder(t, fmt.Sprintf("iter %d models", iter), mo, ro)
		m := make(map[string]string, len(keys))
		for i, k := range keys {
			m[k] = "v" + strconv.Itoa(i)
		}
		b := stringMapStream(t, m)
		assertTokenOrder(t, fmt.Sprintf("iter %d stream", iter), parseMapKeyTokens(t, b), mo)
	}
}

// bytesMapStream runs the typed map[string][]byte body path on a fresh
// encoder and returns the bare map stream — the unit's own bytes, in the
// same grammar as stringMapStream (value slots are BLOB or REF tokens).
func bytesMapStream(t *testing.T, m map[string][]byte) []byte {
	t.Helper()
	e := newCodecEncoder()
	if err := e.encodeBytesMap(reflect.ValueOf(m), pathNode{idx: -1}); err != nil {
		t.Fatalf("typed bytes map stream: %v", err)
	}
	return e.w.Bytes()
}

// TestBytesMapTypedVsGenericKeyOrder is the SC-4 oracle: the typed
// map[string][]byte path and the generic three-phase path (nil interface
// values keep the value slots descriptor-free) must emit the same key
// order, matching the cmpLenPrefix model. The typed order is extracted
// from a full public Marshal by locating each rendered key token — the
// fixed test values never contain key bytes, so token positions are
// unambiguous; full-stream byte equality with an interface-valued
// generic fixture is impossible by design (interface slots carry NAMED
// descriptors).
func TestBytesMapTypedVsGenericKeyOrder(t *testing.T) {
	keys := fixedKeys()
	fast := make(map[string][]byte, len(keys))
	gen := make(map[string]any, len(keys))
	for i, k := range keys {
		fast[k] = []byte{byte(i + 1), byte(i + 2)}
		gen[k] = any(nil)
	}
	blob, err := Marshal(fast)
	if err != nil {
		t.Fatal(err)
	}
	typedOrder := make([][]byte, 0, len(keys))
	type hit struct {
		k   string
		pos int
	}
	hits := make([]hit, 0, len(keys))
	for _, k := range keys {
		tok := renderStringSkel(k)
		pos := bytes.Index(blob, tok)
		if pos < 0 {
			t.Fatalf("key %q token not found in typed stream", k)
		}
		if bytes.Contains(blob[pos+1:], tok) {
			t.Fatalf("key %q token appears twice in typed stream", k)
		}
		hits = append(hits, hit{k: k, pos: pos})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	for _, h := range hits {
		typedOrder = append(typedOrder, renderStringSkel(h.k))
	}
	want := modelKeyOrder(keys)
	assertTokenOrder(t, "typed bytes (positional)", typedOrder, want)
	assertTokenOrder(t, "generic", parseMapKeyTokens(t, genericMapStream(t, gen)), want)
}

// TestBytesMapAliasingRidesBlobPath checks U-I3: an aliased window on a
// shared backing must surface as a VIEW token (0x9 class) after the full
// BLOB emission — the typed path reuses the common blob/group machinery
// instead of a second body copy.
func TestBytesMapAliasingRidesBlobPath(t *testing.T) {
	shared := []byte("shared-backing")
	m := map[string][]byte{
		"k1": shared,
		"k2": shared[:4],
	}
	b := bytesMapStream(t, m)
	views := 0
	for i := 1; i < len(b); i++ {
		if b[i]>>4 == 0x9 {
			views++
		}
	}
	if views == 0 {
		t.Fatalf("no VIEW token in stream with aliased values: % x", b)
	}
	// and the full backing bytes appear exactly once
	if bytes.Count(b, shared) != 1 {
		t.Fatalf("shared backing emitted more than once: % x", b)
	}
}

// TestBytesMapRoundTripAliased pins observable behavior through the
// public API: aliased, nil and empty values survive Marshal/Unmarshal
// with equal content (the typed path never changes decode results).
func TestBytesMapRoundTripAliased(t *testing.T) {
	shared := []byte("shared-backing")
	m := map[string][]byte{
		"k1": shared,
		"k2": shared[:4],
		"k3": nil,
		"k4": {},
	}
	blob, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string][]byte
	if err := Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, out) {
		t.Fatalf("RT diverged: got %v, want %v", out, m)
	}
}

// TestBytesMapMultiSeedStable rebuilds the same map[string][]byte content
// from 16 insertion permutations: the stream must be byte-identical across
// all seeds (order comes from the sort alone).
func TestBytesMapMultiSeedStable(t *testing.T) {
	keys := fixedKeys()
	vals := make(map[string][]byte, len(keys))
	for i, k := range keys {
		vals[k] = []byte("v" + strconv.Itoa(i))
	}
	var first []byte
	for seed := range int64(16) {
		rng := rand.New(rand.NewSource(seed))
		perm := rng.Perm(len(keys))
		m := make(map[string][]byte, len(keys))
		for _, i := range perm {
			m[keys[i]] = vals[keys[i]]
		}
		blob, err := Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = blob
			continue
		}
		if !bytes.Equal(blob, first) {
			t.Fatalf("seed %d: bytes-map stream diverges from seed 0", seed)
		}
	}
}

// TestBytesMapBudgetEdgeMirrorsGeneric checks the budget decisions of the
// typed path against the generic path at the same limit: both must fail
// with ErrBudget on the same MaxBytes window, and both must succeed at a
// roomier one (positions of the checks are never after generic).
func TestBytesMapBudgetEdgeMirrorsGeneric(t *testing.T) {
	fast := map[string][]byte{
		"k1": bytes.Repeat([]byte{1}, 64),
		"k2": bytes.Repeat([]byte{2}, 64),
	}
	gen := make(map[string]any, len(fast))
	for k, v := range fast {
		gen[k] = any(v)
	}
	enc := func(v any, maxBytes int) error {
		e := NewEncoder(&bytes.Buffer{})
		if err := e.SetLimits(Limits{MaxBytes: maxBytes}); err != nil {
			t.Fatalf("SetLimits: %v", err)
		}
		return e.Encode(v)
	}
	typedErr := enc(fast, 96)
	genericErr := enc(gen, 96)
	if typedErr == nil || genericErr == nil || !errors.Is(typedErr, ErrBudget) || !errors.Is(genericErr, ErrBudget) {
		t.Fatalf("MaxBytes=96: typed=%v generic=%v, want both ErrBudget", typedErr, genericErr)
	}
	if err := enc(fast, 4096); err != nil {
		t.Fatalf("MaxBytes=4096 typed: %v, want nil", err)
	}
	if err := enc(gen, 4096); err != nil {
		t.Fatalf("MaxBytes=4096 generic: %v, want nil", err)
	}
}
