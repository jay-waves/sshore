package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runBootstrap(t *testing.T, home string, o options) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", remoteCommand(o))
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func TestRemoteMissingOnlyPrintsInstaller(t *testing.T) {
	home := t.TempDir()
	out, err := runBootstrap(t, home, options{server: filepath.Join(home, "missing"), session: "main"})
	if err == nil {
		t.Fatal("missing server succeeded")
	}
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 127 {
		t.Fatal(err)
	}
	for _, want := range []string{installerURL, "--version '" + version + "'", "REMOTE Linux", "no installation", "Adjust --server"} {
		if !strings.Contains(out, want) {
			t.Fatal("missing hint", want, out)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("probe wrote files", entries)
	}
}
func TestRemoteFallbackAndArgumentQuoting(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "sshore")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '<%s>\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := runBootstrap(t, home, options{server: "sshore", session: "main"})
	if err != nil || out != "<bridge>\n<main>\n" {
		t.Fatal(out, err)
	}
	out, err = runBootstrap(t, home, options{server: "sshore", session: "main", approved: []operation{{Kind: "forward", Port: 80}}})
	if err != nil || out != "<resume>\n<main>\n" {
		t.Fatal(out, err)
	}
	out, err = runBootstrap(t, home, options{server: "sshore", list: true})
	if err != nil || out != "<sessions>\n" {
		t.Fatal(out, err)
	}
	// Explicit paths are literal data and never fall back to an unrelated binary.
	odd := filepath.Join(home, "server ' $(touch INJECTED)")
	if err := os.Rename(bin, odd); err != nil {
		t.Fatal(err)
	}
	out, err = runBootstrap(t, home, options{server: odd, session: "main"})
	if err != nil || out != "<bridge>\n<main>\n" {
		t.Fatal(out, err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho unexpected-fallback\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err = runBootstrap(t, home, options{server: odd + "-missing", session: "main"})
	if err == nil || !strings.Contains(out, "no installation") {
		t.Fatal(out, err)
	}
	if _, err = os.Stat("INJECTED"); !os.IsNotExist(err) {
		t.Fatal("shell interpolation occurred")
	}
}

func TestRemoteExistingFailureDoesNotSuggestInstallation(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho original-error >&2\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := runBootstrap(t, home, options{server: bin, session: "main"})
	e, ok := err.(*exec.ExitError)
	if !ok || e.ExitCode() != 23 || out != "original-error\n" {
		t.Fatal(out, err)
	}
}
