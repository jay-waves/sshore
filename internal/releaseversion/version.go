package releaseversion

import "strings"

// Parse stable release labels for upgrade direction; this is not a compatibility series.
func Series(v string) (string, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return "", false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", false
			}
		}
	}
	return parts[0] + "." + parts[1], true
}

func Compatible(local, peer string) bool {
	return local != "" && local == peer
}

// Compare numeric release components without integer overflow. Custom labels
// cannot be ordered and must be handled manually when they differ.
func Compare(a, b string) (int, bool) {
	if a == b && a != "" {
		return 0, true
	}
	if _, ok := Series(a); !ok {
		return 0, false
	}
	if _, ok := Series(b); !ok {
		return 0, false
	}
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		if len(aa[i]) < len(bb[i]) {
			return -1, true
		}
		if len(aa[i]) > len(bb[i]) {
			return 1, true
		}
		if aa[i] < bb[i] {
			return -1, true
		}
		if aa[i] > bb[i] {
			return 1, true
		}
	}
	return 0, true
}
