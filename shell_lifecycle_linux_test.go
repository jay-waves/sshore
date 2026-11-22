package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/proc"
)

// Every lifecycle process uses newEnv's isolated CLI test executable.
func TestSessionMetadataLifecycle(t *testing.T) {
	e := newEnv(t)
	e.env = append(e.env, "SSHORE_RUNTIME_DIR=unusable-relative-runtime")
	run := func(req remoteServiceRequest) (string, error) {
		cmd := exec.Command(e.bin, remoteServiceArgs(req)...)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	b, pid := e.attach("original")
	shellOutput(t, b, "cd /tmp; printf 'DIRECTORY_%s\\n' ready", "DIRECTORY_ready")
	if out, err := run(remoteServiceRequest{Operation: "rename-session", Name: "original", NewName: "work", Release: "999.0.0"}); err == nil {
		t.Fatal("version mismatch accepted", out)
	}
	if out, err := run(remoteServiceRequest{Operation: "rename-session", Name: "original", NewName: "work"}); err != nil {
		t.Fatal(out, err)
	}
	if out, err := run(remoteServiceRequest{Operation: "inventory"}); err != nil || !strings.Contains(out, `"name":"work","state":"active"`) || !strings.Contains(out, `"directory":"/tmp"`) || strings.Contains(out, `"name":"original","state":"active"`) {
		t.Fatal(out, err)
	}
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
	if out, err := run(remoteServiceRequest{Operation: "rename-session", Name: "work", NewName: "original"}); err == nil {
		t.Fatal("collision accepted", out)
	}
	shellOutput(t, b, "printf 'RENAMED_%s\\n' alive", "RENAMED_alive")
	b.writeInput(t, []byte("exit 7\n"))
	b.readExit(t)
	if out, err := run(remoteServiceRequest{Operation: "kill-session", Name: "original"}); err != nil {
		t.Fatal(out, err)
	}
	_ = other
}

func TestAutomaticSessionSelection(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	id := strings.Repeat("a", 32)
	shell := &remoteShell{shellTask: shellTask{hosted: true, tasks: tasks, record: localShellRecord{UID: id, Name: "pikachu"}, done: make(chan struct{})}}
	if err := tasks.register(id, shell); err != nil {
		t.Fatal(err)
	}
	if _, err := selectHostedShell("pikachu", "new"); err == nil {
		t.Fatal("reused live name")
	}
	r, err := selectHostedShell("", "new")
	if err != nil || !validTaskUID(r.uid) || r.existing || !strings.HasPrefix(r.uid, r.name) {
		t.Fatalf("selection: %+v %v", r, err)
	}
	existing, err := selectHostedShell("pikachu", "attach")
	if err != nil {
		t.Fatal(err)
	}
	if !existing.existing || existing.uid != id {
		t.Fatal("existing selection lost identity")
	}
}

// Neither a hung daemon nor stale metadata should turn name lookup into a
// network health check. A live hung daemon must still reserve its public name.
func TestSessionNameResolutionUsesTaskTable(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	id := strings.Repeat("a", 32)
	shell := &remoteShell{shellTask: shellTask{hosted: true, tasks: tasks, record: localShellRecord{UID: id, Name: "work"}, done: make(chan struct{})}}
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
	ref := strings.ToUpper(uid[:2])
	cmd := exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "rename-session", Name: ref, NewName: "work"})...)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rename by immutable reference: %s %v", out, err)
	}
	b, resumedPID := e.attach(ref, "attach")
	if resumedPID != pid {
		t.Fatal("alias change replaced the shell")
	}
	resolved, err := resolveSession("work")
	if err != nil || resolved != uid {
		t.Fatalf("alias changed identity: %q %v", resolved, err)
	}
	sibling := strings.Repeat("b", 32)
	if strings.HasPrefix(sibling, strings.ToLower(ref)) {
		sibling = strings.Repeat("c", 32)
	}
	if err := createRemoteShell(e.bin, taskIdentitySelection{name: "sibling", uid: sibling}, false, e.dir, nil); err != nil {
		t.Fatal(err)
	}
	c, err := e.dial("work")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	reply, err := exchangeRouteRequest(c, message{Type: "shell_connect", Action: "attach", Name: strings.ToUpper(sibling[:2])})
	c.Close()
	if err != nil || reply.Type != "setup_ready" || reply.Instance != sibling {
		t.Fatalf("nested prefix selection: %+v %v", reply, err)
	}
	stale := strings.Repeat("0", 32)
	if stale == uid {
		stale = strings.Repeat("1", 32)
	}
	cmd = exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "shell", Mode: "attach", Name: "work", Instance: stale})...)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "does not exist") {
		t.Fatalf("stale attach selected a live alias: %s %v", out, err)
	}
	cmd = exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "kill-session", Name: "work", Instance: stale})...)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "instance changed") {
		t.Fatalf("stale kill selected a live alias: %s %v", out, err)
	}
	cmd = exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "kill-session", Name: ref})...)
	cmd.Env = e.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kill by prefix: %s %v", out, err)
	}
	waitSessionEnded(t, e, "work", pid)
	b.close()
}

// Uses the existing isolated CLI test executable, never account sessions.
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
		cmd := exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "kill-session", Name: name, Release: release})...)
		cmd.Env = e.env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	keep, keepPID := e.attach("keep")
	keepProcess, err := proc.ReadTask(keepPID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "kill-session", Name: "keep"})...)
	blocked.Env = append(append([]string{}, e.env...), "SSHORE_DISABLED=1")
	if out, err := blocked.CombinedOutput(); err == nil || !strings.Contains(string(out), "disabled") {
		t.Fatal("recovery allowed kill", string(out), err)
	}
	listing := exec.Command(e.bin, remoteServiceArgs(remoteServiceRequest{Operation: "inventory"})...)
	listing.Env = e.env
	var inv inventory
	if out, err := listing.CombinedOutput(); err != nil || json.Unmarshal(out, &inv) != nil || len(inv.Shells) != 1 || inv.Shells[0].Name != "keep" || inv.Shells[0].State != taskActive || inv.Shells[0].Age == nil {
		t.Fatal("missing list age", string(out), err)
	}
	for _, detached := range []bool{false, true} {
		name := fmt.Sprintf("target-%t", detached)
		b, pid := e.attach(name)
		targetProcess, err := proc.ReadTask(pid)
		if err != nil || targetProcess.Parent != keepProcess.Parent {
			t.Fatalf("shells do not share a daemon: %+v %+v %v", keepProcess, targetProcess, err)
		}
		// Cooperative shell tasks must all end without affecting sibling shells.
		childFile := filepath.Join(e.dir, name+".child")
		shellOutput(t, b, "sh -c 'echo $$ > "+shellQuote(childFile)+"; exec sleep 120' & printf 'CHILD_%s\\n' ready", "CHILD_ready")
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
			b, resumedPID := e.attach(name, "attach")
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
			if task, err := proc.ReadTask(taskPID); err == nil && task.State != "Z" {
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
	records, err := hostedShells("health")
	if err != nil || len(records) != 0 {
		t.Fatalf("ended shells remain registered: %+v %v", records, err)
	}

}

func TestCleanupEndsUncooperativeTask(t *testing.T) {
	childFile := filepath.Join(t.TempDir(), "child")
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM HUP; sleep 120 & echo $! > "+shellQuote(childFile)+"; printf ready; while :; do read line || exit; done")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); cmd.Process.Kill(); cmd.Wait() })
	if _, err := io.ReadFull(output, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	start, err := proc.StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := stopOwnedProcesses([]setupProcess{{PID: cmd.Process.Pid, Start: start + "-changed"}}); err == nil {
		t.Fatal("changed process identity was accepted")
	}
	childData, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childData)))
	if err != nil {
		t.Fatal(err)
	}
	child, err := proc.ReadTask(childPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Signal(childPID, child.Start, 9) })
	unrelated := exec.Command("sleep", "120")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unrelated.Process.Kill(); unrelated.Wait() })
	if err := stopOwnedProcesses([]setupProcess{{PID: cmd.Process.Pid, Start: start}}); err != nil {
		t.Fatal(err)
	}
	if task, err := proc.ReadTask(cmd.Process.Pid); err == nil && task.State != "Z" && task.State != "X" {
		t.Fatalf("task survived kill: %+v", task)
	}
	if task, err := proc.ReadTask(childPID); err == nil && task.Start == child.Start && task.State != "Z" && task.State != "X" {
		t.Fatalf("child survived kill: %+v", task)
	}
	if task, err := proc.ReadTask(unrelated.Process.Pid); err != nil || task.State == "Z" || task.State == "X" {
		t.Fatalf("unrelated process was ended: %+v %v", task, err)
	}
}

func TestSessionProcessTree(t *testing.T) {
	snapshot := newProcessSnapshot([]proc.Process{
		{PID: 10, Parent: 1, Group: 10, Foreground: 20, Name: "bash"},
		{PID: 20, Parent: 10, Group: 20, Foreground: 20, Name: "codex"},
		{PID: 21, Parent: 20, Group: 20, Foreground: 20, Name: "worker (job)"},
		{PID: 30, Parent: 10, Group: 30, Foreground: 20, Name: "sleep"},
		{PID: 40, Parent: 1, Group: 40, Foreground: 40, Name: "unrelated"},
		{PID: 50, Parent: 30, Group: 30, Foreground: 20, Name: "bad\x1bname"},
	})
	got := snapshot.sessionTree(10)
	expected := "bash (10)\n├─ codex (20) [fg]\n│  └─ worker (job) (21) [fg]\n└─ sleep (30)\n   └─ bad�name (50)\n"
	if got != expected {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, expected)
	}
	if got := snapshot.sessionTree(99); !strings.Contains(got, "99 [process unavailable]") {
		t.Fatal(got)
	}
}

func TestForegroundSnapshot(t *testing.T) {
	snapshot := newProcessSnapshot([]proc.Process{
		{PID: 3, Parent: -1, Group: 42, Foreground: -1, Name: "cat (worker)"},
		{PID: 4, Parent: -1, Group: 99, Foreground: -1, Name: "background"},
		{PID: 6, Parent: -1, Group: 42, Foreground: -1, Name: "bad\nname"},
		{PID: 20, Parent: -1, Group: 42, Foreground: -1, Name: "grep"},
	})
	if got := snapshot.foregroundPrograms(42); got != "cat (worker) | bad�name | grep" {
		t.Fatal(got)
	}
	for _, group := range []int{0, 100} {
		if got := snapshot.foregroundPrograms(group); got != "—" {
			t.Fatal(got)
		}
	}
	if got := newProcessSnapshot(nil).foregroundPrograms(42); got != "—" {
		t.Fatal(got)
	}
}
