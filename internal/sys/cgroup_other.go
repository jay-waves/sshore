//go:build !linux

package sys

func DaemonTaskCgroup() (string, error) { return "", nil }

func ProbeShellCgroup() error { return nil }
