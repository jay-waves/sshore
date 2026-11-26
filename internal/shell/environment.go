package shell

import (
	"cmp"
	"runtime"
	"slices"
	"strings"
)

// EnvValue reads the last matching environment entry using the target platform's case rules.
func EnvValue(env []string, name string) string {
	for _, e := range slices.Backward(env) {
		key, value, ok := strings.Cut(e, "=")
		if ok && (key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
			return value
		}
	}
	return ""
}

// DefaultPath selects the account shell, with platform and hosting defaults.
func DefaultPath(env []string, hosted bool) string {
	shell := EnvValue(env, "SHELL")
	if runtime.GOOS == "windows" {
		return cmp.Or(shell, EnvValue(env, "COMSPEC"), "cmd.exe")
	}
	if hosted {
		return cmp.Or(shell, "/bin/bash")
	}
	return cmp.Or(shell, "/bin/sh")
}
