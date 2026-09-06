package gbon_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gbon-format/gbon-go"
)

// hexD decodes a hex fixture (local to the property suite).
func hexD(s string) ([]byte, error) { return hex.DecodeString(s) }

// Property laws on a base-category generator: round-trip
// equivalence, determinism, map order stability,
// malformed-input hygiene.

type pt struct {
	A int64
	B string
	C []float64
}

type gen struct {
	r    *rand.Rand
	ptrs []*int // pointer pool for DAG sharing
}

func (g *gen) value(depth int) any {
	n := 16
	if depth > 0 {
		n = 11
	}
	switch g.r.Intn(n) {
	case 0:
		return g.r.Int63() - g.r.Int63()
	case 1:
		return g.r.Uint64()
	case 2:
		return math.Float64frombits(g.r.Uint64())
	case 3:
		return math.Float32frombits(uint32(g.r.Uint32()))
	case 4:
		return complex(math.Float64frombits(g.r.Uint64()), math.Float64frombits(g.r.Uint64()))
	case 5:
		return g.str()
	case 6:
		b := make([]byte, g.r.Intn(8))
		g.r.Read(b)
		return b
	case 7:
		if depth >= 4 {
			return []int{}
		}
		n := g.r.Intn(5)
		s := make([]int, n, n+g.r.Intn(3))
		for i := range s {
			if g.r.Intn(3) == 0 {
				s[i] = 0 // trailing-zero elision fodder
			} else {
				s[i] = int(g.r.Int31() - g.r.Int31())
			}
		}
		return s
	case 8:
		if depth >= 4 {
			return [3]int64{}
		}
		var a [3]int64
		for i := range a {
			a[i] = g.r.Int63()
		}
		return a
	case 9:
		if depth >= 3 {
			return map[string]int{}
		}
		m := map[string]int{}
		for i, n := 0, g.r.Intn(6); i < n; i++ {
			m[g.str()] = int(g.r.Int31())
		}
		return m
	case 10:
		if depth >= 3 {
			return map[float64]int{}
		}
		m := map[float64]int{}
		for i, n := 0, g.r.Intn(5); i < n; i++ {
			m[g.nonNaNFloat()] = int(g.r.Int31())
		}
		return m
	}
	if depth >= 4 {
		return pt{}
	}
	switch g.r.Intn(3) {
	case 0:
		return pt{A: g.r.Int63(), B: g.str(), C: []float64{g.nonNaNFloat(), 0}}
	case 1:
		if len(g.ptrs) > 0 && g.r.Intn(2) == 0 {
			return g.ptrs[g.r.Intn(len(g.ptrs))] // DAG sharing
		}
		p := new(int)
		*p = int(g.r.Int31())
		g.ptrs = append(g.ptrs, p)
		return p
	default:
		if depth >= 5 {
			return nil
		}
		m := map[int]string{}
		for i, n := 0, g.r.Intn(5); i < n; i++ {
			m[int(g.r.Int31())] = g.str()
		}
		return m
	}
}

func (g *gen) str() string {
	n := g.r.Intn(10)
	b := make([]byte, n)
	g.r.Read(b) // arbitrary bytes, including invalid UTF-8
	return string(b)
}

func (g *gen) nonNaNFloat() float64 {
	for {
		f := math.Float64frombits(g.r.Uint64())
		if !math.IsNaN(f) {
			return f
		}
	}
}

func (g *gen) mapValue() any {
	switch g.r.Intn(3) {
	case 0:
		m := map[string]int{}
		for i, n := 0, 1+g.r.Intn(6); i < n; i++ {
			m[g.str()] = int(g.r.Int31())
		}
		return m
	case 1:
		m := map[int]string{}
		for i, n := 0, 1+g.r.Intn(6); i < n; i++ {
			k := int(g.r.Int31()) - i
			if g.r.Intn(2) == 0 {
				k = -k
			}
			m[k] = g.str()
		}
		return m
	default:
		m := map[float64]int{}
		for i, n := 0, 1+g.r.Intn(6); i < n; i++ {
			m[g.nonNaNFloat()] = int(g.r.Int31())
		}
		return m
	}
}

// Generated base-category values round-trip under the
// shared equivalence operator.
func TestPropertyRoundTrip(t *testing.T) {
	g := &gen{r: rand.New(rand.NewSource(42))}
	for range 200 {
		v := g.value(0)
		rv := reflect.ValueOf(v)
		if !rv.IsValid() {
			continue
		}
		data, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal %s: %v", safeDescValue(v), err)
		}
		out := reflect.New(rv.Type())
		if err := gbon.Unmarshal(data, out.Interface()); err != nil {
			t.Fatalf("Unmarshal %s: %v", safeDescValue(v), err)
		}
		if !equiv(rv, out.Elem()) {
			t.Fatalf("property RT mismatch:\n in  %s\n out %s", safeDescValue(v), safeDescValue(out.Elem().Interface()))
		}
	}
}

// Marshal is a pure function of the value.
func TestPropertyDeterminism(t *testing.T) {
	g := &gen{r: rand.New(rand.NewSource(43))}
	for range 200 {
		v := g.value(0)
		b1, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		b2, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("non-deterministic bytes for %s", safeDescValue(v))
		}
	}
}

// The canonical key order makes encoding independent
// of map iteration order — observed as byte equality across fresh instances
// of equal maps (dual-instance check) AND
// as bytewise-ascending key encodings inside the map payload (a
// wrong-but-deterministic comparator must not pass).
func TestPropertyMapOrder(t *testing.T) {
	g := &gen{r: rand.New(rand.NewSource(44))}
	for range 100 {
		v := g.mapValue()
		b1, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		b2, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("map order unstable for %s", safeDescValue(v))
		}
		if err := assertKeysAscending(v, b1); err != nil {
			t.Fatalf("key order not bytewise-ascending: %v", err)
		}
	}
}

// assertKeysAscending derives each key's in-map wire encoding from the
// format norm (wire-format.md: string key = STRING token + raw bytes;
// float64 key = 0x41 + 8 big-endian raw bits) — an independent oracle — and
// checks that the encodings appear in the map payload in ascending bytewise
// order (KO-1).
func assertKeysAscending(m any, payload []byte) error {
	rv := reflect.ValueOf(m)
	if rv.Kind() != reflect.Map {
		return fmt.Errorf("not a map: %T", m)
	}
	type loc struct {
		at  int
		enc []byte
	}
	locs := make([]loc, 0, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		var enc []byte
		switch k := iter.Key().Interface().(type) {
		case string:
			enc = stringKeyWire(k)
		case float64:
			enc = floatKeyWire(k)
		case int:
			enc = intKeyWire(k)
		default:
			return fmt.Errorf("unsupported key type %T for KO-1 check", k)
		}
		at := bytes.Index(payload, enc)
		if at < 0 {
			return fmt.Errorf("key encoding % x not found in map payload", enc)
		}
		locs = append(locs, loc{at: at, enc: enc})
	}
	sort.Slice(locs, func(i, j int) bool { return locs[i].at < locs[j].at })
	for i := 1; i < len(locs); i++ {
		if bytes.Compare(locs[i-1].enc, locs[i].enc) >= 0 {
			return fmt.Errorf("adjacent keys out of KO-1 order at offset %d", locs[i].at)
		}
	}
	return nil
}

// stringKeyWire: STRING token per the norm — inline length 0..11 is
// (0x60|len) followed by raw bytes; longer strings carry a width selector.
func stringKeyWire(s string) []byte {
	n := len(s)
	if n <= 11 {
		return append([]byte{0x60 | byte(n)}, s...)
	}
	return append([]byte{0x6C, byte(n)}, s...) // u8 width covers generator sizes
}

// floatKeyWire: FLOAT64 token per the norm — 0x41 + big-endian raw bits.
func floatKeyWire(f float64) []byte {
	b := math.Float64bits(f)
	return []byte{0x41,
		byte(b >> 56), byte(b >> 48), byte(b >> 40), byte(b >> 32),
		byte(b >> 24), byte(b >> 16), byte(b >> 8), byte(b)}
}

// intKeyWire: INT token per the norm — zigzag value in minimal ARG form
// (inline 0..11; else minimal width selector + big-endian bytes).
func intKeyWire(n int) []byte {
	u := uint64(n<<1) ^ uint64(n>>63) // zigzag
	if u <= 11 {
		return []byte{0x20 | byte(u)}
	}
	switch {
	case u <= 0xFF:
		return []byte{0x2C, byte(u)}
	case u <= 0xFFFF:
		return []byte{0x2D, byte(u >> 8), byte(u)}
	case u <= 0xFFFFFFFF:
		return []byte{0x2E, byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
	default:
		return []byte{0x2F,
			byte(u >> 56), byte(u >> 48), byte(u >> 40), byte(u >> 32),
			byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
	}
}

// P-malformed: truncations and single-bit flips of golden vectors
// decode to success, ErrFormat, or ErrBudget — never a panic. Seeds for the
// Long-running fuzz corpus.
func TestPropertyMalformed(t *testing.T) {
	for _, tc := range golden {
		full, err := gbon.Marshal(tc.input)
		if err != nil {
			t.Fatalf("Marshal %s: %v", tc.name, err)
		}
		target := reflect.New(reflect.TypeOf(tc.input))
		check := func(data []byte) {
			err := func() (err error) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("%s: panic: %s", tc.name, safeDescValue(p))
					}
				}()
				return gbon.Unmarshal(data, target.Interface())
			}()
			if err == nil {
				return
			}
			// P1 sentinel-or-success: exactly the four sentinels of the
			// class-sentinel table (io.EOF of a drained stream is not an error).
			if !errors.Is(err, gbon.ErrFormat) && !errors.Is(err, gbon.ErrBudget) &&
				!errors.Is(err, gbon.ErrUnsupported) && !errors.Is(err, gbon.ErrIO) {
				t.Fatalf("%s: mutant error without sentinel: %v", tc.name, err)
			}
			// P2 class+location recoverability: every error As-extracts to
			// *gbon.Error with a non-empty class and a byte offset.
			var ae *gbon.Error
			if !errors.As(err, &ae) {
				t.Fatalf("%s: error not As-recoverable: %v", tc.name, err)
			}
			if ae.Class() == "" || ae.Offset < 0 {
				t.Fatalf("%s: error lacks class/offset: %v", tc.name, ae)
			}
			// P4 render invariant: %v is one line.
			if strings.Contains(err.Error(), "\n") {
				t.Fatalf("%s: %v not one line", tc.name, err)
			}
		}
		for n := range full {
			check(full[:n])
		}
		for i := range full {
			for bit := range 8 {
				mutant := append([]byte(nil), full...)
				mutant[i] ^= 1 << bit
				check(mutant)
			}
		}
	}
}

// --- property laws: aliasing and struct keys ---

// equivWindows is the shared equivalence operator for
// shared-slice configurations: element-wise equality plus len/cap plus
// pairwise window-observability — windows intersect before RT iff they
// intersect after, and a mutation in an intersection is visible from both
// sides.
func equivWindows(t *testing.T, a, b [4][]int) bool {
	t.Helper()
	for i := range a {
		if a[i] == nil || b[i] == nil {
			if (a[i] == nil) != (b[i] == nil) {
				return false
			}
			continue
		}
		if len(a[i]) != len(b[i]) || cap(a[i]) != cap(b[i]) {
			return false
		}
		for k := range a[i] {
			if a[i][k] != b[i][k] {
				return false
			}
		}
	}
	for i := range 4 {
		for j := i + 1; j < 4; j++ {
			if a[i] == nil || a[j] == nil {
				continue
			}
			// Window intersection in memory coordinates: the observable
			// [ptr, ptr+len) ranges must overlap iff they overlap after RT.
			// Coordinates are element indices (es=8 for []int).
			inter := func(x, y []int) (ix, iy int, n int, ok bool) {
				const es = 8
				px, py := reflect.ValueOf(x).Pointer(), reflect.ValueOf(y).Pointer()
				lo, hi := px, px+uintptr(len(x))*es
				if py > lo {
					lo = py
				}
				if end := py + uintptr(len(y))*es; end < hi {
					hi = end
				}
				if lo >= hi {
					return 0, 0, 0, false
				}
				return int((lo - px) / es), int((lo - py) / es), int((hi - lo) / es), true
			}
			ax, ay, na, oka := inter(a[i], a[j])
			bx, by, nb, okb := inter(b[i], b[j])
			if oka != okb || na != nb || ax != bx || ay != by {
				t.Fatalf("window geometry (%d,%d): in ok=%v n=%d x/y=%d/%d; out ok=%v n=%d x/y=%d/%d",
					i, j, oka, na, ax, ay, okb, nb, bx, by)
			}
			if !oka {
				continue
			}
			k := na - 1 // an index inside the intersection
			b[i][ax+k] = 4242
			if b[j][ay+k] != 4242 {
				return false
			}
			b[j][ay+k] = 1717
			if b[i][ax+k] != 1717 {
				return false
			}
		}
	}
	return true
}

// Generated shared configurations — base array with
// random windows (zero-len, off+len<cap, disjoint cap-truncated pairs,
// trailing zeros, big-cap) round-trip under the equivWindows operator.
func TestPropertySliceAliasing(t *testing.T) {
	g := rand.New(rand.NewSource(45))
	for range 200 {
		lb := g.Intn(7)
		cb := lb + g.Intn(6) // trailing spare capacity = implicit zeros
		base := make([]int, lb, cb)
		for i := range base {
			if g.Intn(3) != 0 {
				base[i] = g.Intn(1000)
			}
		}
		var wins [4][]int
		for i := range wins {
			off := g.Intn(cb + 1)
			rest := cb - off
			ln := g.Intn(rest + 1)
			capw := ln + g.Intn(rest-ln+1)
			if g.Intn(4) == 0 {
				ln = 0 // zero-len windows over live memory
			}
			wins[i] = base[off : off+ln : off+capw]
		}
		data, err := gbon.Marshal(wins)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var out [4][]int
		if err := gbon.Unmarshal(data, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !equivWindows(t, wins, out) {
			t.Fatalf("aliasing property violated:\n in  %v\n out %v", wins, out)
		}
	}
}

// Maps with struct/array/pointer keys (nested
// multi-pointer fields, cyclic keys up to depth 3, cyclic-vs-nil pairs,
// blank fields, long string fields covering both u8 and u16 selectors) —
// determinism under randomized Go-map iteration (KO-2), len
// preservation, and ==-relation preservation.
type kfield struct {
	_ int // blank: excluded from the key skeleton (KO-6)
	A int64
	B string
	P *int64
	Q *int64
	C *kcyc
}

type kcyc struct {
	V    int64
	Next *kcyc
}

func TestPropertyStructKeys(t *testing.T) {
	g := rand.New(rand.NewSource(46))
	str := func(n int) string {
		b := make([]byte, n)
		g.Read(b)
		return string(b)
	}
	for iter := range 150 {
		m := map[kfield]int{}
		for i, n := 0, 1+g.Intn(5); i < n; i++ {
			p := new(int64)
			*p = int64(g.Intn(50)) // equal pointee values, distinct pointers
			q := new(int64)
			*q = int64(g.Intn(50))
			var c *kcyc
			if g.Intn(2) == 0 {
				c = &kcyc{V: int64(g.Intn(3))}
				if g.Intn(2) == 0 {
					c.Next = c // cycle depth ≤ 3
				}
			}
			m[kfield{A: int64(g.Intn(3)), B: str(8 + g.Intn(300)), P: p, Q: q, C: c}] = i
		}
		// Tie-class: byte-equal skeletons AND values over distinct
		// pointer components (multi-pointer fields; cyclic and
		// nil-terminated C collapse to equal skeleton bytes) — only the
		// DFS address sequence may separate the pairs.
		mtie := map[kfield]int{}
		tieA, tieB := int64(g.Intn(3)), str(8+g.Intn(300))
		tieP := new(int64) // shared first pointer: divergence lands on Q/C
		*tieP = 42
		for i, n := 0, 2+g.Intn(3); i < n; i++ {
			q := new(int64)
			*q = 42
			c := &kcyc{V: 7}
			if g.Intn(2) == 0 {
				c.Next = c // cyclic: same skeleton bytes as nil-terminated
			}
			mtie[kfield{A: tieA, B: tieB, P: tieP, Q: q, C: c}] = 0
		}
		tb1, err := gbon.Marshal(mtie)
		if err != nil {
			t.Fatalf("Marshal tie-class: %v", err)
		}
		for range 3 {
			tb2, err := gbon.Marshal(mtie)
			if err != nil {
				t.Fatalf("Marshal tie-class again: %v", err)
			}
			if !bytes.Equal(tb1, tb2) {
				t.Fatalf("tie-class non-deterministic (iter %d)", iter)
			}
		}
		tout := reflect.New(reflect.TypeOf(mtie))
		if err := gbon.Unmarshal(tb1, tout.Interface()); err != nil {
			t.Fatalf("Unmarshal tie-class: %v", err)
		}
		if tout.Elem().Len() != len(mtie) {
			t.Fatalf("tie-class keys collapsed: %d != %d", tout.Elem().Len(), len(mtie))
		}
		cyc := map[*kcyc]int{}
		for i, n := 0, 1+g.Intn(3); i < n; i++ {
			k := &kcyc{V: int64(g.Intn(3))}
			k.Next = &kcyc{V: int64(g.Intn(3)), Next: k} // cycle depth ≤ 3
			cyc[k] = i
		}
		arr := map[[2]int64]int{}
		for i, n := 0, 1+g.Intn(4); i < n; i++ {
			arr[[2]int64{int64(g.Intn(3)), int64(g.Intn(3))}] = i
		}
		for _, mv := range []any{m, cyc, arr} {
			b1, err := gbon.Marshal(mv)
			if err != nil {
				t.Fatalf("Marshal %T: %v", mv, err)
			}
			b2, err := gbon.Marshal(mv)
			if err != nil {
				t.Fatalf("Marshal2 %T: %v", mv, err)
			}
			if !bytes.Equal(b1, b2) {
				t.Fatalf("non-deterministic bytes for %T", mv)
			}
			out := reflect.New(reflect.TypeOf(mv))
			if err := gbon.Unmarshal(b1, out.Interface()); err != nil {
				t.Fatalf("Unmarshal %T: %v", mv, err)
			}
			got := out.Elem()
			if got.Len() != reflect.ValueOf(mv).Len() {
				t.Fatalf("len not preserved for %T: %d != %d", mv, got.Len(), reflect.ValueOf(mv).Len())
			}
			// Key correspondence by pair value (generator values are
			// unique per slot); map iteration order is randomized.
			orig := reflect.ValueOf(mv)
			keyByVal := func(m reflect.Value, want int) reflect.Value {
				iter := m.MapRange()
				for iter.Next() {
					if int(iter.Value().Int()) == want {
						return iter.Key()
					}
				}
				return reflect.Value{}
			}
			n := orig.Len()
			for i := range n {
				for j := range n {
					eqA := keyByVal(orig, i).Equal(keyByVal(orig, j))
					eqB := keyByVal(got, i).Equal(keyByVal(got, j))
					if eqA != eqB {
						t.Fatalf("== relation lost for %T at (%d,%d)", mv, i, j)
					}
				}
			}
		}
	}
}

// Interface keys, with the reject branch:
// generated interface-keyed maps — KO-2 determinism on iface skeletons, RT
// key equivalence via interface ==, byte-stable external REF onto key
// pointers (keyPtrSeq tie-break), and the unhashable-key reject property:
// unhashable dynamic keys yield ErrUnsupported, never a Go hash panic.
func TestPropertyIfaceKeys(t *testing.T) {
	anyKey := func(r *rand.Rand, pool *[]*X) any {
		switch r.Intn(7) {
		case 0:
			return r.Int()
		case 1:
			return int32(r.Int())
		case 2:
			return int64(r.Int63())
		case 3:
			return fmt.Sprintf("k%d", r.Intn(1000))
		case 4:
			if len(*pool) > 0 && r.Intn(2) == 0 {
				return (*pool)[r.Intn(len(*pool))]
			}
			x := &X{N: int64(r.Int())}
			*pool = append(*pool, x)
			return x
		case 5:
			return (*X)(nil)
		default:
			return nil
		}
	}
	for seed := range int64(50) {
		r := rand.New(rand.NewSource(seed))
		var pool []*X
		orig := map[any]int{}
		for i, n := 0, r.Intn(6); i < n; i++ {
			orig[anyKey(r, &pool)] = r.Intn(100)
		}
		b1, err := gbon.Marshal(orig)
		if err != nil {
			t.Fatalf("seed %d: marshal: %v", seed, err)
		}
		b2, err := gbon.Marshal(orig)
		if err != nil || !bytes.Equal(b1, b2) {
			t.Fatalf("seed %d: determinism on iface skeletons", seed)
		}
		dec := gbon.NewDecoder(bytes.NewReader(b1))
		if err := dec.Register(int(0), int32(0), int64(0), "", X{}, &X{}); err != nil {
			t.Fatal(err)
		}
		var rt map[any]int
		if err := dec.Decode(&rt); err != nil {
			t.Fatalf("seed %d: decode: %v", seed, err)
		}
		if len(rt) != len(orig) {
			t.Fatalf("seed %d: len %d vs %d", seed, len(rt), len(orig))
		}
		keyRepr := func(k any) string {
			rv := reflect.ValueOf(k)
			if rv.Kind() == reflect.Pointer {
				if rv.IsNil() {
					return "*nil"
				}
				return "*X:" + fmt.Sprint(rv.Elem().Interface())
			}
			return fmt.Sprintf("%T:%v", k, k)
		}
		classes := func(m map[any]int) map[string]int {
			out := map[string]int{}
			for k, v := range m {
				out[keyRepr(k)+"=>"+fmt.Sprint(v)]++
			}
			return out
		}
		if !reflect.DeepEqual(classes(orig), classes(rt)) {
			t.Fatalf("seed %d: ==-equivalence classes or values changed", seed)
		}
	}
	// Reject property: unhashable dynamic keys are unbuildable in
	// Go (insertion panics), so the reject surface is
	// crafted input: mutations of unhashable-key streams decode to a
	// sentinel or success, never a panic.
	craft, _ := hexD("67626F6E0000" +
		"D36C146D61705B696E74657266616365207B7D5D696E74" +
		"D66C0C696E74657266616365207B7D" + "D863696E7408" +
		"A1" + "D1655B5D696E74" + "D863696E7408" + "01" + "22")
	for i := range 25 {
		r := rand.New(rand.NewSource(int64(i) + 1000))
		mut := append([]byte{}, craft...)
		mut[r.Intn(len(mut))] ^= byte(1 << r.Intn(8))
		dec := gbon.NewDecoder(bytes.NewReader(mut))
		if err := dec.Register([]int{}); err != nil {
			t.Fatal(err)
		}
		m := map[any]int{}
		err := dec.Decode(&m)
		if err != nil && !errors.Is(err, gbon.ErrFormat) &&
			!errors.Is(err, gbon.ErrBudget) && !errors.Is(err, gbon.ErrUnsupported) &&
			!errors.Is(err, gbon.ErrIO) && !errors.Is(err, io.EOF) {
			t.Fatalf("mutation %d: non-sentinel %v", i, err)
		}
		var ae *gbon.Error
		if err != nil && !errors.Is(err, io.EOF) {
			if !errors.As(err, &ae) || ae.Class() == "" {
				t.Fatalf("mutation %d: error not class-recoverable: %v", i, err)
			}
		}
	}
	// Inheritance: external REF onto a key pointer — stable bytes.
	for seed := range int64(10) {
		r := rand.New(rand.NewSource(seed))
		x := &X{N: int64(r.Int())}
		root := struct {
			M map[*X]*X
			P *X
		}{M: map[*X]*X{x: x}, P: x}
		b1, err := gbon.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		b2, err := gbon.Marshal(root)
		if err != nil || !bytes.Equal(b1, b2) {
			t.Fatalf("REF counterexample seed %d: unstable bytes", seed)
		}
	}
}

// Random topological graphs (map/slice/any pools, fan-in sharing,
// cycle injection) — Marshal terminates, and the round trip preserves
// reference identity: every pair of paths that met one object in the
// original meets one object in the copy, with matching slot geometry.
func TestPropertyTopology(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		g := &topoGen{r: rand.New(rand.NewSource(seed)),
			maxDepth: 3 + int(seed%4), maxFan: 1 + int(seed%3),
			reusePct: 15, cyclePct: 15}
		v := g.value(0)
		data, err := marshalGuarded(t, v)
		if err != nil {
			t.Fatalf("seed %d: Marshal: %v", seed, err)
		}
		dec := gbon.NewDecoder(bytes.NewReader(data))
		if err := dec.Register(map[string]any{}, []any{}, int64(0), "", false); err != nil {
			t.Fatal(err)
		}
		var out any
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("seed %d: Decode: %v", seed, err)
		}
		_, dups := topoPaths(v)
		for _, p := range dups {
			a := resolvePath(out, p[0])
			b := resolvePath(out, p[1])
			if a == nil || b == nil {
				t.Fatalf("seed %d: path does not resolve in copy: %v / %v", seed, p[0], p[1])
			}
			av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
			if av.Kind() != bv.Kind() {
				t.Fatalf("seed %d: dup pair kinds diverge: %v vs %v", seed, av.Kind(), bv.Kind())
			}
			if topoKeyOf(av) != topoKeyOf(bv) {
				t.Fatalf("seed %d: shared node split into two: %v vs %v", seed, p[0], p[1])
			}
			if av.Kind() == reflect.Slice {
				if av.Len() != bv.Len() || av.Cap() != bv.Cap() {
					t.Fatalf("seed %d: slot geometry diverges: len/cap %d/%d vs %d/%d",
						seed, av.Len(), av.Cap(), bv.Len(), bv.Cap())
				}
			}
			if av.Kind() == reflect.Map {
				// Mutation observability: one shared map.
				av.SetMapIndex(reflect.ValueOf("witness"), reflect.ValueOf(int64(1)))
				if bv.MapIndex(reflect.ValueOf("witness")).IsValid() {
					av.SetMapIndex(reflect.ValueOf("witness"), reflect.Value{})
				} else {
					t.Fatalf("seed %d: map mutation through one path invisible through the other: %v", seed, p[0])
				}
			}
		}
	}
}

// A shared DAG (fan-out 2, depth d, 2^d root-to-leaf paths) encodes
// linearly in the number of unique reference nodes — no literal blow-up —
// and decodes to exactly one map per node.
func TestPropertyDAGLinear(t *testing.T) {
	const cPerNode = 32     // gate constant: bytes per unique node
	const descOverhead = 96 // fixed descriptors/names independent of d
	for _, d := range []int{2, 5, 10, 15, 20} {
		var root any = int64(7) // shared leaf
		for range d {
			m := map[string]any{}
			m["l"], m["r"] = root, root
			root = m
		}
		data, err := marshalGuarded(t, root)
		if err != nil {
			t.Fatalf("d=%d: Marshal: %v", d, err)
		}
		// Unique reference nodes: d map nodes (the leaf is a value).
		U := d
		if got := len(data); got > descOverhead+cPerNode*U {
			t.Fatalf("d=%d: %d bytes exceed linearity bound %d (c=%d/node)",
				d, got, descOverhead+cPerNode*U, cPerNode)
		}
		dec := gbon.NewDecoder(bytes.NewReader(data))
		if err := dec.Register(map[string]any{}, int64(0)); err != nil {
			t.Fatal(err)
		}
		var out any
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("d=%d: Decode: %v", d, err)
		}
		seen := map[uintptr]bool{}
		var count func(x any) bool // reports true when x is a map node
		count = func(x any) bool {
			m, ok := x.(map[string]any)
			if !ok {
				return false
			}
			p := reflect.ValueOf(m).Pointer()
			if seen[p] {
				return true
			}
			seen[p] = true
			for _, v := range m {
				count(v)
			}
			return true
		}
		count(out)
		if len(seen) != d {
			t.Fatalf("d=%d: decoded %d distinct maps, want %d (blow-up)", d, len(seen), d)
		}
	}
}

// The window count over one backing is a generated parameter —
// aliasing holds across the dimensional axis, not at one fixed size.
func TestPropertyWindowAxis(t *testing.T) {
	for seed := range int64(8) {
		r := rand.New(rand.NewSource(seed))
		n := 3 + r.Intn(17)
		base := make([]int64, 2*n+2)
		for i := range base {
			base[i] = int64(i)
		}
		wins := make([][]int64, n)
		for i := range wins {
			wins[i] = base[2*i : 2*i+4 : 2*i+4] // stride 2, width 4: neighbors overlap
		}
		fan := fanWin{W: wins}
		out, err := gbon.Marshal(fan)
		if err != nil {
			t.Fatalf("s=%d: Marshal: %v", n, err)
		}
		var got fanWin
		if err := gbon.Unmarshal(out, &got); err != nil {
			t.Fatalf("s=%d: Unmarshal: %v", n, err)
		}
		for i := range got.W {
			if len(got.W[i]) != len(fan.W[i]) || cap(got.W[i]) != cap(fan.W[i]) {
				t.Fatalf("s=%d window %d: geometry %d/%d, want %d/%d",
					n, i, len(got.W[i]), cap(got.W[i]), len(fan.W[i]), cap(fan.W[i]))
			}
		}
		got.W[0][3] = -1 // base[3] lies inside window 1 ([2,6))
		if got.W[1][1] != -1 {
			t.Fatalf("s=%d: overlapping window did not observe the write", n)
		}
	}
}

// fanWin is a generated fan of windows over one backing.
type fanWin struct{ W [][]int64 }

// Multi-Encode, stream-lifetime intern: the same map object across
// two Encode calls on one stream is a REF into the first value's records —
// map identity is stream-transportable, mirroring pointer interning.
func TestPropertyMapInternStream(t *testing.T) {
	m := map[string]int{"a": 1}
	var buf bytes.Buffer
	enc := gbon.NewEncoder(&buf)
	if err := enc.Encode(mapShare{X: m, Y: m}); err != nil {
		t.Fatal(err)
	}
	first := buf.Len()
	if err := enc.Encode(mapShare{X: m, Y: m}); err != nil {
		t.Fatal(err)
	}
	// Second value: REF to the type descriptor (C0), then the body with
	// both fields as REF into the map record interned by the first value
	// (stream lifetime).
	if hex.EncodeToString(buf.Bytes()[first:]) != "c0b0caca" {
		t.Fatalf("second value = %s, want c0b0caca", hex.EncodeToString(buf.Bytes()[first:]))
	}
	dec := gbon.NewDecoder(&buf)
	var a, b mapShare
	if err := dec.Decode(&a); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&b); err != nil {
		t.Fatal(err)
	}
	if a.X == nil || a.Y == nil || b.X == nil || b.Y == nil {
		t.Fatal("nil map in decoded stream")
	}
	a.X["z"] = 9
	if _, visible := a.Y["z"]; !visible {
		t.Fatal("identity inside first value lost")
	}
	if _, visible := b.X["z"]; !visible {
		t.Fatal("map identity does not cross Encode values (stream lifetime broken)")
	}
}

// Crafted allocation bombs — L·es >> input
// bytes, es ∈ {1,8,16} × L ∈ {10^6..10^9} × E=0 — always decode to
// ErrBudget|ErrFormat under a limit below the claimed charge, never to
// success, an OOM, or a panic, within a wall-clock guard.
func TestPropertyAllocBudget(t *testing.T) {
	start := time.Now()
	axes := []struct {
		name     string
		elemDesc *cDesc
		es       uint64
		example  any
	}{
		{"int8", descInt8, 1, []int8(nil)},
		{"int64", descInt64, 8, []int64(nil)},
		{"complex128", descComplex128, 16, []complex128(nil)},
	}
	for _, ax := range axes {
		for _, L := range []uint64{1000000, 10000000, 100000000, 1000000000} {
			stream := craftedSliceStream(ax.elemDesc, L)
			d := gbon.NewDecoder(bytes.NewReader(stream))
			if err := d.Register(ax.example); err != nil {
				t.Fatalf("Register(%s): %v", ax.name, err)
			}
			// MaxBytes below the claimed charge: decode must refuse.
			d.SetLimits(gbon.Limits{MaxBytes: 999999})
			out := reflect.New(reflect.TypeOf(ax.example)).Interface()
			err := d.Decode(out)
			if err == nil {
				t.Fatalf("%s L=%d: decode succeeded on a claimed %d-byte backing",
					ax.name, L, L*ax.es)
			}
			if !errors.Is(err, gbon.ErrBudget) && !errors.Is(err, gbon.ErrFormat) {
				t.Fatalf("%s L=%d: err = %v, want ErrBudget|ErrFormat", ax.name, L, err)
			}
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Fatalf("wall-clock guard tripped at %s L=%d after %v", ax.name, L, elapsed)
			}
		}
	}
	// mutation axis: L varied around the g51 boundary on the seed vector
	for _, L := range []uint64{99999999, 100000001, 50000000} {
		stream := craftedSliceStream(descInt64, L)
		d := gbon.NewDecoder(bytes.NewReader(stream))
		if err := d.Register([]int64(nil)); err != nil {
			t.Fatalf("Register: %v", err)
		}
		d.SetLimits(gbon.Limits{MaxBytes: 999999})
		var vi []int64
		if err := d.Decode(&vi); !errors.Is(err, gbon.ErrBudget) {
			t.Fatalf("mutation L=%d: err = %v, want ErrBudget", L, err)
		}
	}
}

// Legal deep values at the depth-budget
// boundary — below the limit encodes, above fails with ErrBudget (never a
// fatal stack, never a hang), wall-clock guarded. The boundary pair runs
// on an explicit small limit: the OK-path at the default boundary is
// quadratic in the existing per-level path-string construction; the
// default-side claim is carried by the over-budget case, and the
// 10^5-OK leg lives in TestEncodeDepthBudget.
func TestPropertyEncodeDepthBoundary(t *testing.T) {
	chain := deepChain
	// threshold pair on a configured limit: 2 frames per level + leaf
	enc := gbon.NewEncoder(&bytes.Buffer{})
	if err := enc.SetLimits(gbon.Limits{MaxDepth: 33}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(chain(16)); err != nil { // depth 33 ≤ 33
		t.Fatalf("d=16 (depth 33, MaxDepth 33): %v", err)
	}
	if err := enc.Encode(chain(17)); !errors.Is(err, gbon.ErrBudget) { // depth 35 > 33
		t.Fatalf("d=17 (depth 35, MaxDepth 33): err = %v, want ErrBudget", err)
	}
	// default side: past the encode default the scan guard refuses with
	// ErrBudget — never a fatal stack, never a hang (regular builds; race
	// frames are ~4x heavier and the 1M-frame walk is covered by the
	// configured-threshold pair above)
	if !raceEnabled {
		start := time.Now()
		if _, err := gbon.Marshal(deepChain(1000002)); !errors.Is(err, gbon.ErrBudget) {
			t.Fatalf("d=1000002 at defaults: err = %v, want ErrBudget", err)
		}
		if elapsed := time.Since(start); elapsed > 30*time.Second {
			t.Fatalf("wall-clock guard tripped after %v", elapsed)
		}
	}
}

// Windows over one memory, sequential Encodes, mutations
// between them. The oracle is an independent model of the join rule
// (guard-on-join): containment of the cap-window in the closed record's
// region plus bitwise-zero elided tail within the len-window; a repeat
// that fails the guard opens a fresh record (the sole body-duplication
// carve-out). Verified: P1 representable windows alias one backing
// (pointer deltas match window offsets), P2 every decoded window equals
// its model bytes (snapshot for the dense prefix, zeros only where the
// memory was zero), P3 unrepresentable repeats decode correctly as
// duplicates, P4 the byte stream is deterministic under replay.
type cvWin struct{ off, ln, cap int }

type cvStep struct {
	w    cvWin
	muts [][2]int // memory mutations applied after this Encode
}

func TestPropertyCrossValueWindows(t *testing.T) {
	g := rand.New(rand.NewSource(47))
	for iter := range 300 {
		n := 1 + g.Intn(12)
		mem := make([]byte, n)
		for i := range mem {
			if g.Intn(3) != 0 {
				mem[i] = byte(1 + g.Intn(255))
			}
		}
		steps := make([]cvStep, 1+g.Intn(6))
		for si := range steps {
			off := g.Intn(n + 1)
			rest := n - off
			ln := g.Intn(rest + 1)
			capw := ln + g.Intn(rest-ln+1)
			steps[si].w = cvWin{off, ln, capw}
			var muts [][2]int
			for k := 0; k < g.Intn(3); k++ {
				muts = append(muts, [2]int{g.Intn(n), 1 + g.Intn(255)})
			}
			steps[si].muts = muts
		}
		run := func() (stream []byte, decoded [][]byte, ptrs []uintptr) {
			m := append([]byte(nil), mem...)
			var buf bytes.Buffer
			e := gbon.NewEncoder(&buf)
			for _, st := range steps {
				if err := e.Encode(m[st.w.off : st.w.off+st.w.ln : st.w.off+st.w.cap]); err != nil {
					t.Fatalf("Encode: %v", err)
				}
				for _, mu := range st.muts {
					m[mu[0]] = byte(mu[1])
				}
			}
			dec := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
			for range steps {
				var v []byte
				if err := dec.Decode(&v); err != nil {
					t.Fatalf("Decode: %v", err)
				}
				decoded = append(decoded, v)
				ptrs = append(ptrs, reflect.ValueOf(v).Pointer())
			}
			return buf.Bytes(), decoded, ptrs
		}
		stream1, dec1, ptr1 := run()
		stream2, _, _ := run()
		if !bytes.Equal(stream1, stream2) {
			t.Fatalf("iter %d: encode not deterministic", iter)
		}
		// model
		type rec struct{ origin, L, E int }
		m := append([]byte(nil), mem...)
		var recs []rec
		joined := make([]int, len(steps)) // record index per step, -1 = fresh
		for si, st := range steps {
			w := st.w
			joined[si] = -1
			for ri, r := range recs {
				if w.off < r.origin || w.off+w.cap > r.origin+r.L {
					continue
				}
				ok := true
				for k := 0; k < w.ln; k++ {
					if w.off+k-r.origin >= r.E && m[w.off+k] != 0 {
						ok = false
						break
					}
				}
				if ok {
					joined[si] = ri
					break
				}
			}
			if joined[si] == -1 {
				E := 0
				for i := w.off + w.cap - 1; i >= w.off; i-- {
					if m[i] != 0 {
						E = i + 1 - w.off
						break
					}
				}
				recs = append(recs, rec{w.off, w.cap, E})
				joined[si] = len(recs) - 1
			}
			for _, mu := range st.muts {
				m[mu[0]] = byte(mu[1])
			}
		}
		// P2: element-level oracle, replaying the emission-time memory per
		// record (fresh: the live bytes at that step; joined: the snapshot
		// bytes of the record's birth step).
		m = append([]byte(nil), mem...)
		snaps := make([][]byte, len(recs))
		birth := make([]int, len(recs))
		for ri := range recs {
			birth[ri] = -1
		}
		for si, st := range steps {
			if birth[joined[si]] == -1 {
				r := recs[joined[si]]
				e := r.E
				snaps[joined[si]] = append([]byte(nil), m[r.origin:r.origin+e]...)
				birth[joined[si]] = si
			}
			for _, mu := range st.muts {
				m[mu[0]] = byte(mu[1])
			}
		}
		for si, st := range steps {
			r := recs[joined[si]]
			snap := snaps[joined[si]]
			for k := 0; k < st.w.ln; k++ {
				gi := st.w.off + k
				var want byte
				if gi-r.origin < r.E {
					want = snap[gi-r.origin]
				}
				if dec1[si][k] != want {
					t.Fatalf("iter %d step %d elem %d: got %02X want %02X (join=%v) steps=%s mem=% x",
						iter, si, k, dec1[si][k], want, joined[si] != birth[joined[si]], safeDescValue(steps), mem)
				}
			}
		}
		// P1: steps joined to one record alias one backing — pointer
		// deltas equal window-offset deltas; a mutation through one is
		// visible through the other on overlap.
		for i := range steps {
			for j := i + 1; j < len(steps); j++ {
				if joined[i] != joined[j] {
					continue
				}
				// len-0 windows carry an implementation-defined data
				// pointer (Go normalizes it), so pointer identity is
				// observable only through non-empty windows
				if steps[i].w.ln == 0 || steps[j].w.ln == 0 {
					continue
				}
				di, dj := int(ptr1[i]), int(ptr1[j])
				if di-dj != (steps[i].w.off - steps[j].w.off) {
					t.Fatalf("iter %d: steps %d,%d joined rec %d but pointers differ by %d, want %d; steps=%s mem=% x",
						iter, i, j, joined[i], di-dj, steps[i].w.off-steps[j].w.off, safeDescValue(steps), mem)
				}
			}
		}
	}
}

// --- BIGINT properties (the unbounded integer domain) ---

// bigintBoundarys sweeps the domain edges the wire grammar pivots on.
func bigintBoundarys() []*big.Int {
	p2 := big.NewInt(2)
	var out []*big.Int
	add := func(v *big.Int) { out = append(out, new(big.Int).Set(v)) }
	add(big.NewInt(0))
	add(big.NewInt(1))
	add(big.NewInt(-1))
	add(new(big.Int).SetInt64(math.MaxInt64))
	add(new(big.Int).SetInt64(math.MinInt64))
	for _, e := range []uint{63, 64, 65, 66} {
		p := new(big.Int).Lsh(p2, e) // 2^e
		for _, d := range []int64{-1, 0, 1} {
			v := new(big.Int).Add(p, big.NewInt(d))
			add(v)
			add(new(big.Int).Neg(v))
		}
	}
	return out
}

// randomBig returns a random integer of the given bit length with a
// random sign (bitLen 0 yields zero).
func randomBig(r *rand.Rand, bitLen int) *big.Int {
	v := new(big.Int)
	if bitLen > 0 {
		v.SetBits(randomWords(r, bitLen))
		if r.Intn(2) == 0 {
			v.Neg(v)
		}
	}
	return v
}

// randomWords builds a nonzero-top-word absolute value of exactly bitLen
// bits: the top word carries its high bit.
func randomWords(r *rand.Rand, bitLen int) []big.Word {
	words := (bitLen + 63) / 64
	out := make([]big.Word, words)
	for i := range out[:words-1] {
		out[i] = big.Word(r.Uint64())
	}
	topBits := uint(bitLen-(words-1)*64) % 64
	if topBits == 0 {
		topBits = 64
	}
	var top uint64
	if topBits == 64 {
		top = r.Uint64() | 1<<63
	} else {
		top = r.Uint64() & ((1 << topBits) - 1)
		if top == 0 {
			top = 1
		}
		if topBits > 1 && top < 1<<(topBits-1) {
			top |= 1 << (topBits - 1)
		}
	}
	out[words-1] = big.Word(top)
	return out
}

// P1: the whole integer domain round-trips as a byte fixpoint —
// Marshal∘Unmarshal∘Marshal reproduces the first encoding exactly
// (canonical determinism), values compare equal, and two encodes
// of one value are identical.
func TestPropertyBigintRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(145))
	vals := bigintBoundarys()
	for range 120 {
		vals = append(vals, randomBig(r, r.Intn(2001)))
	}
	for _, v := range vals {
		m1, err := gbon.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", v, err)
		}
		m1b, err := gbon.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(m1, m1b) {
			t.Fatalf("non-deterministic encode for %v", v)
		}
		var got *big.Int
		if err := gbon.Unmarshal(m1, &got); err != nil {
			t.Fatalf("Unmarshal(%v): %v", v, err)
		}
		if v.Sign() == 0 {
			if got != nil {
				t.Fatalf("zero decoded to %v, want nil coincidence byte read", got)
			}
		} else if got == nil || got.Cmp(v) != 0 {
			t.Fatalf("value mismatch: in %v, out %v", v, got)
		}
		m2, err := gbon.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(m1, m2) {
			t.Fatalf("byte fixpoint broken for %v:\n m1 % x\n m2 % x", v, m1, m2)
		}
	}
}

// P2: token continuity at the int64 range — the bare-ARG body of a big
// value equals the low nibble and payload of the int64 token of the same
// number (forms below 2^64; byte continuity of the existing range).
func TestPropertyBigintContinuity(t *testing.T) {
	r := rand.New(rand.NewSource(146))
	vals := []int64{math.MinInt64, math.MinInt64 + 1, -2, -1, 0, 1, 2,
		math.MaxInt64 - 1, math.MaxInt64, 1 << 32, -(1 << 32), 255, 256, 65535, 65536}
	for range 60 {
		vals = append(vals, r.Int63())
		if r.Intn(2) == 0 {
			vals[len(vals)-1] = -vals[len(vals)-1]
		}
	}
	for _, n := range vals {
		bigStream := mustMarshal(t, big.NewInt(n))
		intStream := mustMarshal(t, n)
		// kind-15 DESC = 2 selector bytes + 1 name token + 7 name bytes;
		// int64 DESC = 1 + 1 + 5 + 1 (width)
		bigBody := bigStream[6+10:]
		intTok := intStream[6+8:]
		if bigBody[0] != intTok[0]&0x0F {
			t.Fatalf("form nibble drift at %d: %#x vs %#x", n, bigBody[0], intTok[0]&0x0F)
		}
		if !bytes.Equal(bigBody[1:], intTok[1:]) {
			t.Fatalf("payload drift at %d:\n big % x\n int % x", n, bigBody[1:], intTok[1:])
		}
	}
}

// P4: equal values in distinct pointers of one stream encode identically
// — the descriptor literal appears once, the repeat is a REF, and both
// bodies are byte-equal (intern order is the byte-DFS).
func TestPropertyBigintIntern(t *testing.T) {
	r := rand.New(rand.NewSource(147))
	for range 50 {
		v := randomBig(r, r.Intn(2001))
		a, b := new(big.Int).Set(v), new(big.Int).Set(v)
		stream := mustMarshal(t, []any{a, b})
		lit := append([]byte{0xDC, 0x0F, 0x67}, []byte("big.Int")...)
		if c := bytes.Count(stream, lit); c != 1 {
			t.Fatalf("descriptor literal count %d for %v", c, v)
		}
		if !bytes.Contains(stream, []byte{0xC5}) {
			t.Fatalf("no REF to the first descriptor for %v: % x", v, stream)
		}
		var out []any
		if err := gbon.Unmarshal(stream, &out); err != nil {
			t.Fatalf("decode %v: %v", v, err)
		}
		pa, _ := out[0].(*big.Int)
		pb, _ := out[1].(*big.Int)
		if pa == nil || pb == nil || pa.Cmp(v) != 0 || pb.Cmp(v) != 0 {
			t.Fatalf("intern decode mismatch for %v: %v %v", v, pa, pb)
		}
	}
}

// Pointer-identity bigint map keys round-trip: distinct slots — including
// equal values in distinct pointers — survive as separate keys, values
// compare by magnitude (KO-2a identity layer over the BIGINT key form).
func TestBigintMapKeysRoundtrip(t *testing.T) {
	k1 := big.NewInt(7)
	k2 := big.NewInt(7) // equal value, distinct slot: identity, not value, keys the map
	k3 := new(big.Int).Neg(new(big.Int).SetBytes([]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0}))
	in := map[*big.Int]string{k1: "a", k2: "b", k3: "wide"}
	b := mustMarshal(t, in)
	var out map[*big.Int]string
	if err := gbon.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode map[*big.Int]: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("slot count %d, want 3 (identity keys must not collapse)", len(out))
	}
	for k, v := range in {
		hit := false
		for ko, vo := range out {
			if ko != nil && ko.Cmp(k) == 0 && vo == v {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("key %v (%q) not recovered by value", k, v)
		}
	}
}

// tk6Box and tk6Root build the pointer/slice key-collision graph: the
// pointer pointee address equals the slice data pointer (&s[0] == data(s)).
type tk6Box struct {
	X    int64
	Tail []int64
}

type tk6Root struct {
	P *tk6Box
	S []tk6Box
	M map[string][]int64
}

// The scan visited set must keep pointer encounters and slice encounters
// in separate key spaces: p := &s[0] makes the pointee address equal the
// slice data pointer, so a shared key space lets whichever of {p, s} is
// scanned first mask the other's element walk. All Tail sub-slices are
// windows over one backing array, so a masked walk loses slot
// registrations and changes the Marshal bytes — and since the carrier is
// a map (random iteration order per pass), the bytes also become
// order-dependent. Correct separation keeps the bytes stable across
// repeated Marshal calls and the round trip preserves every value.
func TestScanVisitedPointerSliceCollision(t *testing.T) {
	backing := make([]int64, 12)
	for i := range backing {
		backing[i] = int64(100 + i)
	}
	s := make([]tk6Box, 3)
	for i := range s {
		s[i] = tk6Box{X: int64(i), Tail: backing[i*3 : i*3+2 : i*3+3]}
	}
	p := &s[0]
	m := map[string][]int64{"t": backing[9:11:12]}
	root := map[string]any{"p": p, "s": s, "m": m}

	b1, err := gbon.Marshal(root)
	if err != nil {
		t.Fatalf("Marshal map carrier: %v", err)
	}
	for k := range 64 {
		b2, err := gbon.Marshal(root)
		if err != nil {
			t.Fatalf("Marshal map carrier again: %v", err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("collision graph bytes depend on scan order (pass %d)", k)
		}
	}

	typed := tk6Root{P: &s[0], S: s, M: m}
	bt, err := gbon.Marshal(typed)
	if err != nil {
		t.Fatalf("Marshal typed carrier: %v", err)
	}
	var out tk6Root
	if err := gbon.Unmarshal(bt, &out); err != nil {
		t.Fatalf("Unmarshal typed carrier: %v", err)
	}
	if out.P == nil || out.P.X != 0 || len(out.P.Tail) != 2 || cap(out.P.Tail) != 3 ||
		out.P.Tail[0] != 100 || out.P.Tail[1] != 101 {
		t.Fatalf("pointer branch lost its walk: P=%s", safeDescValue(out.P))
	}
	if len(out.S) != 3 {
		t.Fatalf("slice branch lost its walk: len(S)=%d", len(out.S))
	}
	for i := range out.S {
		w := out.S[i].Tail
		if out.S[i].X != int64(i) || len(w) != 2 || cap(w) != 3 ||
			w[0] != int64(100+3*i) || w[1] != int64(101+3*i) {
			t.Fatalf("slice element %d lost its walk: %s", i, safeDescValue(out.S[i]))
		}
	}
	if tw := out.M["t"]; len(tw) != 2 || tw[0] != 109 || tw[1] != 110 {
		t.Fatalf("map branch lost its walk: %v", tw)
	}
}

// tk-elision-bigint-zero — marshal → unmarshal(self) stays green for value
// graphs whose slice tails are runs of nil/zero big integers at depth
// (table-generative over leaf counts, values and zero-tail shapes).
func TestPropBigintZeroTailRT(t *testing.T) {
	type leaf struct{ W *big.Int }
	type node struct {
		Kids []leaf
		Tag  string
	}
	r := rand.New(rand.NewSource(20260901))
	for i := range 200 {
		var n node
		for j := 0; j < r.Intn(6); j++ {
			n.Kids = append(n.Kids, leaf{W: big.NewInt(int64(r.Intn(3) - 1))})
		}
		for _, w := range []*big.Int{nil, big.NewInt(0), nil} {
			n.Kids = append(n.Kids, leaf{W: w})
		}
		b, err := gbon.Marshal(n)
		if err != nil {
			t.Fatalf("iter %d: marshal: %v", i, err)
		}
		var out node
		if err := gbon.Unmarshal(b, &out); err != nil {
			t.Fatalf("iter %d: unmarshal: %v", i, err)
		}
		b2, err := gbon.Marshal(out)
		if err != nil {
			t.Fatalf("iter %d: re-marshal: %v", i, err)
		}
		if !bytes.Equal(b, b2) {
			t.Fatalf("iter %d: re-marshal drift: %x vs %x", i, b, b2)
		}
	}
}

// TestSafeDescCyclicProp: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicProp(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	s := safeDescValue(m)
	if len(s) == 0 || len(s) > descMaxRunes {
		t.Fatalf("safeDescValue render out of bounds (%d runes): %q", len(s), s)
	}
	if !strings.Contains(s, "<cycle>") {
		t.Fatalf("safeDescValue misses the cycle marker: %q", s)
	}
}
