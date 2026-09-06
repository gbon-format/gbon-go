package gbon_test

// Cycle-safe failure-message rendering shared by the test corpus. The
// codec round-trips cyclic graphs, so test values printed in failure
// paths can carry cycles; raw %#v/%+v of a cyclic value overflows the
// stack inside fmt instead of reporting. The helpers below render any
// value through a depth-capped structural walk with a pointer/map/slice
// seen-set, so every render terminates.

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	descMaxDepth = 6
	descMaxItems = 6
	descMaxRunes = 2048
)

// safeDescValue renders v for failure messages: any Go value, cycle-safe
// and bounded (depth, per-container item caps, total rune cap).
func safeDescValue(v any) string {
	s := safeDescReflect(reflect.ValueOf(v), 0, make(map[uintptr]bool))
	if len(s) > descMaxRunes {
		return s[:descMaxRunes] + "…"
	}
	return s
}

// safeDescReflect is the reflect-level form of safeDescValue: the walk
// carries its own depth counter and seen-set; callers pass fresh state
// at entry.
func safeDescReflect(v reflect.Value, depth int, seen map[uintptr]bool) string {
	if !v.IsValid() {
		return "<invalid>"
	}
	if depth > descMaxDepth {
		return "…"
	}
	switch v.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		if v.CanInterface() {
			return fmt.Sprint(v.Interface())
		}
		return v.Type().String()
	case reflect.String:
		s := v.String()
		if utf8.RuneCountInString(s) > 48 {
			return fmt.Sprintf("%q…", s[:48])
		}
		return fmt.Sprintf("%q", s)
	case reflect.Pointer:
		if v.IsNil() {
			return "nil"
		}
		p := v.Pointer()
		if seen[p] {
			return "<cycle>"
		}
		seen[p] = true
		defer delete(seen, p)
		return "*" + safeDescReflect(v.Elem(), depth+1, seen)
	case reflect.Map:
		if v.IsNil() {
			return "nilmap"
		}
		p := v.Pointer()
		if seen[p] {
			return "<cycle>"
		}
		seen[p] = true
		defer delete(seen, p)
		var parts []string
		it := v.MapRange()
		for i := 0; it.Next() && i < descMaxItems; i++ {
			parts = append(parts, safeDescReflect(it.Key(), depth+1, seen)+":"+safeDescReflect(it.Value(), depth+1, seen))
		}
		return fmt.Sprintf("map{%s}(%d)", strings.Join(parts, ","), v.Len())
	case reflect.Slice:
		if v.IsNil() {
			return "nilslice"
		}
		if v.Type().Elem().Kind() == reflect.Uint8 && v.CanInterface() {
			b := v.Bytes()
			if len(b) > 32 {
				return fmt.Sprintf("%x…(%dB)", b[:32], len(b))
			}
			return fmt.Sprintf("%x", b)
		}
		p := v.Pointer()
		cyc := false
		if seen[p] {
			cyc = true
		} else if v.Len() > 0 {
			seen[p] = true
			defer delete(seen, p)
		}
		if cyc {
			return fmt.Sprintf("sl(len=%d,cap=%d)<cycle>", v.Len(), v.Cap())
		}
		var parts []string
		for i := 0; i < v.Len() && i < descMaxItems; i++ {
			parts = append(parts, safeDescReflect(v.Index(i), depth+1, seen))
		}
		return fmt.Sprintf("sl(len=%d,cap=%d){%s}", v.Len(), v.Cap(), strings.Join(parts, ","))
	case reflect.Array:
		var parts []string
		for i := 0; i < v.Len() && i < descMaxItems; i++ {
			parts = append(parts, safeDescReflect(v.Index(i), depth+1, seen))
		}
		return fmt.Sprintf("[%d]%s{%s}", v.Len(), v.Type().Elem(), strings.Join(parts, ","))
	case reflect.Struct:
		var parts []string
		for i := 0; i < v.NumField() && i < descMaxItems; i++ {
			f := v.Type().Field(i)
			if v.Field(i).CanInterface() {
				parts = append(parts, f.Name+"="+safeDescReflect(v.Field(i), depth+1, seen))
			} else {
				parts = append(parts, f.Name+"=<unexported>")
			}
		}
		return v.Type().Name() + "{" + strings.Join(parts, ",") + "}"
	case reflect.Interface:
		if v.IsNil() {
			return "nil"
		}
		return safeDescReflect(v.Elem(), depth+1, seen)
	default:
		return fmt.Sprintf("%v", v.Type())
	}
}
