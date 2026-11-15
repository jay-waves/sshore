//go:build windows && sshoretest

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shellmux "github.com/jay-waves/sshore/internal/mux"
)

// The supplied executable must be built with sshoretest, like this test binary.
func TestWindowsLocalDetachKeepsShell(t *testing.T) {
	bin := os.Getenv("SSHORE_TEST_BINARY")
	if bin == "" {
		t.Skip("SSHORE_TEST_BINARY must name the sshoretest executable")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", t.TempDir())
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_DISABLED", "")
	t.Cleanup(func() {
		// Verify idle shutdown and release Windows file handles before TempDir cleanup.
		path, err := daemonPath()
		if err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("local daemon did not exit after its tasks ended")
	})
	const name = "nativecheck"
	defer func() { _, _ = localRequest(name, message{Type: "cancel"}) }()
	outer, err := shellmux.Start(shellmux.Config{Path: "cmd.exe", Args: []string{"/d", "/q"}, Env: os.Environ(), Size: shellmux.Size{Cols: 160, Rows: 40}})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	chunks := make(chan string, 64)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(chunks)
		b := make([]byte, 4096)
		for {
			n, err := outer.Read(b)
			if n > 0 {
				select {
				case chunks <- string(b[:n]):
				case <-stop:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var transcript strings.Builder
	pending := ""
	waitFor := func(marker string) {
		t.Helper()
		timeout := time.NewTimer(15 * time.Second)
		defer timeout.Stop()
		for {
			if i := strings.Index(pending, marker); i >= 0 {
				pending = pending[i+len(marker):]
				return
			}
			select {
			case text, ok := <-chunks:
				if !ok {
					t.Fatalf("outer console ended while waiting for %q\n%s", marker, transcript.String())
				}
				transcript.WriteString(text)
				pending += text
			case <-timeout.C:
				t.Fatalf("timed out waiting for %q\n%s", marker, transcript.String())
			}
		}
	}
	send := func(text string) {
		t.Helper()
		if _, err := io.WriteString(outer, text); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(">")
	send(fmt.Sprintf("\"%s\" shell --name %s & echo SSHORE_OUTER_AFTER_DETACH\r\n", bin, name))
	waitFor("Connected local / " + name)
	send("prompt SSHORE_INNER_PROMPT$G\r\n")
	waitFor("SSHORE_INNER_PROMPT>")
	send("\x02d")
	waitFor("Detached;")
	waitFor("SSHORE_OUTER_AFTER_DETACH")
	before, err := localRequest(name, message{Type: "status"})
	if err != nil || before.PID == 0 || before.Attached {
		t.Fatalf("shell did not survive detach: %+v, %v", before, err)
	}
	t.Logf("detach returned to outer cmd; background shell PID %d is alive", before.PID)
	first, err := readLocalSession(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = localRequest("nativesecond", message{Type: "cancel"}) }()
	send(fmt.Sprintf("\"%s\" shell --name nativesecond & echo SSHORE_SECOND_DETACHED\r\n", bin))
	waitFor("Connected local / nativesecond")
	send("\x02d")
	waitFor("SSHORE_SECOND_DETACHED")
	second, err := readLocalSession("nativesecond")
	if err != nil {
		t.Fatal(err)
	}
	if first.ManagerPID <= 0 || first.ManagerPID != second.ManagerPID {
		t.Fatalf("different daemon PIDs: %d, %d", first.ManagerPID, second.ManagerPID)
	}
	t.Logf("both shells share daemon PID %d", first.ManagerPID)
	send(fmt.Sprintf("\"%s\" attach --local %s & echo SSHORE_OUTER_AFTER_EXIT\r\n", bin, name))
	waitFor("Connected local / " + name)
	waitFor("SSHORE_INNER_PROMPT>")
	t.Log("reattach repainted the prompt without sending input")
	after, err := localRequest(name, message{Type: "status"})
	if err != nil || after.PID != before.PID || !after.Attached {
		t.Fatalf("reattach changed shell: %+v, %v", after, err)
	}
	send("exit\r\n")
	waitFor("SSHORE_OUTER_AFTER_EXIT")
	t.Log("reattached to the same shell; exit returned to outer cmd")
}
