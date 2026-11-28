//go:build integration && linux

package tests_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run with a disposable account daemon. The background job starts a new
// POSIX session and relinquishes the PTY before the frontend detaches.
func TestShellCgroup(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_CGROUP")
	if binary == "" {
		t.Skip("set SSHORE_TEST_CGROUP to the fixture binary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := func(name string) (string, string) {
		t.Helper()
		marker := filepath.Join(t.TempDir(), "group")
		endpoint := ".:" + name
		cmd := exec.CommandContext(ctx, binary, "sh", "--name", name, "--pin", "--no-profile", "--no-login")
		runOpenSSHTerminal(t, ctx, cmd, func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			send(fmt.Sprintf("setsid sh -c 'cat /proc/self/cgroup > %q; exec sleep 60' </dev/null >/dev/null 2>&1 &\n", marker))
			send(fmt.Sprintf("while [ ! -s %q ]; do sleep 0.05; done; printf 'group_%%s\\n' ready\n", marker))
			wait("group_ready")
			send("\x02d")
			wait("No shell attached.")
			send("exit\r")
		})
		t.Cleanup(func() { _ = exec.Command(binary, "kill", name).Run() })
		data, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		path, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "0::/")
		if !ok || path == "" {
			t.Fatalf("no task cgroup in %q", data)
		}
		return name, filepath.Join("/sys/fs/cgroup", path)
	}
	target, first := start("cg-first")
	_, second := start("cg-second")
	if first == second {
		t.Fatal("different tasks share a cgroup")
	}
	if out, err := exec.CommandContext(ctx, binary, "kill", target).CombinedOutput(); err != nil {
		t.Fatalf("kill: %s (%v)", out, err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("stopped task cgroup still exists: %s (%v)", first, err)
	}
	if data, err := os.ReadFile(filepath.Join(second, "cgroup.events")); err != nil || !strings.Contains(string(data), "populated 1") {
		t.Fatalf("sibling cgroup: %s (%v)", data, err)
	}
}
