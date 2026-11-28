//go:build integration

package tests_test

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testWindowsOriginApproval(t *testing.T, host string, command func(...string) *exec.Cmd, run func(*testing.T, string, func(func(string), terminalWait))) {
	t.Helper()
	endpoint := host + ":ci-origin"
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.txt"), filepath.Join(dir, "second.txt")
	service, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	reserved, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	forwardPort := reserved.Addr().(*net.TCPAddr).Port
	reserved.Close()

	var forwardID string
	defer func() {
		if forwardID != "" {
			windowsRun(t, command("kill", forwardID))
		}
	}()

	run(t, endpoint, func(send func(string), wait terminalWait) {
		wait("Connected " + endpoint)
		send("mkdir ci-origin-first ci-origin-second ci-origin-upload\r")
		send("echo first>ci-origin-first\\source.txt\r")
		send("echo second>ci-origin-second\\source.txt\r")
		// Consent belongs to the origin terminal. Declining must leave no local file.
		send("sshore cp :ci-origin-first\\source.txt ::\"" + first + "\"\r")
		wait("Approve this request? [y/N]", true)
		send("\r")
		wait("approval cancelled")
		wait("C:\\Users\\sshore-fixture>", true)
		if _, err := os.Stat(first); !os.IsNotExist(err) {
			t.Fatalf("declined copy destination: %v", err)
		}

		// A multi-file approval gives each destination its own review.
		send("sshore cp :ci-origin-first\\source.txt :ci-origin-second\\source.txt ::\"" + first + "\"\r")
		wait("Approve this request? [y/N]", true)
		send("y\r")
		wait("Save as:", true)
		send("\r")
		wait("Save as:", true)
		send("\x15" + second + "\r")
		wait("Transferred 2/2 files.")
		wait("C:\\Users\\sshore-fixture>", true)

		send("sshore cp ::\"" + first + "\" :ci-origin-upload\r")
		wait("Approve this request? [y/N]", true)
		send("y\r")
		wait("Local file to upload:", true)
		send("\r")
		wait("Uploaded ")
		wait("C:\\Users\\sshore-fixture>", true)
		send("set /p CI_UPLOADED=<ci-origin-upload\\first.txt\r")
		send("echo CI_UPLOADED=%CI_UPLOADED%\r")
		wait("CI_UPLOADED=first")

		send("sshore mount :ci-origin-first :: --read-only\r")
		wait("Approve this request? [y/N]", true)
		send("\r")
		wait("approval cancelled")
		wait("C:\\Users\\sshore-fixture>", true)

		send(fmt.Sprintf("sshore fwd :%d ::%d\r", service.Addr().(*net.TCPAddr).Port, forwardPort))
		wait("Local target port (Enter to approve, Ctrl-C to cancel):", true)
		send("\r")
		wait("Started forward/")
		task := strings.TrimSuffix(wait(":"), ":")
		forwardID = task
		wait("C:\\Users\\sshore-fixture>", true)
		send("exit\r")
	})

	for path, want := range map[string]string{first: "first", second: "second"} {
		data, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Fatalf("download %s: %q (%v)", path, data, err)
		}
	}
	client, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", forwardPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = service.SetDeadline(time.Now().Add(5 * time.Second))
	backend, err := service.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(backend, "approved forward"); err != nil {
		backend.Close()
		t.Fatal(err)
	}
	backend.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if data, err := io.ReadAll(client); err != nil || string(data) != "approved forward" {
		t.Fatalf("forward response: %q (%v)", data, err)
	}
}
