package cahgen

import (
	"regexp"
	"strconv"
	"strings"
)

var committedVersionRE = regexp.MustCompile(`(?m)^const cahVersion = "([^"]+)"$`)

// committedVersion extracts cahVersion from a generated file; "" means unknown.
func committedVersion(generated []byte) string {
	if m := committedVersionRE.FindSubmatch(generated); m != nil {
		return string(m[1])
	}
	return ""
}

// semverLess reports whether a is strictly lower than b. Versions that are not
// major.minor.patch[-prerelease] never compare as lower, so the guard stays off.
func semverLess(a, b string) bool {
	ac, ap, ok := splitSemver(a)
	if !ok {
		return false
	}
	bc, bp, ok := splitSemver(b)
	if !ok {
		return false
	}
	for i := range ac {
		if ac[i] != bc[i] {
			return ac[i] < bc[i]
		}
	}
	switch {
	case ap == bp:
		return false
	case ap == "":
		return false // release is higher than any prerelease
	case bp == "":
		return true
	}
	return prereleaseLess(ap, bp)
}

func splitSemver(v string) (core [3]int, pre string, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, pre, hasPre := strings.Cut(v, "-")
	if hasPre && pre == "" {
		return core, "", false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return core, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return core, "", false
		}
		core[i] = n
	}
	return core, pre, true
}

func prereleaseLess(a, b string) bool {
	x, y := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		xn, xe := strconv.Atoi(x[i])
		yn, ye := strconv.Atoi(y[i])
		switch {
		case xe == nil && ye == nil:
			return xn < yn
		case xe == nil:
			return true // numeric identifiers sort before alphanumeric
		case ye == nil:
			return false
		}
		return x[i] < y[i]
	}
	return len(x) < len(y)
}
