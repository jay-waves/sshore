package main

import (
	"fmt"
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
	// The helper retains the original task ID after rename.
	helper := exec.Command(e.bin, "detach")
	helper.Env = append(append([]string{}, e.env...), "SSHORE_SOCKET="+func() string {
		id, err := resolveSession("work")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}())
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

func TestAutomaticSessionSelection(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	id := strings.Repeat("a", 32)
	shell := &remoteShell{tasks: tasks, instance: id, name: "pikachu", done: make(chan struct{})}
	if err := tasks.register(id, shell); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveSession([]string{"pikachu"}, true); err == nil {
		t.Fatal("reused live name")
	}
	r, err := reserveSession([]string{"pikachu", "eevee"}, true)
	if err != nil || !validTaskUID(r.name) || r.conn != nil {
		t.Fatalf("selection: %+v %v", r, err)
	}
	existing, err := reserveSession([]string{"pikachu"}, false)
	if err != nil {
		t.Fatal(err)
	}
	existing.conn.Close()
	if existing.name != id {
		t.Fatal("existing selection lost identity")
	}
}

// Neither a hung daemon nor stale metadata should turn name lookup into a
// network health check. A live hung daemon must still reserve its public name.
func TestSessionNameResolutionUsesTaskTable(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	id := strings.Repeat("a", 32)
	shell := &remoteShell{tasks: tasks, instance: id, name: "work", done: make(chan struct{})}
	if err := tasks.register(id, shell); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSession("work"); err != nil || got != id {
		t.Fatalf("lookup: %s %v", got, err)
	}
	t.Setenv("SSHORE_RUNTIME_DIR", t.TempDir())
	if got, err := resolveSession("old-label", id); err != nil || got != id {
		t.Fatalf("account lookup: %s %v", got, err)
	}
	close(shell.done)
	if got, err := resolveSession("work"); err != nil || got != "" {
		t.Fatalf("ended lookup: %s %v", got, err)
	}
}

func TestShellTaskIDIndependentOfAlias(t *testing.T) {
	e := newEnv(t)
	alias := strings.Repeat("a", 32)
	b, pid := e.attach(alias)
	uid, err := resolveSession(alias)
	if err != nil || !validTaskUID(uid) || uid == alias {
		t.Fatalf("alias became identity: %q %v", uid, err)
	}
	detachTestClient(t, b)
	cmd := exec.Command(e.bin, "rename-session", uid[:6], "work", version, uid)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rename by immutable reference: %s %v", out, err)
	}
	b, resumedPID := e.attach(uid[:6], "attach")
	if resumedPID != pid {
		t.Fatal("alias change replaced the shell")
	}
	resolved, err := resolveSession("work")
	if err != nil || resolved != uid {
		t.Fatalf("alias changed identity: %q %v", resolved, err)
	}
	sendTest(t, b.tty, message{Type: "input", Data: []byte("exit\n")})
	untilType(t, b.tty, "exit")
	b.close()
}

// Uses the existing isolated -tags=sshoretest executable, never account sessions.
func TestKillSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	query := func(name string) message {
		t.Helper()
		c, err := e.dial(name)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		sendTest(t, c, message{Type: "status"})
		return recv(t, c)
	}
	run := func(name, release string) (string, error) {
		cmd := exec.Command(e.bin, "kill-session", name, release)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	keep, keepPID := e.attach("keep")
	keepProcess, err := readSetupTask(keepPID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := exec.Command(e.bin, "kill-session", "keep", version)
	blocked.Env = append(append([]string{}, e.env...), "SSHORE_DISABLED=1")
	if out, err := blocked.CombinedOutput(); err == nil || !strings.Contains(string(out), "disabled") {
		t.Fatal("recovery allowed kill", string(out), err)
	}
	listing := exec.Command(e.bin, "sessions")
	listing.Env = e.env
	if out, err := listing.CombinedOutput(); err != nil || !strings.Contains(string(out), "keep [attached] · started ") {
		t.Fatal("missing list age", string(out), err)
	}
	for _, detached := range []bool{false, true} {
		name := fmt.Sprintf("target-%t", detached)
		b, pid := e.attach(name)
		targetProcess, err := readSetupTask(pid)
		if err != nil || targetProcess.parent != keepProcess.parent {
			t.Fatalf("shells do not share a daemon: %+v %+v %v", keepProcess, targetProcess, err)
		}
		// A child that ignores TERM/HUP exercises the bounded escalation path.
		childFile := filepath.Join(e.dir, name+".child")
		shellOutput(t, b, "sh -c 'trap \"\" TERM HUP; echo $$ > "+shellQuote(childFile)+"; exec sleep 120' & printf 'CHILD_%s\\n' ready", "CHILD_ready")
		var child int
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(childFile)
			if _, err := fmt.Sscan(string(data), &child); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if child == 0 {
			t.Fatal("child did not start")
		}
		age := query(name).AgeSeconds
		if age == nil {
			t.Fatal("missing age")
		}
		if detached {
			sendTest(t, b.in, message{Type: "detach"})
			untilType(t, b.out, "detached")
			b.close()
			b, resumedPID := e.attach(name)
			if resumedPID != pid || query(name).AgeSeconds == nil || *query(name).AgeSeconds < *age {
				t.Fatal("age or identity reset")
			}
			sendTest(t, b.in, message{Type: "detach"})
			untilType(t, b.out, "detached")
			b.close()
		}
		if out, err := run(name, "999.0.0"); err == nil {
			t.Fatal("accepted mismatch", out)
		}
		if query(name).PID != pid {
			t.Fatal("mismatch ended shell")
		}
		if out, err := run(name, version); err != nil || !strings.Contains(out, "Ended shell "+name) {
			t.Fatal(out, err)
		}
		for _, taskPID := range []int{pid, child} {
			if task, err := readSetupTask(taskPID); err == nil && task.state != "Z" {
				t.Fatalf("task %d survived", taskPID)
			}
		}
		if out, err := run(name, version); err == nil {
			t.Fatal("missing target accepted", out)
		}
		shellOutput(t, keep, "printf 'STILL_%s\\n' alive", "STILL_alive")
	}
	if out, err := run("keep", version); err != nil {
		t.Fatal(out, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if process, err := readSetupTask(keepProcess.parent); err != nil || process.state == "Z" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shared remote daemon survived its last shell")

}

func TestSessionProcessTree(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"10": "10 (bash) S 1 10 10 1 20",
		"20": "20 (codex) S 10 20 10 1 20",
		"21": "21 (worker (job)) S 20 20 10 1 20",
		"30": "30 (sleep) S 10 30 10 1 20",
		"40": "40 (unrelated) S 1 40 40 1 40",
		"50": "50 (bad\x1bname) S 30 30 10 1 20",
		"60": "invalid",
	} {
		path := filepath.Join(root, pid)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got := sessionProcessTree(root, 10)
	expected := "bash (10)\n├─ codex (20) [fg]\n│  └─ worker (job) (21) [fg]\n└─ sleep (30)\n   └─ bad�name (50)\n"
	if got != expected {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, expected)
	}
	if got := sessionProcessTree(root, 99); !strings.Contains(got, "99 [process unavailable]") {
		t.Fatal(got)
	}
}

func TestForegroundSnapshot(t *testing.T) {
	root := t.TempDir()
	for pid, stat := range map[string]string{
		"20": "20 (grep) S 1 42 1",
		"3":  "3 (cat (worker)) S 1 42 1",
		"4":  "4 (background) S 1 99 1",
		"5":  "malformed",
		"6":  "6 (bad\nname) S 1 42 1",
	} {
		dir := filepath.Join(root, pid)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := foregroundPrograms(root, 42); got != "cat (worker) | bad�name | grep" {
		t.Fatal(got)
	}
	for _, group := range []int{0, 100} {
		if got := foregroundPrograms(root, group); got != "—" {
			t.Fatal(got)
		}
	}
	if got := foregroundPrograms(filepath.Join(root, "missing"), 42); got != "—" {
		t.Fatal(got)
	}
}
