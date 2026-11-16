package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestCompletionZshReadline(t *testing.T) {
	shell, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh unavailable")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := strings.ReplaceAll(completionScripts["zsh"], "command sshore __complete", shellQuote(binary)+" '-test.run=^TestCompletionQueryProcess$' --")
	scriptPath := filepath.Join(dir, "completion.zsh")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "afile space"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "adir space"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, "-f")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "ZDOTDIR="+dir, "TERM=xterm", "SSHORE_COMPLETION_TEST=1", "SSHORE_DISABLED=", "SSHORE_STATE_DIR="+filepath.Join(dir, "state"))
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { io.Copy(io.Discard, terminal); close(drained) }()
	defer func() { cmd.Process.Kill(); cmd.Wait(); terminal.Close(); <-drained }()
	setup := `autoload -Uz compinit; compinit -D -i
source ` + shellQuote(scriptPath) + `
_sshore_capture() { print -r -- "$BUFFER" >> captured; BUFFER=''; zle reset-prompt; }
zle -N _sshore_capture
bindkey '^X^D' _sshore_capture
print ready > ready
`
	if _, err := io.WriteString(terminal, setup); err != nil {
		t.Fatal(err)
	}
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("shell completion timeout")
	}
	wait(func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil })
	for i, tc := range []struct{ input, want string }{
		{"sshore att", "sshore attach"},
		{"sshore cp ./af", "./afile space"},
		{"sshore launch work --download-dir=./ad", "--download-dir=./adir space/"},
	} {
		if _, err := io.WriteString(terminal, tc.input+"\t\x18\x04"); err != nil {
			t.Fatal(err)
		}
		var lines []string
		wait(func() bool {
			data, _ := os.ReadFile(filepath.Join(dir, "captured"))
			lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			return len(lines) > i && lines[i] != ""
		})
		if !strings.Contains(strings.ReplaceAll(lines[i], `\ `, " "), tc.want) {
			t.Fatalf("%q -> %q; want %q", tc.input, lines[i], tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("completion created state", err)
	}
}
