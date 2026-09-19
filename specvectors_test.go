package gbon_test

// Reader of the spec-repository conformance corpus (vectors/ + manifest.json).
// The corpus is data derived from the wire-format tables; this test is its
// independent consumer: decode == IR (identity-aware) and encode == bytes
// for ok vectors, sentinel-class asserts for negatives. The corpus root
// is /spec (gate mount) or the directory named by GBON_SPEC_VECTORS.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gbon-format/gbon-go"
)

type corpusVector struct {
	ID        string         `json:"id"`
	Desc      string         `json:"desc"`
	Tags      []string       `json:"tags"`
	Verdict   string         `json:"verdict"`
	Direction string         `json:"direction"`
	Bytes     string         `json:"bytes"`
	IR        map[string]any `json:"ir"`
	Deriv     []string       `json:"deriv"`
	Class     string         `json:"class"`
	Limits    map[string]int `json:"limits"`
	// Narrowing channel of negative vectors only: the declared target
	// binding and the expected absolute offset of the reject.
	NarrowTarget string `json:"narrow_target,omitempty"`
	NarrowOffset int    `json:"narrow_offset,omitempty"`
	rawIR        map[string]any
}

type corpusFile struct {
	Category string          `json:"category"`
	Vectors  []*corpusVector `json:"vectors"`
}

type corpusLedgerRow struct {
	Test       string `json:"test"`
	Level      string `json:"level"`
	Claim      string `json:"claim"`
	Sub        string `json:"sub"`
	Instrument string `json:"instrument"`
}

type corpusManifest struct {
	Version    int `json:"version"`
	Categories []struct {
		File    string   `json:"file"`
		Section string   `json:"section"`
		Covers  []string `json:"covers"`
	} `json:"categories"`
	Vectors []struct {
		ID       string `json:"id"`
		Category string `json:"category"`
		Verdict  string `json:"verdict"`
	} `json:"vectors"`
	Ledger []corpusLedgerRow `json:"ledger"`
}

// Local types bound to corpus wire names through RegisterAs.

type vecPoint struct{ X, Y int64 }
type vecPair struct{ X, Y []int64 }
type vecShare struct{ P1, P2 *int64 }
type vecNode struct {
	Next *vecNode
	V    int64
}
type vecRing struct {
	Next *vecRing
	V    int64
	P    *int64
}
type vecEvo1 struct{ A int64 }
type vecEvo2 struct {
	A int64
	B string
}
type vecTemp float64
type vecScals struct {
	B bool
	I int64
	U uint64
	F float32
	C complex64
	S string
}
type vecDM struct {
	A map[string]int64
	B map[string]int64
	S string
}
type vecS1 struct{ F1 any }
type vecGrainS struct{ A int64 }
type vecGrainBox struct {
	P *int64
	Q *vecGrainS
}
type vecGrainA struct{ X int64 }
type vecGrainB struct{ X int64 }
type vecGrainH struct {
	PA *vecGrainA
	PB *vecGrainB
}
type vecGrainT struct {
	H int64
	L struct{}
	M int64
}
type vecGrainW struct {
	PL *struct{}
	PM *int64
}
type vecGrainN struct {
	V    int64
	Next *vecGrainN
}
type vecGrainF struct {
	Q *bool
	P *int64
}
type vecAliased struct{ A, B, C []int64 }
type vecAppended struct{ P, Q []int64 }
type vecGrainWin struct {
	W []int64
	P *[3]int64
}
type vecMapShare2 struct{ F2 map[string]int64 }
type vecViewShare2 struct{ W []int64 }
type vecWrongSort2 struct{ M map[string]int64 }

var corpusBindings = []struct {
	name string
	ex   any
}{
	{"vec.point", vecPoint{}},
	{"vec.pair", vecPair{}},
	{"vec.share", vecShare{}},
	{"vec.node", vecNode{}},
	{"vec.ring", vecRing{}},
	{"vec.evo2", vecEvo2{}},
	{"vec.temp", vecTemp(0)},
	{"vec.scals", vecScals{}},
	{"vec.dm", vecDM{}},
	{"*vec.pair", (*vecPair)(nil)},
	{"*vec.share", (*vecShare)(nil)},
	{"*vec.node", (*vecNode)(nil)},
	{"*vec.ring", (*vecRing)(nil)},
	{"*vec.evo2", (*vecEvo2)(nil)},
	{"map[*vec.node]int64", map[*vecNode]int64{}},
	{"main.S1", vecS1{}},
	{"*main.S1", (*vecS1)(nil)},
	{"main.S", vecGrainS{}},
	{"*main.S", (*vecGrainS)(nil)},
	{"main.Box", vecGrainBox{}},
	{"*main.Box", (*vecGrainBox)(nil)},
	{"main.A", vecGrainA{}},
	{"*main.A", (*vecGrainA)(nil)},
	{"main.B", vecGrainB{}},
	{"*main.B", (*vecGrainB)(nil)},
	{"main.H", vecGrainH{}},
	{"*main.H", (*vecGrainH)(nil)},
	{"main.T", vecGrainT{}},
	{"main.W", vecGrainW{}},
	{"*main.W", (*vecGrainW)(nil)},
	{"main.N", vecGrainN{}},
	{"*main.N", (*vecGrainN)(nil)},
	{"main.F", vecGrainF{}},
	{"*main.F", (*vecGrainF)(nil)},
	{"vec.aliased", vecAliased{}},
	{"vec.appended", vecAppended{}},
	{"vec.grain", vecGrainWin{}},
	{"vec.mapshare", vecMapShare2{}},
	{"vec.viewshare", vecViewShare2{}},
	{"vec.wrongsort", vecWrongSort2{}},
	{"map[string]vec.aliased", map[string]vecAliased{}},
	{"map[string]vec.appended", map[string]vecAppended{}},
	{"map[string]vec.grain", map[string]vecGrainWin{}},
	{"struct {}", struct{}{}},
	{"*struct {}", (*struct{})(nil)},
}

// corpusMapTypes resolves explicit map type names carried by map nodes.
var corpusMapTypes = map[string]reflect.Type{
	"map[*int64]string":       reflect.TypeFor[map[*int64]string](),
	"map[*vec.node]int64":     reflect.TypeFor[map[*vecNode]int64](),
	"map[string]int64":        reflect.TypeFor[map[string]int64](),
	"map[string]vec.aliased":  reflect.TypeFor[map[string]vecAliased](),
	"map[string]vec.appended": reflect.TypeFor[map[string]vecAppended](),
	"map[string]vec.grain":    reflect.TypeFor[map[string]vecGrainWin](),
}

// ifaceTypeNames resolves interface payload type tags that appear in the corpus.
var ifaceTypeNames = map[string]reflect.Type{
	"int64":                   reflect.TypeFor[int64](),
	"int32":                   reflect.TypeFor[int32](),
	"float64":                 reflect.TypeFor[float64](),
	"*int64":                  reflect.TypeFor[*int64](),
	"map[string]interface {}": reflect.TypeFor[map[string]any](),
	"[]interface {}":          reflect.TypeFor[[]any](),
}

func corpusRoot(t *testing.T) string {
	if d := os.Getenv("GBON_SPEC_VECTORS"); d != "" {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			t.Fatalf("GBON_SPEC_VECTORS=%s is not a readable directory: %v", d, err)
		}
		return d
	}
	if fi, err := os.Stat("/spec"); err == nil && fi.IsDir() {
		return "/spec"
	}
	return ""
}

func loadCorpus(t *testing.T) []*corpusVector {
	root := corpusRoot(t)
	if root == "" {
		t.Skip("conformance corpus not mounted (set GBON_SPEC_VECTORS or mount /spec)")
	}
	mb, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man corpusManifest
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	// schema coupling: a version outside the accepted set is a red, not
	// a skip; a version-2 manifest without its ledger block would
	// silently no-op every ledger-driven behavior
	if man.Version != 1 && man.Version != 2 {
		t.Fatalf("corpus schema version %d outside the accepted set {1,2}", man.Version)
	}
	if man.Version == 2 && len(man.Ledger) == 0 {
		t.Fatalf("corpus schema version 2 carries no ledger block")
	}
	var out []*corpusVector
	for _, cat := range man.Categories {
		pb, err := os.ReadFile(filepath.Join(root, cat.File))
		if err != nil {
			t.Fatalf("category %s: %v", cat.File, err)
		}
		var cf corpusFile
		if err := json.Unmarshal(pb, &cf); err != nil {
			t.Fatalf("category %s parse: %v", cat.File, err)
		}
		for _, v := range cf.Vectors {
			v.rawIR = v.IR
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].ID, out[j].ID
		num := func(s string) int {
			n := 0
			for _, c := range s[2:] {
				n = n*10 + int(c-'0')
			}
			return n
		}
		return num(a) < num(b)
	})
	if len(out) == 0 {
		t.Fatal("empty corpus")
	}
	return out
}

// censusClassShape mirrors the declaration block a fixture-class file
// may carry: its presence marks the census class.
type censusClassShape struct {
	Declaration *struct {
		Grammar    string `json:"grammar"`
		CensusSize int    `json:"census_size"`
		Families   []struct {
			Name string `json:"name"`
		} `json:"families"`
	} `json:"declaration"`
}

// ledgerFixtureClass derives the class of a cited fixture from its
// category file: a declaration block marks the census class, its
// absence the fixed-vector class.
func ledgerFixtureClass(t *testing.T, root, category string) string {
	t.Helper()
	pb, err := os.ReadFile(filepath.Join(root, "vectors", category+".json"))
	if err != nil {
		t.Fatalf("fixture class %s: %v", category, err)
	}
	var shape censusClassShape
	if err := json.Unmarshal(pb, &shape); err != nil {
		t.Fatalf("fixture class %s parse: %v", category, err)
	}
	if shape.Declaration != nil {
		return "census"
	}
	return "vector"
}

// materializeLedgerCensus validates the census class vocabulary the
// loader consumes: the grammar bound and the family names. The pair
// construction and byte-compare legs ride the differential runner.
func materializeLedgerCensus(t *testing.T, root, category string) {
	t.Helper()
	pb, err := os.ReadFile(filepath.Join(root, "vectors", category+".json"))
	if err != nil {
		t.Fatalf("census class %s: %v", category, err)
	}
	var shape censusClassShape
	if err := json.Unmarshal(pb, &shape); err != nil {
		t.Fatalf("census class %s parse: %v", category, err)
	}
	if shape.Declaration == nil {
		t.Fatalf("census class %s carries no declaration", category)
	}
	if shape.Declaration.Grammar != "canongrain.Graphs" {
		t.Fatalf("census class %s: unknown grammar %q", category, shape.Declaration.Grammar)
	}
	if shape.Declaration.CensusSize <= 0 {
		t.Fatalf("census class %s: census size %d", category, shape.Declaration.CensusSize)
	}
	known := map[string]bool{"census-pair": true, "aliased-backing": true, "multi-grain-address": true}
	for _, f := range shape.Declaration.Families {
		if !known[f.Name] {
			t.Fatalf("census class %s: unknown fixture family %q", category, f.Name)
		}
	}
}

// materializeLedgerVector runs one fixed-byte fixture through the
// adapter operations: materialize from the IR, encode to bytes,
// decode to observables.
func materializeLedgerVector(t *testing.T, v *corpusVector) {
	t.Helper()
	data, err := hex.DecodeString(v.Bytes)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	switch v.Verdict {
	case "ok":
		runOKVector(t, v, data)
	case "format":
		runNegativeVector(t, v, data, gbon.ErrFormat)
	case "budget":
		runNegativeVector(t, v, data, gbon.ErrBudget)
	default:
		t.Fatalf("unknown verdict %q", v.Verdict)
	}
}

// TestSpecLedger: the frozen schema consumed — every F row resolves to
// a live fixture and materializes through its fixture class; level I
// rows resolve in the gbon-go tree through the spec gate's lint leg.
func TestSpecLedger(t *testing.T) {
	root := corpusRoot(t)
	if root == "" {
		t.Skip("conformance corpus not mounted (set GBON_SPEC_VECTORS or mount /spec)")
	}
	mb, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man corpusManifest
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Version != 1 && man.Version != 2 {
		t.Fatalf("corpus schema version %d outside the accepted set {1,2}", man.Version)
	}
	if man.Version == 2 && len(man.Ledger) == 0 {
		t.Fatalf("corpus schema version 2 carries no ledger block")
	}
	if len(man.Ledger) == 0 {
		t.Skip("no ledger block to consume")
	}
	byID := map[string]*corpusVector{}
	for _, v := range loadCorpus(t) {
		byID[v.ID] = v
	}
	catOf := map[string]string{}
	for _, mv := range man.Vectors {
		catOf[mv.ID] = mv.Category
	}
	for _, r := range man.Ledger {
		switch r.Level {
		case "F":
			v := byID[r.Test]
			cat := catOf[r.Test]
			if v == nil || cat == "" {
				t.Fatalf("ledger row cites unknown fixture %s", r.Test)
			}
			if class := ledgerFixtureClass(t, root, cat); class != "vector" {
				t.Fatalf("ledger row %s: unknown fixture class %q", r.Test, class)
			}
			materializeLedgerVector(t, v)
		case "I":
			// the live-test existence check is the lint's cross-repo leg
		default:
			t.Fatalf("ledger row %s: level %q", r.Test, r.Level)
		}
	}
	// fixture classes of the corpus: every declaration-carrying
	// category validates its census vocabulary (the fixed-vector class
	// rides the rows above)
	for _, cat := range man.Categories {
		name := filepath.Base(cat.File)
		name = name[:len(name)-len(".json")]
		if ledgerFixtureClass(t, root, name) == "census" {
			materializeLedgerCensus(t, root, name)
		}
	}
}

func corpusLimits(v *corpusVector) gbon.Limits {
	var l gbon.Limits
	for k, n := range v.Limits {
		switch k {
		case "max_depth":
			l.MaxDepth = n
		case "max_nodes":
			l.MaxNodes = n
		case "max_bytes":
			l.MaxBytes = n
		case "max_map_pairs":
			l.MaxMapPairs = n
		case "max_slice_len":
			l.MaxSliceLen = n
		}
	}
	return l
}

func newCorpusEncoder() (*gbon.Encoder, *bytes.Buffer) {
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	for _, b := range corpusBindings {
		if err := e.RegisterAs(b.name, b.ex); err != nil {
			panic(err)
		}
	}
	return e, &buf
}

func newCorpusDecoder(data []byte) *gbon.Decoder {
	d := gbon.NewDecoder(bytes.NewReader(data))
	for _, b := range corpusBindings {
		if err := d.RegisterAs(b.name, b.ex); err != nil {
			panic(err)
		}
	}
	if err := d.Register(
		(*int64)(nil),
		map[string]int64{},
		map[float64]int64{},
		map[int64]string{},
		map[complex64]string{},
		map[*int64]string{},
		map[any]int64{},
		map[string]any{},
	); err != nil {
		panic(err)
	}
	return d
}

// builder: IR (generic maps) -> Go values with identity binding. Labels map
// to addressable storage so REF edges yield the same object (identity
// binding of the corpus graph model).

type irBuilder struct {
	nodes    map[string]map[string]any
	storage  map[string]reflect.Value
	built    map[string]bool
	building map[string]bool
}

func nodeKind(n map[string]any) string {
	if n == nil {
		return ""
	}
	k, _ := n["kind"].(string)
	return k
}

func (b *irBuilder) resolve(x any) (map[string]any, bool) {
	if m, ok := x.(map[string]any); ok {
		if ref, ok := m["ref"].(string); ok {
			n := b.nodes[ref]
			return n, n != nil
		}
		return m, true
	}
	return nil, false
}

func irString(n map[string]any, k string) string { s, _ := n[k].(string); return s }

func irInt(n map[string]any, k string) int {
	switch x := n[k].(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

// build constructs the Go value for a value position. Addressable storage is
// kept per label; REF positions return the shared storage's value.
func (b *irBuilder) build(x any) (reflect.Value, error) {
	if m, ok := x.(map[string]any); ok {
		if ref, ok := m["ref"].(string); ok {
			if st, ok := b.storage[ref]; ok && (b.built[ref] || b.building[ref]) {
				return st, nil
			}
			n := b.nodes[ref]
			if n == nil {
				return reflect.Value{}, fmt.Errorf("dangling ref %s", ref)
			}
			v, err := b.buildNode(ref, n)
			if err != nil {
				return reflect.Value{}, err
			}
			if v.Kind() != reflect.Pointer && v.Kind() != reflect.Map && v.CanAddr() {
				return v, nil
			}
			st := reflect.New(v.Type()).Elem()
			st.Set(v)
			b.storage[ref] = st
			return st, nil
		}
	}
	n, ok := b.resolve(x)
	if !ok {
		return reflect.Value{}, fmt.Errorf("unresolvable value position %s", safeDescValue(x))
	}
	if lbl, has := n["node"].(string); has {
		if st, ok := b.storage[lbl]; ok {
			return st, nil
		}
		return b.buildNode(lbl, n)
	}
	return reflect.Value{}, fmt.Errorf("inline node without label: %s", safeDescValue(n))
}

func (b *irBuilder) buildNode(label string, n map[string]any) (reflect.Value, error) {
	if b.built[label] {
		if st, ok := b.storage[label]; ok {
			return st, nil
		}
	}
	b.building[label] = true
	defer func() {
		b.building[label] = false
		b.built[label] = true
	}()
	switch nodeKind(n) {
	case "int":
		s := irString(n, "value")
		v := new(big.Int)
		v.SetString(s, 10)
		if v.IsInt64() {
			return reflect.ValueOf(int64(v.Int64())), nil
		}
		return reflect.ValueOf(v), nil
	case "bigint":
		if n["value"] == nil {
			return reflect.ValueOf((*big.Int)(nil)), nil
		}
		v := new(big.Int)
		v.SetString(irString(n, "value"), 10)
		return reflect.ValueOf(*v), nil
	case "uint":
		v := new(big.Int)
		v.SetString(irString(n, "value"), 10)
		return reflect.ValueOf(uint64(v.Uint64())), nil
	case "bool":
		cv, _ := n["value"].(bool)
		return reflect.ValueOf(cv), nil
	case "float":
		bits, err := hex.DecodeString(irString(n, "bits"))
		if err != nil {
			return reflect.Value{}, err
		}
		switch len(bits) {
		case 4:
			return reflect.ValueOf(math.Float32frombits(uint32(bits[0])<<24 | uint32(bits[1])<<16 | uint32(bits[2])<<8 | uint32(bits[3]))), nil
		case 8:
			var u uint64
			for _, c := range bits {
				u = u<<8 | uint64(c)
			}
			fv := reflect.ValueOf(math.Float64frombits(u))
			if name, ok := n["type"].(string); ok {
				for _, bd := range corpusBindings {
					if bd.name == name && fv.Type().ConvertibleTo(reflect.TypeOf(bd.ex)) {
						fv = fv.Convert(reflect.TypeOf(bd.ex))
					}
				}
			}
			return fv, nil
		}
		return reflect.Value{}, fmt.Errorf("decimal128 position is a reader degrade")
	case "complex":
		rb, _ := hex.DecodeString(irString(n, "real"))
		ib, _ := hex.DecodeString(irString(n, "imag"))
		be := func(bs []byte) uint64 {
			var u uint64
			for _, c := range bs {
				u = u<<8 | uint64(c)
			}
			return u
		}
		if len(rb) == 4 {
			return reflect.ValueOf(complex(math.Float32frombits(uint32(be(rb))), math.Float32frombits(uint32(be(ib))))), nil
		}
		return reflect.ValueOf(complex(math.Float64frombits(be(rb)), math.Float64frombits(be(ib)))), nil
	case "string":
		if s, ok := n["value"].(string); ok {
			return reflect.ValueOf(s), nil
		}
		raw, err := hex.DecodeString(irString(n, "bytes"))
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(string(raw)), nil
	case "blob":
		return b.buildBlob(n)
	case "nil":
		var t reflect.Type
		switch irString(n, "sort") {
		case "pointer":
			// typed nil of a derivable pointer chain (payload (*any)(nil))
			if dt, ok := deriveIfacePtrChainForTest(irString(n, "type")); ok {
				t = dt
				break
			}
			t = reflect.TypeFor[*int64]()
		case "slice":
			t = reflect.TypeFor[[]int64]()
		case "map":
			t = reflect.TypeFor[map[string]int64]()
		default:
			t = reflect.TypeFor[any]()
		}
		return reflect.Zero(t), nil
	case "array", "view":
		return b.buildBacking(n)
	case "map":
		return b.buildMap(n)
	case "struct":
		return b.buildStruct(n)
	case "cell":
		// interior cell: the field l-value of a built struct node
		if err := b.materialize(irString(n, "of")); err != nil {
			return reflect.Value{}, err
		}
		st, ok := b.storage[irString(n, "of")]
		if !ok || !st.IsValid() {
			return reflect.Value{}, fmt.Errorf("cell of %s: no storage", irString(n, "of"))
		}
		fl := st.FieldByName(irString(n, "name"))
		if !fl.IsValid() {
			return reflect.Value{}, fmt.Errorf("cell field %q not found", irString(n, "name"))
		}
		b.storage[label] = fl
		return fl, nil
	case "iface":
		// iface-slot storage: the slot cell registers before its payload
		// builds, so self-referential payloads resolve through {ref};
		// a preregistered field slot is reused as the node's storage;
		// a chain tag two or more pointers deep is a pointer variable,
		// not an interface slot
		st := reflect.New(reflect.TypeFor[any]()).Elem()
		if chainDepth(irString(n, "type")) >= 2 {
			st = reflect.New(reflect.TypeFor[*any]()).Elem()
			b.storage[label] = st
		} else if pre, ok := b.storage[label]; ok && pre.IsValid() && pre.CanSet() && pre.Kind() == reflect.Interface {
			st = pre
		} else {
			b.storage[label] = st
		}
		inner, err := b.build(n["value"])
		if err != nil {
			return reflect.Value{}, err
		}
		if !inner.IsValid() {
			inner = reflect.Zero(reflect.TypeFor[any]())
		}
		// a {ref} payload under a bound pointer type is a REF in a
		// pointer position: it takes the target storage's address (REF
		// identity), not a copy of its value
		if bt, ok := b.bindingType(irString(n, "type")); ok && bt.Kind() == reflect.Pointer {
			if rv, ok := n["value"].(map[string]any); ok {
				if ref, ok := rv["ref"].(string); ok {
					if err := b.materialize(ref); err != nil {
						return reflect.Value{}, err
					}
					if target, ok := b.storage[ref]; ok && target.CanAddr() && target.Type() == bt.Elem() {
						inner = target.Addr()
					}
				}
			}
		}
		// a {ref} payload under a derivable-chain declared type is a REF
		// in a pointer position: it takes the target storage's address
		// (REF identity), not a copy of its value
		if _, isChain := deriveIfacePtrChainForTest(irString(n, "type")); isChain {
			if rv, ok := n["value"].(map[string]any); ok {
				if ref, ok := rv["ref"].(string); ok {
					if err := b.materialize(ref); err != nil {
						return reflect.Value{}, err
					}
					if target, ok := b.storage[ref]; ok && target.CanAddr() {
						inner = target.Addr()
					}
				}
			}
		}
		// the declared dynamic type fixes the Go representation (KO-4:
		// int64(1) and int32(1) are distinct keys)
		if t, ok := ifaceTypeNames[irString(n, "type")]; ok && inner.Type() != t {
			if inner.Kind() == reflect.Interface && !inner.IsNil() {
				inner = inner.Elem()
			}
			if inner.Type().ConvertibleTo(t) {
				inner = inner.Convert(t)
			}
		}
		st.Set(inner)
		return st, nil
	}
	return reflect.Value{}, fmt.Errorf("unbuildable kind %q", nodeKind(n))
}

// chainDepth counts the leading pointer stars of a declared type name.
func chainDepth(t string) int {
	d := 0
	for d < len(t) && t[d] == '*' {
		d++
	}
	return d
}

// materialize builds a referenced node unless it is built or in flight,
// so address-taking {ref} edges never observe an unbuilt storage.
func (b *irBuilder) materialize(ref string) error {
	n := b.nodes[ref]
	if n == nil || b.built[ref] || b.building[ref] {
		return nil
	}
	_, err := b.buildNode(ref, n)
	return err
}

// bindingType resolves a declared type name through the corpus bindings.
func (b *irBuilder) bindingType(name string) (reflect.Type, bool) {
	for _, bd := range corpusBindings {
		if bd.name == name {
			return reflect.TypeOf(bd.ex), true
		}
	}
	return nil, false
}

// inferType picks a value's Go type from the IR node shape alone, without
// building it: containers must register their storage before children.
func (b *irBuilder) inferType(x any) reflect.Type {
	n, ok := b.resolve(x)
	if !ok {
		return nil
	}
	switch nodeKind(n) {
	case "int":
		s, _ := n["value"].(string)
		v, ok2 := new(big.Int).SetString(s, 10)
		if ok2 && !v.IsInt64() {
			return reflect.TypeFor[*big.Int]()
		}
		return reflect.TypeFor[int64]()
	case "bigint":
		return reflect.TypeFor[*big.Int]()
	case "uint":
		return reflect.TypeFor[uint64]()
	case "bool":
		return reflect.TypeOf(false)
	case "float":
		if bits, ok := n["bits"].(string); ok && len(bits)/2 == 4 {
			return reflect.TypeFor[float32]()
		}
		return reflect.TypeFor[float64]()
	case "complex":
		if r, ok := n["real"].(string); ok && len(r)/2 == 4 {
			return reflect.TypeFor[complex64]()
		}
		return reflect.TypeFor[complex128]()
	case "string":
		return reflect.TypeFor[string]()
	case "blob":
		return reflect.TypeFor[[]byte]()
	case "nil":
		switch irString(n, "sort") {
		case "pointer":
			return reflect.TypeFor[*int64]()
		case "slice":
			return reflect.TypeFor[[]int64]()
		case "map":
			return reflect.TypeFor[map[string]int64]()
		}
		return reflect.TypeFor[any]()
	case "array":
		var et reflect.Type = reflect.TypeFor[int64]()
		if elems, ok := n["elements"].([]any); ok && len(elems) > 0 {
			if t := b.inferType(elems[0]); t != nil {
				et = t
			}
		}
		return reflect.SliceOf(et) // array-as-backing feeds slices only
	case "view":
		if t := b.inferType(n["backing"]); t != nil {
			return t
		}
		return reflect.TypeFor[[]int64]()
	case "map":
		var kt, vt reflect.Type = reflect.TypeFor[string](), reflect.TypeFor[any]()
		if pairs, ok := n["pairs"].([]any); ok && len(pairs) > 0 {
			if p, ok2 := pairs[0].(map[string]any); ok2 {
				if t := b.inferType(p["key"]); t != nil {
					kt = t
				}
				if t := b.inferType(p["value"]); t != nil {
					vt = t
				}
			}
		}
		// an explicit map type (the wire descriptor name) fixes key and
		// element representations outright
		if mt, ok := corpusMapTypes[irString(n, "type")]; ok {
			return mt
		}
		if kn, ok := b.resolve(firstKey(n)); ok && nodeKind(kn) == "struct" {
			if bt := b.inferType(firstKey(n)); bt != nil {
				kt = reflect.PointerTo(bt)
			}
		}
		return reflect.MapOf(kt, vt)
	case "struct":
		for _, bd := range corpusBindings {
			if bd.name == irString(n, "type") {
				return reflect.TypeOf(bd.ex)
			}
		}
	case "iface":
		// an interface position is an any-slot; the dynamic type rides
		// on the payload, not on the slot type
		return reflect.TypeFor[any]()
	}
	return reflect.TypeOf(any(nil))
}

func (b *irBuilder) buildBlob(n map[string]any) (reflect.Value, error) {
	raw, _ := hex.DecodeString(irString(n, "bytes"))
	full := make([]byte, irInt(n, "len"))
	copy(full, raw)
	st := reflect.New(reflect.TypeOf(full)).Elem()
	st.Set(reflect.ValueOf(full))
	if lbl, has := n["node"].(string); has {
		b.storage[lbl] = st
	}
	return st, nil
}

// buildBacking builds an array node (dense elements + implicit zero tail)
// or a view node (slice window over the backing, extent as capacity). An
// array node carrying a backing ref is an array-grain window over shared
// storage: a plain slice window, converted to the array-pointer grain by
// the consuming field.
func (b *irBuilder) buildBacking(n map[string]any) (reflect.Value, error) {
	var target map[string]any
	grain := nodeKind(n) == "array" && n["backing"] != nil
	if nodeKind(n) == "view" || grain {
		t, ok := b.resolve(n["backing"])
		if !ok {
			return reflect.Value{}, fmt.Errorf("view without backing")
		}
		target = t
	} else {
		target = n
	}
	var arr reflect.Value
	var err error
	if tl, ok := target["node"].(string); ok {
		if st, ok2 := b.storage[tl]; ok2 {
			arr = st
		}
	}
	if !arr.IsValid() {
		if nodeKind(target) == "blob" {
			arr, err = b.buildBlob(target)
		} else {
			arr, err = b.buildArray(target)
		}
		if err != nil {
			return reflect.Value{}, err
		}
	}
	if err != nil {
		return reflect.Value{}, err
	}
	if nodeKind(n) == "array" && !grain {
		return arr, nil
	}
	off, ln, ext := irInt(n, "off"), irInt(n, "len"), irInt(n, "extent")
	if grain {
		return arr.Slice(off, off+ln), nil
	}
	sl := arr.Slice3(off, off+ln, off+ext)
	return sl, nil
}

func (b *irBuilder) buildArray(n map[string]any) (reflect.Value, error) {
	elems, _ := n["elements"].([]any)
	L := irInt(n, "len")
	if L == 0 && len(elems) == 0 {
		// length may live only in len for empty backings
		L = irInt(n, "len")
	}
	var et reflect.Type = reflect.TypeFor[int64]()
	if len(elems) > 0 {
		if t := b.inferType(elems[0]); t != nil {
			et = t
		}
	}
	if b.inferType(n) != nil && b.inferType(n).Kind() == reflect.Slice {
		et = b.inferType(n).Elem()
	}
	arr := reflect.New(reflect.ArrayOf(L, et)).Elem()
	if lbl, has := n["node"].(string); has {
		b.storage[lbl] = arr
	}
	for i, e := range elems {
		ev, err := b.build(e)
		if err != nil {
			return reflect.Value{}, err
		}
		if !ev.IsValid() {
			continue
		}
		if ev.Type() != et {
			if ev.Type().ConvertibleTo(et) {
				ev = ev.Convert(et)
			} else {
				et2 := reflect.TypeOf(any(nil))
				arr2 := reflect.New(reflect.ArrayOf(L, et2)).Elem()
				for j := range i {
					arr2.Index(j).Set(reflect.ValueOf(arr.Index(j).Interface()))
				}
				arr = arr2
				et = et2
			}
		}
		arr.Index(i).Set(ev)
	}
	return arr, nil
}

func firstKey(n map[string]any) any {
	if pairs, ok := n["pairs"].([]any); ok && len(pairs) > 0 {
		if p, ok2 := pairs[0].(map[string]any); ok2 {
			return p["key"]
		}
	}
	return nil
}

func (b *irBuilder) buildMap(n map[string]any) (reflect.Value, error) {
	pairs, _ := n["pairs"].([]any)
	kt, vt := b.inferType(n).Key(), b.inferType(n).Elem()
	m := reflect.MakeMapWithSize(reflect.MapOf(kt, vt), len(pairs))
	if lbl, has := n["node"].(string); has {
		b.storage[lbl] = m
	}
	for _, pr := range pairs {
		p, _ := pr.(map[string]any)
		if p == nil {
			continue
		}
		kv, err := b.build(p["key"])
		if err != nil {
			return reflect.Value{}, err
		}
		if kt.Kind() == reflect.Pointer && kv.Kind() != reflect.Pointer && kv.CanAddr() {
			kv = kv.Addr()
		}
		vv, err := b.build(p["value"])
		if err != nil {
			return reflect.Value{}, err
		}
		if !vv.IsValid() {
			vv = reflect.Zero(vt)
		}
		if vv.Type() != vt && vv.Type().ConvertibleTo(vt) {
			vv = vv.Convert(vt)
		}
		if os.Getenv("VDBG") != "" {
			fmt.Printf("DBG kt=%v vt=%v vv=%T\n", kt, vt, vv.Interface())
		}
		m.SetMapIndex(kv, vv)
	}
	return m, nil
}

func (b *irBuilder) buildStruct(n map[string]any) (reflect.Value, error) {
	var proto any
	for _, bd := range corpusBindings {
		if bd.name == irString(n, "type") {
			proto = bd.ex
		}
	}
	if proto == nil {
		return reflect.Value{}, fmt.Errorf("struct type %q not bound", irString(n, "type"))
	}
	st := reflect.New(reflect.TypeOf(proto)).Elem()
	if lbl, has := n["node"].(string); has {
		if pre, ok := b.storage[lbl]; ok && pre.IsValid() {
			st = pre
		} else {
			b.storage[lbl] = st
		}
	}
	fields, _ := n["fields"].([]any)
	for _, f := range fields {
		fd, _ := f.(map[string]any)
		if fd == nil {
			continue
		}
		name := irString(fd, "name")
		fv := st.FieldByName(name)
		if !fv.IsValid() {
			return reflect.Value{}, fmt.Errorf("field %q not found", name)
		}
		bv, err := b.build(fd["value"])
		if err != nil {
			return reflect.Value{}, err
		}
		if !bv.IsValid() {
			continue
		}
		// named-grain conversion: a ref annotated with a bound grain
		// type materializes through the legal pointer conversion of an
		// identical-underlying pair, aliasing the target storage
		if rm, ok := fd["value"].(map[string]any); ok && bv.Kind() != reflect.Pointer {
			if gname := irString(rm, "type"); gname != "" {
				if _, gok := b.bindingType(gname); gok && fv.Kind() == reflect.Pointer && bv.CanAddr() {
					if pt := bv.Addr().Type(); pt != fv.Type() && pt.ConvertibleTo(fv.Type()) {
						bv = bv.Addr().Convert(fv.Type())
					}
				}
			}
		}
		if bv.Type() != fv.Type() {
			if fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Array &&
				bv.Kind() == reflect.Slice && bv.Type().ConvertibleTo(fv.Type()) {
				// an array-grain window materializes as the slice-to-
				// array-pointer conversion, aliasing the backing
				bv = bv.Convert(fv.Type())
			} else if fv.Kind() == reflect.Pointer && bv.Kind() != reflect.Pointer && bv.CanAddr() {
				bv = bv.Addr()
			} else if bv.Type().ConvertibleTo(fv.Type()) {
				bv = bv.Convert(fv.Type())
			} else if fv.Kind() == reflect.Interface {
				iv := reflect.New(fv.Type()).Elem()
				iv.Set(bv)
				bv = iv
			} else {
				return reflect.Value{}, fmt.Errorf("field %s: %s not assignable to %s", name, bv.Type(), fv.Type())
			}
		}
		fv.Set(bv)
	}
	return st, nil
}

// buildRoot builds the corpus root: a {"ref": l} root is a pointer to the
// labeled node's storage (the pointer-ness rides on the stream descriptor).
func (b *irBuilder) buildRoot(ir map[string]any) (reflect.Value, error) {
	switch r := ir["root"].(type) {
	case string:
		n := b.nodes[r]
		if n == nil {
			return reflect.Value{}, fmt.Errorf("root label %s missing", r)
		}
		return b.buildNode(r, n)
	case map[string]any:
		if ref, ok := r["ref"].(string); ok {
			n := b.nodes[ref]
			if n == nil {
				return reflect.Value{}, fmt.Errorf("root ref %s missing", ref)
			}
			inner, err := b.buildNode(ref, n)
			if err != nil {
				return reflect.Value{}, err
			}
			// struct/int/map targets: pointer root takes the storage address
			if st, ok := b.storage[ref]; ok && st.CanAddr() {
				return reflect.ValueOf(st.Addr().Interface()), nil
			}
			switch nodeKind(n) {
			case "map":
				p := reflect.New(inner.Type())
				p.Elem().Set(inner)
				return p, nil
			}
			return reflect.ValueOf(inner.Interface()), nil
		}
	}
	return reflect.Value{}, fmt.Errorf("bad root %s", safeDescValue(ir["root"]))
}

func newBuilder(ir map[string]any) (*irBuilder, error) {
	b := &irBuilder{nodes: map[string]map[string]any{}, storage: map[string]reflect.Value{}, built: map[string]bool{}, building: map[string]bool{}}
	ns, _ := ir["nodes"].([]any)
	for _, x := range ns {
		n, _ := x.(map[string]any)
		if n == nil {
			continue
		}
		if lbl, ok := n["node"].(string); ok {
			b.nodes[lbl] = n
		}
	}
	b.preslot()
	return b, nil
}

// preslot preregisters struct storages and their interface-field slots:
// an iface node named by a field value receives the field's storage,
// binding the slot to its container the way the decoder does.
func (b *irBuilder) preslot() {
	for lbl, n := range b.nodes {
		if nodeKind(n) != "struct" {
			continue
		}
		var proto any
		for _, bd := range corpusBindings {
			if bd.name == irString(n, "type") {
				proto = bd.ex
			}
		}
		if proto == nil {
			continue
		}
		st := reflect.New(reflect.TypeOf(proto)).Elem()
		b.storage[lbl] = st
		fields, _ := n["fields"].([]any)
		for _, f := range fields {
			fd, _ := f.(map[string]any)
			if fd == nil {
				continue
			}
			rv, ok := fd["value"].(map[string]any)
			if !ok {
				continue
			}
			ref, ok := rv["ref"].(string)
			if !ok {
				continue
			}
			if target := b.nodes[ref]; target != nil && nodeKind(target) == "iface" {
				if fv := st.FieldByName(irString(fd, "name")); fv.IsValid() && fv.Kind() == reflect.Interface {
					b.storage[ref] = fv
				}
			}
		}
	}
}

// cmpValues: identity-aware lockstep comparison. Pointers and maps pair by
// identity (visited set); maps compare by canonical bytes (the model's own
// equality); floats compare bitwise; slices compare nil-ness, length,
// capacity (extent) and elements.
func cmpValues(t *testing.T, a, b reflect.Value, path string, visited map[uintptr]uintptr) bool {
	fail := func(why string) bool {
		t.Errorf("%s: %s\n  decoded: %s\n  built:   %s", path, why, safeDescReflect(a, 0, map[uintptr]bool{}), safeDescReflect(b, 0, map[uintptr]bool{}))
		return false
	}
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Type() != b.Type() {
		return fail(fmt.Sprintf("type %s != %s", a.Type(), b.Type()))
	}
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				return fail("nil-ness mismatch")
			}
			return true
		}
		pa, pb := a.Pointer(), b.Pointer()
		if seen, ok := visited[pa]; ok {
			return seen == pb
		}
		visited[pa] = pb
		return cmpValues(t, a.Elem(), b.Elem(), path+"*", visited)
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				return fail("nil-ness mismatch (nil vs empty)")
			}
			return true
		}
		pa, pb := a.Pointer(), b.Pointer()
		if seen, ok := visited[pa]; ok {
			return seen == pb
		}
		visited[pa] = pb
		ab, bb := canonicalBytes(a), canonicalBytes(b)
		if ab != bb {
			return fail("map canonical bytes differ")
		}
		return true
	case reflect.Slice:
		if a.IsNil() != b.IsNil() {
			return fail("nil-ness mismatch (nil vs empty)")
		}
		if a.Len() != b.Len() {
			return fail(fmt.Sprintf("len %d != %d", a.Len(), b.Len()))
		}
		if a.Cap() != b.Cap() {
			return fail(fmt.Sprintf("extent (cap) %d != %d", a.Cap(), b.Cap()))
		}
		pa, pb := a.Pointer(), b.Pointer()
		if seen, ok := visited[pa]; ok {
			return seen == pb
		}
		visited[pa] = pb
		for i := 0; i < a.Len(); i++ {
			if !cmpValues(t, a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i), visited) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !cmpValues(t, a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i), visited) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !cmpValues(t, a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name, visited) {
				return false
			}
		}
		return true
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				return fail("interface nil-ness mismatch (typed-nil vs nil)")
			}
			return true
		}
		return cmpValues(t, a.Elem(), b.Elem(), path+"/iface", visited)
	case reflect.Float32:
		if math.Float32bits(float32(a.Float())) != math.Float32bits(float32(b.Float())) {
			return fail("float32 bits differ")
		}
		return true
	case reflect.Float64:
		if math.Float64bits(a.Float()) != math.Float64bits(b.Float()) {
			return fail("float64 bits differ")
		}
		return true
	case reflect.Complex64, reflect.Complex128:
		cA, cB := a.Complex(), b.Complex()
		if math.Float64bits(real(cA)) != math.Float64bits(real(cB)) || math.Float64bits(imag(cA)) != math.Float64bits(imag(cB)) {
			return fail("complex bits differ")
		}
		return true
	}
	if !a.Equal(b) {
		return fail("values differ")
	}
	return true
}

// canonicalBytes: the model's own map equality - canonical form of both sides.
func canonicalBytes(m reflect.Value) string {
	return hex.EncodeToString(mustCorpusBytes(m.Interface()))
}

func mustCorpusBytes(v any) []byte {
	e, buf := newCorpusEncoder()
	if err := e.Encode(v); err != nil {
		return []byte(fmt.Sprintf("err:%v", err))
	}
	return buf.Bytes()
}

func TestSpecCorpus(t *testing.T) {
	for _, v := range loadCorpus(t) {
		t.Run(v.ID, func(t *testing.T) {
			data, err := hex.DecodeString(v.Bytes)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			switch v.Verdict {
			case "ok":
				runOKVector(t, v, data)
			case "format":
				runNegativeVector(t, v, data, gbon.ErrFormat)
			case "budget":
				runNegativeVector(t, v, data, gbon.ErrBudget)
			default:
				t.Fatalf("unknown verdict %q", v.Verdict)
			}
		})
	}
}

func corpusHasWideFloat(ir map[string]any) bool {
	found := false
	var walk func(x any)
	walk = func(x any) {
		if found {
			return
		}
		switch xx := x.(type) {
		case map[string]any:
			if nodeKind(xx) == "float" {
				if bits, ok := xx["bits"].(string); ok && len(bits)/2 == 16 {
					found = true
				}
			}
			for _, vv := range xx {
				walk(vv)
			}
		case []any:
			for _, vv := range xx {
				walk(vv)
			}
		}
	}
	walk(ir)
	return found
}

// slotRootedCorpusWraps marks the slot-root vectors whose decoded any
// target carries the root pointer: the built root compares boxed in
// wrap pointer covers, not dereferenced.
var slotRootedCorpusWraps = map[string]int{
	"V-100": 0, "V-102": 0, "V-104": 0, "V-105": 0,
}

// chainRootedCorpusVector marks the double-pointer chain root.
func chainRootedCorpusVector(id string) bool { return id == "V-101" }

// zsRootedCorpusVector marks the zero-size-marker root: the root value
// is the pointer itself (non-nil, zero-size pointee), decoded into its
// own grain rather than a dereferenced pointee slot.
func zsRootedCorpusVector(id string) bool { return id == "V-107" }

func runOKVector(t *testing.T, v *corpusVector, data []byte) {
	// projection degrade: decimal128 is valid on the wire, unsupported here
	if corpusHasWideFloat(v.rawIR) {
		dv := newCorpusDecoder(data)
		err := dv.Decode(new(any))
		if !errors.Is(err, gbon.ErrUnsupported) {
			t.Fatalf("decimal128 degrade: want ErrUnsupported, got %v", err)
		}
		return
	}
	bld, err := newBuilder(v.rawIR)
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	want, err := bld.buildRoot(v.rawIR)
	if err != nil {
		t.Fatalf("build root: %v", err)
	}
	// decode == IR (identity-aware); a pointer root decodes into its pointee
	// slot (the pointer descriptor matches value targets per the evolution
	// contract) and compares dereferenced
	dv := newCorpusDecoder(data)
	dv.SetLimits(corpusLimits(v))
	comp := want
	slotType := want.Type()
	if chainRootedCorpusVector(v.ID) || zsRootedCorpusVector(v.ID) {
		// a double-pointer chain root or a zero-size-marker root decodes
		// into its own grain: the deref comparison of value roots does
		// not apply
		comp = want
	} else if slotType.Kind() == reflect.Pointer && !want.IsNil() {
		slotType = slotType.Elem()
		comp = want.Elem()
	}
	target := reflect.New(slotType)
	if err := dv.Decode(target.Interface()); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wraps, ok := slotRootedCorpusWraps[v.ID]; ok {
		// slot-root roots: the decoded any target carries the root
		// pointer itself, so the built root compares boxed in wrap
		// pointer covers, not dereferenced
		covered := want
		for range wraps {
			p := reflect.New(reflect.TypeFor[*any]()).Elem()
			p.Set(covered)
			covered = p.Addr()
		}
		box := reflect.New(reflect.TypeFor[any]()).Elem()
		box.Set(covered)
		comp = box
	}
	if !cmpValues(t, target.Elem(), comp, "$", map[uintptr]uintptr{}) {
		t.Fatalf("decode != IR")
	}
	// schema property: a second, independently numbered construction of the
	// same IR (reversed labels, shuffled nodes) is identity-aware equal.
	if len(v.rawIR["nodes"].([]any)) >= 2 {
		want2 := rebuildRenumbered(t, v)
		comp2, compw := want, want2
		if want.Kind() == reflect.Pointer {
			compw, comp2 = want.Elem(), want2.Elem()
		}
		if !cmpValues(t, compw, comp2, "$renum", map[uintptr]uintptr{}) {
			t.Fatalf("renumbered construction not equal (IR node labels leaked into equality)")
		}
	}
	// encode == bytes (canonical round-trip claim)
	if v.Direction == "both" {
		got := mustCorpusBytes(want.Interface())
		if !bytes.Equal(got, data) {
			t.Fatalf("encode != corpus bytes\n  got:  %x\n  want: %x", got, data)
		}
	}
	// evolution extras: skip path and zero-fill (stream evolution contract)
	if v.ID == "V-52" {
		runEvolutionExtras(t, data)
	}
}

func rebuildRenumbered(t *testing.T, v *corpusVector) reflect.Value {
	ir := renumberIR(v.rawIR)
	bld, err := newBuilder(ir)
	if err != nil {
		t.Fatalf("builder2: %v", err)
	}
	w2, err := bld.buildRoot(ir)
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	return w2
}

// renumberIR reverses node labels and shuffles the nodes array: equality must
// run through REF structure, never through label spelling or array order.
func renumberIR(ir map[string]any) map[string]any {
	bs, _ := json.Marshal(ir)
	var cp map[string]any
	if err := json.Unmarshal(bs, &cp); err != nil {
		panic(err)
	}
	ns := cp["nodes"].([]any)
	labels := make([]string, 0, len(ns))
	for _, x := range ns {
		n := x.(map[string]any)
		if lbl, ok := n["node"].(string); ok {
			labels = append(labels, lbl)
		}
	}
	rev := map[string]string{}
	for i, l := range labels {
		rev[l] = labels[len(labels)-1-i]
	}
	var mustLabel func(x any) any
	mustLabel = func(x any) any {
		m, ok := x.(map[string]any)
		if !ok {
			return x
		}
		if l, ok := m["node"].(string); ok {
			m["node"] = rev[l]
		}
		if l, ok := m["ref"].(string); ok {
			m["ref"] = rev[l]
		}
		if l, ok := m["of"].(string); ok {
			m["of"] = rev[l]
		}
		if bb, ok := m["backing"].(map[string]any); ok {
			if l, ok := bb["ref"].(string); ok {
				bb["ref"] = rev[l]
			}
		}
		for _, k := range []string{"value", "key", "elements", "pairs", "fields"} {
			switch xv := m[k].(type) {
			case []any:
				for i := range xv {
					xv[i] = mustLabel(xv[i])
				}
			case map[string]any:
				m[k] = mustLabel(xv)
			}
		}
		return m
	}
	for i := range ns {
		ns[i] = mustLabel(ns[i])
	}
	if r, ok := cp["root"].(map[string]any); ok {
		if l, ok := r["ref"].(string); ok {
			r["ref"] = rev[l]
		}
	} else if l, ok := cp["root"].(string); ok {
		cp["root"] = rev[l]
	}
	// deterministic reorder: reversed node array
	for i, j := 0, len(ns)-1; i < j; i, j = i+1, j-1 {
		ns[i], ns[j] = ns[j], ns[i]
	}
	return cp
}

// TestCorpusDriftLayerNegativeFixture proves the drift layer's
// comparison rejects tampered expectations (SB-5 negative fixture): a
// flipped byte in a loaded expectation must compare unequal.

func runNegativeVector(t *testing.T, v *corpusVector, data []byte, sentinel error) {
	dv := newCorpusDecoder(data)
	dv.SetLimits(corpusLimits(v))
	if v.NarrowTarget != "" {
		runNarrowNegativeVector(t, v, dv, sentinel)
		return
	}
	var target any
	err := dv.Decode(&target)
	if err == nil {
		t.Fatalf("negative vector decoded successfully")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("want %v, got %v (class %q)", sentinel, err, errClass(err))
	}
	if v.Class != "" {
		if c := errClass(err); c != v.Class {
			t.Fatalf("class: want %q, got %q", v.Class, c)
		}
	}
	// truncated-input atomicity spot check: target stays zero
	if target != nil {
		t.Fatalf("partial value exposed on failed decode")
	}
}

// runNarrowNegativeVector decodes into the vector's declared narrow
// target: the reject fires at the kept position resolving a record the
// skipped prefix left unmaterialized; class and offset assert.
func runNarrowNegativeVector(t *testing.T, v *corpusVector, dv *gbon.Decoder, sentinel error) {
	t.Helper()
	var ex any
	for _, b := range corpusBindings {
		if b.name == v.NarrowTarget {
			ex = b.ex
			break
		}
	}
	if ex == nil {
		t.Fatalf("narrow_target %q has no corpus binding", v.NarrowTarget)
	}
	tgt := reflect.New(reflect.TypeOf(ex)).Interface()
	err := dv.Decode(tgt)
	if err == nil {
		t.Fatalf("negative vector decoded successfully into %s", v.NarrowTarget)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("want %v, got %v (class %q)", sentinel, err, errClass(err))
	}
	if c := errClass(err); c != v.Class {
		t.Fatalf("class: want %q, got %q", v.Class, c)
	}
	se, ok := errors.AsType[*gbon.Error](err)
	if !ok {
		t.Fatalf("not a *gbon.Error: %v", err)
	}
	if se.Offset != v.NarrowOffset {
		t.Fatalf("offset: want %d, got %d", v.NarrowOffset, se.Offset)
	}
}

func errClass(err error) string {
	if se, ok := errors.AsType[*gbon.Error](err); ok {
		return se.Class()
	}
	return ""
}

func runEvolutionExtras(t *testing.T, data []byte) {
	// v2 stream -> v1 target: field B skipped, A kept (evolution contract)
	d1 := gbon.NewDecoder(bytes.NewReader(data))
	if err := d1.RegisterAs("vec.evo2", vecEvo1{}); err != nil {
		t.Fatalf("bind v1: %v", err)
	}
	var v1 vecEvo1
	if err := d1.Decode(&v1); err != nil {
		t.Fatalf("skip decode: %v", err)
	}
	if v1.A != 7 {
		t.Fatalf("skip decode kept wrong A: %d", v1.A)
	}
	// v1 stream -> v2 target: absent field zero-filled
	var buf bytes.Buffer
	e := gbon.NewEncoder(&buf)
	if err := e.RegisterAs("vec.evo2", vecEvo1{}); err != nil {
		t.Fatalf("bind enc v1: %v", err)
	}
	if err := e.Encode(vecEvo1{A: 9}); err != nil {
		t.Fatalf("encode v1: %v", err)
	}
	d2 := gbon.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err := d2.RegisterAs("vec.evo2", vecEvo2{}); err != nil {
		t.Fatalf("bind v2: %v", err)
	}
	var v2 vecEvo2
	if err := d2.Decode(&v2); err != nil {
		t.Fatalf("zero-fill decode: %v", err)
	}
	if v2.A != 9 || v2.B != "" {
		t.Fatalf("zero-fill wrong: %s", safeDescValue(v2))
	}
}

// TestSafeDescCyclicVectors: the diagnostic renderer survives cyclic values —
// safeDescValue terminates with a bounded render where raw value
// formatting would overflow the fmt stack.
func TestSafeDescCyclicVectors(t *testing.T) {
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
