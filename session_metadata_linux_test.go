package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every lifecycle process uses newEnv's sshoretest-tagged, isolated binary.
func TestSessionMetadataLifecycle(t *testing.T) {
	e := newEnv(t)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(e.bin, args...)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	b, pid := e.attach("original")
	shellOutput(t, b, "cd /tmp; printf 'DIRECTORY_%s\\n' ready", "DIRECTORY_ready")
	if out, err := run("rename-session", "original", "work", "999.0.0"); err == nil {
		t.Fatal("version mismatch accepted", out)
	}
	if out, err := run("rename-session", "original", "work", version); err != nil {
		t.Fatal(out, err)
	}
	if out, err := run("sessions"); err != nil || !strings.Contains(out, "work [attached]") || !strings.Contains(out, "sh · /tmp") || strings.Contains(out, "original [attached]") {
		t.Fatal(out, err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte("sleep 30\n")})
	foregroundSeen := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		out, err := run("sessions")
		if err != nil {
			t.Fatal(out, err)
		}
		if strings.Contains(out, "sleep · /tmp") {
			foregroundSeen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !foregroundSeen {
		t.Fatal("foreground process or directory missing")
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte{3}})
	shellOutput(t, b, "printf 'INTERRUPTED_%s\\n' ready", "INTERRUPTED_ready")
	// The helper still uses the original inherited socket after rename.
	helper := exec.Command(e.bin, "detach")
	helper.Env = append(append([]string{}, e.env...), "SSHORE_SOCKET="+filepath.Join(e.dir, "original.sock"))
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}

	if m := untilType(t, b.out, "detached"); m.Name != "work" {
		t.Fatal("stale detach name", m.Name)
	}
	b.close()
	b, resumed := e.attach("work", "attach")
	if resumed != pid {
		t.Fatal("rename replaced shell")
	}
	other, otherPID := e.attach("original", "new")
	if otherPID == pid {
		t.Fatal("old label not reusable")
	}
	if out, err := run("rename-session", "work", "original", version); err == nil {
		t.Fatal("collision accepted", out)
	}
	shellOutput(t, b, "printf 'RENAMED_%s\\n' alive", "RENAMED_alive")
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit 7\n")})
	untilType(t, b.tty, "exit")
	waitExitReceipt(t, e.dir, "work", "shell exited (code 7)")
	if out, err := run("kill-session", "original", version); err != nil {
		t.Fatal(out, err)
	}
	_ = other
	waitExitReceipt(t, e.dir, "original", "cancelled by sshore cancel")
	if out, err := run("sessions"); err != nil || !strings.Contains(out, "Recent exits:") || !strings.Contains(out, "work [ended]") {
		t.Fatal(out, err)
	}
}

func waitExitReceipt(t *testing.T, dir, name, reason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, record := range readSessionExits(dir) {
			if record.Name == name && record.Reason == reason {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("missing exit receipt %s / %s: %+v", name, reason, readSessionExits(dir))
}

func TestSessionExitReceiptBounds(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "test.sock")
	if err := os.Symlink(outside, socket+".exit.json"); err != nil {
		t.Fatal(err)
	}
	saveSessionExit(socket, "test", "shell exited (code 0)")
	if b, _ := os.ReadFile(outside); string(b) != "untouched" {
		t.Fatal("followed symlink")
	}
	if info, err := os.Stat(socket + ".exit.json"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for i := 0; i < 102; i++ {
		// Distinct names are not necessary: receipts are keyed by identity.
		saveSessionExit(filepath.Join(dir, time.Unix(int64(i), 0).Format("150405")+".sock"), "test", "ended")
	}
	if got := len(readSessionExits(dir)); got != 100 {
		t.Fatal("unbounded receipts", got)
	}
}

func TestAutomaticSessionReservation(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := os.MkdirTemp("", "ss-names-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "pikachu.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	lifetime, err := openLock(filepath.Join(dir, "pikachu.sock.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer lifetime.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if m, err := readMessage(c); err == nil && m.Type == "status" {
					_ = writeMessage(c, message{Type: "session", Name: "pikachu"})
				}
			}()
		}
	}()
	// Occupied names include detached daemons: allocation does not depend on
	// whether a terminal client is attached to this listening socket.
	if _, err := reserveSession([]string{"pikachu"}, true); err == nil || !strings.Contains(err.Error(), "all automatic shell names") {
		t.Fatalf("expected exhaustion, got %v", err)
	}
	r, err := reserveSession([]string{"pikachu", "eevee"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.name != "eevee" || r.conn != nil {
		t.Fatalf("allocated %q, connection %v", r.name, r.conn)
	}
	// Explicit names may still connect to an existing daemon.
	explicit, err := reserveSession([]string{"pikachu"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer explicit.Close()
	defer explicit.conn.Close()
	if explicit.name != "pikachu" {
		t.Fatal(explicit.name)
	}
	listener.Close()
	lifetime.Close()
	// A leftover startup lock does not permanently consume the name.
	explicit.Close()
	reused, err := reserveSession([]string{"pikachu"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	if reused.name != "pikachu" || reused.conn != nil {
		t.Fatalf("reuse: %q %v", reused.name, reused.conn)
	}
}

// Neither a hung daemon nor stale metadata should turn name lookup into a
// network health check. A live hung daemon must still reserve its public name.
func TestSessionNameResolutionWithoutStatus(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := os.MkdirTemp("", "ss-label-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("SSHORE_RUNTIME_DIR", dir)
	path := filepath.Join(dir, "internal.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() // Deliberately never accepts or answers status.
	lock, err := openLock(path+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeSessionName(path, "work"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSession("work"); err != nil || got != path {
		t.Fatalf("%q: %v", got, err)
	}
	if got, err := resolveSession("other"); err != nil || got != "" {
		t.Fatalf("%q: %v", got, err)
	}
	if _, err := reserveSession([]string{"work"}, true); err == nil {
		t.Fatal("reallocated live label")
	}
	// Losing the socket must not release a still-running daemon's label.
	listener.Close()
	if got, err := resolveSession("work"); err != nil || got != path {
		t.Fatalf("live identity without socket: %q: %v", got, err)
	}
	if _, err := reserveSession([]string{"work"}, false); err == nil {
		t.Fatal("unavailable live identity treated as a new shell")
	}
	lock.Close()
	if got, err := resolveSession("work"); err != nil || got != "" {
		t.Fatalf("stale label: %q: %v", got, err)
	}
}
