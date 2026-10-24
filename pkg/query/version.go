package query

import (
	"regexp"
	"strconv"
	"strings"
)

// version is the numeric parts of a dotted version: 1.22.3 is {1, 22, 3}.
type version []int

// compare returns -1, 0 or 1; a missing part counts as 0.
func (v version) compare(o version) int {
	for i := range max(len(v), len(o)) {
		var a, b int
		if i < len(v) {
			a = v[i]
		}
		if i < len(o) {
			b = o[i]
		}
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (v version) String() string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// afterOperator joins an operator to the version after it (">= 3.8").
var afterOperator = regexp.MustCompile(`([<>=!~^]+)\s+`)

// lowerBound parses a declared version into the lowest version it allows:
// "1.22.3", "v20.1.0", ">=18", "^20", "~=3.11", ">=1.2 <2" and "18 || 20" give
// 1.22.3, 20.1.0, 18, 20, 3.11, 1.2 and 18. ok is false for anything else,
// such as "lts/hydrogen", "pypy3.10-7.3" or "*", which are never guessed.
// shortcut: ">" is read as ">="; fine for "below X" unless an exact boundary
// matters.
func lowerBound(s string) (v version, ok bool) {
	var best version
	for alt := range strings.SplitSeq(s, "||") {
		low, ok := rangeLowerBound(alt)
		if !ok {
			return nil, false
		}
		if best == nil || low.compare(best) < 0 {
			best = low
		}
	}
	return best, best != nil
}

// rangeLowerBound is lowerBound for one alternative: comparators joined by
// spaces or commas. Its lower bound is the highest of the lower bounds
// its comparators set; upper bounds are ignored, but a range with no lower
// bound is unknown.
func rangeLowerBound(s string) (version, bool) {
	var best version
	for tok := range strings.FieldsSeq(strings.ReplaceAll(afterOperator.ReplaceAllString(s, "$1"), ",", " ")) {
		op, rest := splitOperator(tok)
		switch op {
		case "<", "<=", "!=": // no lower bound
			continue
		case "", ">", ">=", "=", "==", "^", "~", "~=":
		default:
			return nil, false
		}
		v, ok := parseDotted(rest)
		if !ok {
			return nil, false
		}
		if best == nil || v.compare(best) > 0 {
			best = v
		}
	}
	return best, best != nil
}

func splitOperator(tok string) (op, rest string) {
	i := strings.IndexFunc(tok, func(r rune) bool { return !strings.ContainsRune("<>=!~^", r) })
	if i < 0 {
		return tok, ""
	}
	return tok[:i], tok[i:]
}

// parseDotted reads "v1.22.3", "20.x" or "3.11-rc1+b2": numeric parts, a
// wildcard part ending the version, anything after - or + ignored.
func parseDotted(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	core, _, _ := strings.Cut(s, "+")
	core, _, _ = strings.Cut(core, "-")
	var v version
	for part := range strings.SplitSeq(core, ".") {
		if part == "x" || part == "X" || part == "*" {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		v = append(v, n)
	}
	return v, len(v) > 0
}
