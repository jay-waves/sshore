package release

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func Valid(version string) bool {
	_, ok := parts(version)
	return ok
}

func parts(v string) ([3]string, bool) {
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

// Compare numeric components without overflowing on large release coordinates.
func Compare(a, b string) (int, bool) {
	aa, ok := parts(a)
	if !ok {
		return 0, false
	}
	bb, ok := parts(b)
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

func ExecutableVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	return parseExecutableVersion(out)
}

func parseExecutableVersion(out []byte) (string, error) {
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, "sshore ") || strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("unrecognized version response")
	}
	return strings.TrimPrefix(line, "sshore "), nil
}
