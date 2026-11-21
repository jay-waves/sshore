package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/pty"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestWindowsHandoffPassword(t *testing.T) {
	for _, phase := range []string{"submit", "cancel-prompt", "cancel-handshake"} {
		t.Run(phase, func(t *testing.T) {
			testWindowsHandoffPassword(t, phase)
		})
	}
}

func testWindowsHandoffPassword(t *testing.T, phase string) {
	bin := testBinary(t)
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Niubash bash executable unavailable")
	}
	dir := t.TempDir()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := cryptossh.NewSignerFromKey(key)
	received := make(chan string, 5)
	release := make(chan struct{})
	defer close(release)
	config := &cryptossh.ServerConfig{PasswordCallback: func(_ cryptossh.ConnMetadata, password []byte) (*cryptossh.Permissions, error) {
		received <- string(password)
		if phase == "cancel-handshake" {
			<-release
		}
		return nil, fmt.Errorf("fixture authentication stopped")
	}}
	config.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		raw, e := listener.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(20 * time.Second))
		cryptossh.NewServerConn(raw, config)
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	known := filepath.Join(dir, "known_hosts")
	os.WriteFile(known, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, cryptossh.MarshalAuthorizedKey(host.PublicKey()))), 0600)
	cfg := filepath.Join(dir, "config")
	prompts := 1
	if phase == "cancel-prompt" {
		prompts = 3
	}
	os.WriteFile(cfg, []byte(fmt.Sprintf("Host fixture\n HostName 127.0.0.1\n Port %d\n User fixture\n UserKnownHostsFile %s\n StrictHostKeyChecking yes\n NumberOfPasswordPrompts %d\n", port, filepath.ToSlash(known), prompts)), 0600)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("SHELL", shell)
	env := os.Environ()
	terminal, err := pty.Start(pty.Config{Path: bin, Args: []string{"launch", "here:"}, Env: env, Size: pty.Size{Cols: 120, Rows: 40}})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer func() { cmd := exec.Command(bin, "kill", "--all"); cmd.Env = env; cmd.Run() }()
	chunks := make(chan string, 128)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(chunks)
		b := make([]byte, 4096)
		for {
			n, e := terminal.Read(b)
			if n > 0 {
				select {
				case chunks <- string(b[:n]):
				case <-stop:
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	var output strings.Builder
	wait := func(marker string) {
		t.Helper()
		timeout := time.After(15 * time.Second)
		for !strings.Contains(output.String(), marker) {
			select {
			case text, ok := <-chunks:
				if !ok {
					t.Fatalf("console closed while waiting for %s: %s", marker, output.String())
				}
				output.WriteString(text)
			case <-timeout:
				t.Fatalf("waiting for %s: %s", marker, output.String())
			}
		}
	}
	wait("Connected here:")
	// Wait for the child shell to begin accepting commands.
	terminal.Write([]byte("echo SHELL_READY\r"))
	wait("SHELL_READY")
	time.Sleep(200 * time.Millisecond)
	command := fmt.Sprintf("'%s' spawn fixture: -- -F '%s'\r", filepath.ToSlash(bin), filepath.ToSlash(cfg))
	terminal.Write([]byte(command))
	wait("fixture@127.0.0.1's password:")
	cancelAndReturn := func() {
		t.Helper()
		output.Reset()
		if _, err := terminal.Write([]byte{3}); err != nil {
			t.Fatal(err)
		}
		wait("Target unavailable; restoring")
		wait("Connected here:")
	}
	if phase == "cancel-prompt" {
		if _, err := terminal.Write([]byte("partial-password")); err != nil {
			t.Fatal(err)
		}
		cancelAndReturn()
		select {
		case <-received:
			t.Fatal("server received a password after prompt cancellation")
		default:
		}
		return
	}
	for _, b := range []byte("test-Password!42\r") {
		terminal.Write([]byte{b})
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case password := <-received:
		if password != "test-Password!42" {
			t.Fatalf("handoff changed synthetic password: got %q", password)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server received no password")
	}
	if phase == "cancel-handshake" {
		// The password reader has restored cooked mode, but SSH has not finished.
		// ConPTY delivers a console interrupt here, not a remote Ctrl+C byte.
		cancelAndReturn()
	}
}
