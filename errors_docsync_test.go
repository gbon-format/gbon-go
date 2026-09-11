package gbon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorCoreSmoke(t *testing.T) {
	sentinels := map[error]bool{ErrFormat: true, ErrBudget: true, ErrUnsupported: true, ErrIO: true, ErrInternal: true}
	for class, sentinel := range classSentinels {
		if !sentinels[sentinel] {
			t.Fatalf("class %q maps to a non-sentinel", class)
		}
		for other := range sentinels {
			want := other == sentinel
			e := &Error{class: class}
			if got := errors.Is(e, other); got != want {
				t.Errorf("class %q: Is(%v) = %v, want %v", class, other, got, want)
			}
		}
	}

	marker := errors.New("marker")
	cases := []*Error{
		errFormat(classOverflowValue, 42, `$f["k"][0]`, int64(999), "int8", nil),
		errBudget(classBudgetDepth, -1, "$.deep", 10001, 10000, nil),
		errUnsupported(classUnsupportedKind, `$x.chan`, "chan", "serializable", nil),
		errIO(7, marker),
		errRegister(`wire name "x" already registered`),
		errCoder(classCoderRecursion, 9, "$.t", marker),
	}
	for _, e := range cases {
		s := e.Error()
		if strings.Contains(s, "\n") {
			t.Errorf("%%v not one line: %q", s)
		}
		if !strings.HasPrefix(s, "gbon: ") || !strings.Contains(s, e.Class()) {
			t.Errorf("%%v lacks the uniform prefix/class: %q", s)
		}
		verbose := fmt.Sprintf("%+v", e)
		if !strings.Contains(verbose, "\n    class = ") {
			t.Errorf("%%+v lacks class field: %q", verbose)
		}
	}
	ioErr := cases[3]
	if !errors.Is(ioErr, ErrIO) || errors.Is(ioErr, ErrFormat) {
		t.Errorf("io class sentinel mapping wrong")
	}
	if ioErr.Unwrap() != marker {
		t.Errorf("io cause not preserved through Unwrap")
	}
	if got := cases[4].Error(); !strings.Contains(got, `wire name "x" already registered`) {
		t.Errorf("register detail phrase lost: %q", got)
	}

	oneLine := errFormat(classTruncated, 3, "$", nil, nil, errors.New("end of input")).Error()
	wantLine := "gbon: truncated at $ (offset 3): end of input"
	if oneLine != wantLine {
		t.Errorf("%%v = %q, want %q", oneLine, wantLine)
	}
	verbose := fmt.Sprintf("%+v", errBudget(classBudgetBytes, 5, `$m["k"]`, "3 bytes", 1<<20, nil))
	for _, part := range []string{"class = budget_bytes", "offset = 5", `path = $m["k"]`, "got", "want"} {
		if !strings.Contains(verbose, part) {
			t.Errorf("%%+v missing %q: %q", part, verbose)
		}
	}

	k16 := strings.Repeat("k", 16)
	if got := boundedKey(k16); got != `["`+k16+`"]` {
		t.Errorf("boundedKey(16 printable) = %q", got)
	}
	if got := boundedKey(strings.Repeat("k", 17)); got != "[<key 17 B>]" {
		t.Errorf("boundedKey(17 printable) = %q", got)
	}
	if got := boundedKey("a\x00b"); got != "[<key 3 B>]" {
		t.Errorf("boundedKey(non-printable) = %q", got)
	}
	if got := boundedKey(`a"b\c`); got != `["a\"b\\c"]` {
		t.Errorf("boundedKey(quotes) = %q", got)
	}
	long := renderValue(strings.Repeat("z", 80))
	if n := len([]rune(long)); n > maxDetailRunes+1 {
		t.Errorf("renderValue(string) not bounded: %d runes", n)
	}
	if got := renderValue([]byte{1, 2, 3}); got != "[<bytes 3 B>]" {
		t.Errorf("renderValue(bytes) = %q", got)
	}
}
