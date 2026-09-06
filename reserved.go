package gbon

import (
	"reflect"
	"slices"
	"strings"

	"github.com/gbon-format/gbon-go/internal/wire"
)

// ValidTombstoneName reports whether name is a well-formed tombstone
// name: a wire type name with an optional field component —
// [import/path/]pkg.Type[.field]. At most three non-empty dot components
// follow the path; field names carry no dots.
func ValidTombstoneName(name string) bool {
	rest := name
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		rest = name[i+1:]
	}
	if rest == "" {
		return false
	}
	comps := strings.Split(rest, ".")
	if len(comps) < 2 || len(comps) > 3 {
		return false
	}
	return !slices.Contains(comps, "")
}

// validReservedName is the reserved-name form: a well-formed tombstone
// name (a wire type name with an optional field component) or a single
// dot-free, slash-free type token (primitive and structural wire names).
func validReservedName(name string) bool {
	if ValidTombstoneName(name) {
		return true
	}
	return name != "" && !strings.ContainsAny(name, "/.")
}

// scanDescNames collects every wire name of t's descriptor graph under
// the encoder scope's naming, with coder-registered types resolved to
// their CODER leaves — the reserved-name gate's view of a value.
func scanDescNames(enc *codecEncoder, t reflect.Type) (map[string]bool, error) {
	hook := registerLeaf
	if len(enc.coders) > 0 {
		hook = func(rt reflect.Type) (*wire.Desc, bool) {
			if enc.coders[rt] != nil {
				return &wire.Desc{Kind: wire.KindCoder, Name: scopeName(namingOf(enc.asName), rt)}, true
			}
			return registerLeaf(rt)
		}
	}
	d, err := descWalk(t, "", make(map[reflect.Type]*wire.Desc), hook, namingOf(enc.asName))
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	visited := make(map[*wire.Desc]bool)
	var walk func(*wire.Desc)
	walk = func(d *wire.Desc) {
		if d == nil || visited[d] {
			return
		}
		visited[d] = true
		names[d.Name] = true
		for _, f := range d.Fields {
			walk(f.Type)
		}
		for _, r := range d.Refs {
			walk(r)
		}
	}
	walk(d)
	return names, nil
}
