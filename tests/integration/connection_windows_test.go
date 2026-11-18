package tests_test

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

	"github.com/jay-waves/sshore/internal/tmux"
	cryptossh "golang.org/x/crypto/ssh"
)

func TestWindowsHandoffPassword(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupt=%t", interrupt), func(t *testing.T) {
			testWindowsHandoffPassword(t, interrupt)
		})
	}
}

func testWindowsHandoffPassword(t *testing.T, interrupt bool) {
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
		if interrupt {
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
	os.WriteFile(cfg, []byte(fmt.Sprintf("Host fixture\n HostName 127.0.0.1\n Port %d\n User fixture\n IdentityAgent none\n UserKnownHostsFile %s\n StrictHostKeyChecking yes\n NumberOfPasswordPrompts 1\n", port, filepath.ToSlash(known))), 0600)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	t.Setenv("SSHORE_SOCKET", "")
	t.Setenv("SHELL", shell)
	env := os.Environ()
	terminal, err := tmux.Start(tmux.Config{Path: bin, Args: []string{"launch", "--local", "pw" + filepath.Base(dir)}, Env: env, Size: tmux.Size{Cols: 120, Rows: 40}})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer func() { cmd := exec.Command(bin, "kill", "--all"); cmd.Env = env; cmd.Run() }()
	chunks := make(chan string, 128)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
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
			case text := <-chunks:
				output.WriteString(text)
			case <-timeout:
				t.Fatalf("waiting for %s: %s", marker, output.String())
			}
		}
	}
	wait("Connected self:pw" + filepath.Base(dir))
	// Wait for the child shell to begin accepting commands.
	terminal.Write([]byte("echo SHELL_READY\r"))
	wait("SHELL_READY")
	time.Sleep(200 * time.Millisecond)
	command := fmt.Sprintf("'%s' spawn fixture: -- -F '%s'\r", filepath.ToSlash(bin), filepath.ToSlash(cfg))
	terminal.Write([]byte(command))
	wait("fixture@127.0.0.1's password:")
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
	if interrupt {
		// The password reader has restored cooked mode, but SSH has not finished.
		// ConPTY delivers a console interrupt here, not a remote Ctrl+C byte.
		output.Reset()
		if _, err := terminal.Write([]byte{3}); err != nil {
			t.Fatal(err)
		}
		wait("Target unavailable; restoring")
		wait("Connected self:pw" + filepath.Base(dir))
	}
}
