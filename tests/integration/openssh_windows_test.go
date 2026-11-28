//go:build integration

package tests_test

import (
	"context"
	json "encoding/json/v2"
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

func TestWindowsOpenSSH(t *testing.T) {
	key := os.Getenv("SSHORE_TEST_WINDOWS_SSH_KEY")
	hosts := os.Getenv("SSHORE_TEST_WINDOWS_SSH_HOSTS")
	if key == "" || hosts == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("Windows OpenSSH fixture was not configured")
		}
		t.Skip("requires the isolated Windows OpenSSH fixture")
	}
	const host = "127.0.0.1"
	const endpoint = host + ":ci-shell"
	sshOptions := []string{"-F", "none", "-p", "22222", "-l", "sshore-fixture", "-i", key,
		"-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile=" + hosts, "-o", "StrictHostKeyChecking=yes"}
	raw := func(t *testing.T, ctx context.Context, operation, release, mode, name string) (reply struct {
		Type            string `json:"type"`
		Error           string `json:"error"`
		SoftwareVersion string `json:"software_version"`
	}) {
		t.Helper()
		request, err := json.Marshal(map[string]any{
			"release": release, "operation": operation, "mode": mode, "name": name,
		})
		if err != nil {
			t.Fatal(err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		id, err := os.ReadFile(filepath.Join(home, ".sshore", "deployment-id"))
		if err != nil {
			t.Fatal(err)
		}
		args := append(append([]string{"-o", "BatchMode=yes"}, sshOptions...), host,
			`.\.sshore\clients\`+string(id)+`\0.0.0\sshore.exe service`)
		var diagnostic strings.Builder
		probe := exec.CommandContext(ctx, "ssh.exe", args...)
		probe.Stdin = strings.NewReader(string(request) + "\n")
		probe.Stderr = &diagnostic
		out, runErr := probe.Output()
		if len(out) == 0 || out[0] != 0 {
			t.Fatalf("remote %s did not acknowledge startup: %v\n%s\n%s", operation, runErr, out, diagnostic.String())
		}
		if err := json.Unmarshal(out[1:], &reply); err != nil {
			t.Fatalf("remote %s response: %v (%v)\n%s\n%s", operation, err, runErr, out, diagnostic.String())
		}
		if runErr == nil || reply.Type != "fatal" {
			t.Fatalf("expected service rejection, got %s (exit %v)", out, runErr)
		}
		return reply
	}
	ctx, command := windowsClient(t)
	run := func(t *testing.T, verb, target string, args []string, steps func(func(string), terminalWait)) {
		t.Helper()
		if verb == "shell" {
			hostName, name, _ := strings.Cut(target, ":")
			target = hostName
			if name != "" {
				args = append(args, "--name", name)
			}
		}
		args = append(append([]string{verb, target}, args...), "--ssh")
		args = append(args, sshOptions...)
		if out, err := runWindowsTerminal(t, ctx, command(args...), steps); err != nil {
			t.Fatalf("Windows OpenSSH client: %v\n%s", err, out)
		}
	}
	if !t.Run("cold start detach and reattach", func(t *testing.T) {
		run(t, "shell", endpoint, []string{"--no-profile"}, func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			send("echo CI_ACCOUNT=%USERNAME%\r")
			wait("CI_ACCOUNT=sshore-fixture")
			send("set SSHORE_REMOTE_STATE=retained\r")
			send("echo CI_READY=%SSHORE_REMOTE_STATE%\r")
			wait("CI_READY=retained")
			send("\x02d")
			wait("Detached from " + endpoint)
		})
		run(t, "attach", host, nil, func(send func(string), wait terminalWait) {
			wait("CI_READY=retained")
			send("echo CI_RESUMED=%SSHORE_REMOTE_STATE%\r")
			wait("CI_RESUMED=retained")
			send("exit\r")
		})
		if reply := raw(t, ctx, "shell", "0.0.0", "attach", "ci-shell"); !strings.Contains(reply.Error, "does not exist") {
			t.Fatalf("exited shell remained attachable: %q", reply.Error)
		}
	}) {
		return
	}
	t.Run("deployed service rejects a release mismatch", func(t *testing.T) {
		for _, operation := range []string{"shell", "cp-agent", "mount-agent", "forward-agent"} {
			reply := raw(t, ctx, operation, "0.0.1", "new", "wrong-release")
			if reply.SoftwareVersion != "0.0.0" || !strings.Contains(reply.Error, "0.0.1") {
				t.Fatalf("wrong release diagnostic: %+v", reply)
			}
		}
	})
	t.Run("origin resource approval", func(t *testing.T) {
		testWindowsOriginApproval(t, host, command, func(t *testing.T, endpoint string, steps func(func(string), terminalWait)) {
			run(t, "shell", endpoint, []string{"--no-profile"}, steps)
		})
	})
	t.Run("kill a detached shell", func(t *testing.T) {
		const target = host + ":ci-kill"
		run(t, "shell", target, []string{"--no-profile"}, func(send func(string), wait terminalWait) {
			wait("Connected " + target)
			send("\x02d")
			wait("Detached from " + target)
		})
		windowsRun(t, command(append([]string{"kill", "ci-kill", "--ssh"}, sshOptions...)...))
		if reply := raw(t, ctx, "shell", "0.0.0", "attach", "ci-kill"); !strings.Contains(reply.Error, "does not exist") {
			t.Fatalf("expected killed task to be absent, got %q", reply.Error)
		}
	})
	for _, reverse := range []bool{false, true} {
		name := "direct forward"
		if reverse {
			name = "reverse forward"
		}
		t.Run(name, func(t *testing.T) {
			testWindowsForward(t, command, host, sshOptions, reverse)
		})
	}
	t.Run("remote reset removes the matching server and its state", func(t *testing.T) {
		const name = "ci-reset"
		run(t, "shell", host+":"+name, []string{"--no-profile"}, func(send func(string), wait terminalWait) {
			wait("Connected " + host + ":" + name)
			send("\x02d")
			wait("Detached from " + host + ":" + name)
		})
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		id, err := os.ReadFile(filepath.Join(home, ".sshore", "deployment-id"))
		if err != nil {
			t.Fatal(err)
		}
		dir := `.\.sshore\clients\` + string(id) + `\0.0.0`
		rawCommand := func(remote string) []byte {
			args := append([]string{"sh", host, "--ssh"}, sshOptions...)
			return windowsRun(t, command(append(args, "--", remote)...))
		}
		state := dir + `\state\daemon\local.log`
		rawCommand(`if not exist ` + state + ` exit /b 1`)
		reset := command(append([]string{"kill", host, "--all", "--reset", "--ssh"}, sshOptions...)...)
		reset.Stdin = strings.NewReader("y\n")
		if out := windowsRun(t, reset); !strings.Contains(string(out), name) {
			t.Fatalf("reset did not display the detached task:\n%s", out)
		}
		rawCommand(`if exist ` + dir + `\sshore.exe exit /b 1`)
		rawCommand(`if exist ` + state + ` exit /b 1`)
		run(t, "shell", host+":ci-after-reset", []string{"--no-profile"}, func(send func(string), wait terminalWait) {
			wait("Connected " + host + ":ci-after-reset")
			send("exit\r")
		})
	})
}

func testWindowsForward(t *testing.T, command func(...string) *exec.Cmd, host string, sshOptions []string, reverse bool) {
	t.Helper()
	listen := func() net.Listener {
		t.Helper()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	service := listen()
	defer service.Close()
	reserved := listen()
	address := reserved.Addr().String()
	servicePort := service.Addr().(*net.TCPAddr).Port
	forwardPort := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()
	source, target := fmt.Sprintf("%s:%d", host, servicePort), fmt.Sprintf("here:%d", forwardPort)
	if reverse {
		source, target = fmt.Sprintf("here:%d", servicePort), fmt.Sprintf("%s:%d", host, forwardPort)
	}
	out := windowsRun(t, command(append([]string{"forward", source, target, "--ssh"}, sshOptions...)...))
	_, task, found := strings.Cut(string(out), "Started forward/")
	task, _, _ = strings.Cut(task, ":")
	if !found || task == "" {
		t.Fatalf("missing forward task in %q", out)
	}
	id := task
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			windowsRun(t, command("kill", id))
		}
	})

	// Two clients share the SSH connection. Each backend waits for EOF
	// before replying, so the relay must preserve TCP half-close.
	tcpService := service.(*net.TCPListener)
	_ = tcpService.SetDeadline(time.Now().Add(5 * time.Second))
	var clients, backends [2]*net.TCPConn
	for i := range clients {
		client, err := net.DialTimeout("tcp4", address, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		backend, err := tcpService.AcceptTCP()
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
		clients[i], backends[i] = client.(*net.TCPConn), backend
	}
	for i, client := range clients {
		backend := backends[i]
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		_ = backend.SetDeadline(time.Now().Add(5 * time.Second))
		msg := fmt.Sprintf("stream %d", i)
		if _, err := io.WriteString(client, msg); err != nil {
			t.Fatal(err)
		}
		if err := client.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if got, err := io.ReadAll(backend); err != nil || string(got) != msg {
			t.Fatalf("request %q (%v)", got, err)
		}
		if _, err := io.WriteString(backend, msg); err != nil {
			t.Fatal(err)
		}
		if err := backend.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if got, err := io.ReadAll(client); err != nil || string(got) != msg {
			t.Fatalf("response %q (%v)", got, err)
		}
	}
	windowsRun(t, command("kill", id))
	stopped = true
	if conn, err := net.DialTimeout("tcp4", address, time.Second); err == nil {
		conn.Close()
		t.Fatal("forward listener still open after stop")
	}
}
