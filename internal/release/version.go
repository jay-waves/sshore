package release

import (
	"cmp"
	"fmt"
	"strings"
)

const InstallHelp = "Install the same full release on both machines.\n" +
	"Linux installer: https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh\n" +
	"Windows installer: https://raw.githubusercontent.com/jay-waves/sshore/main/install.ps1"

// Check requires identical, canonical release versions.
func Check(local, peer, role string) error {
	if local == peer {
		if _, ok := releaseParts(local); ok {
			return nil
		}
	}
	if peer == "" {
		peer = "unknown"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%q.\n%s", local, role, peer, InstallHelp)
}

// Return the canonical components for exact matching and upgrade ordering.
func releaseParts(v string) ([3]string, bool) {
	major, rest, hasMinor := strings.Cut(v, ".")
	minor, patch, hasPatch := strings.Cut(rest, ".")
	if !hasMinor || !hasPatch {
		return [3]string{}, false
	}
	parts := [3]string{major, minor, patch}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return [3]string{}, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return [3]string{}, false
			}
		}
	}
	return parts, true
}

// Compare numeric release components without integer overflow. Custom labels
// cannot be ordered and must be handled manually when they differ.
func Compare(a, b string) (int, bool) {
	aa, ok := releaseParts(a)
	if !ok {
		return 0, false
	}
	bb, ok := releaseParts(b)
	if !ok {
		return 0, false
	}
	for i := range aa {
		if order := cmp.Or(cmp.Compare(len(aa[i]), len(bb[i])), strings.Compare(aa[i], bb[i])); order != 0 {
			return order, true
		}
	}
	return 0, true
}
