//go:build integration

package tests_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
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
	ctx, command := windowsClient(t)
	const host = "127.0.0.1"
	const endpoint = host + ":ci-shell"
	sshOptions := []string{"-F", "none", "-p", "22222", "-l", "sshore-fixture", "-i", key,
		"-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile=" + hosts, "-o", "StrictHostKeyChecking=yes"}
	raw := func(t *testing.T, release, mode, name string) (reply struct{ Type, Error string }) {
		t.Helper()
		request, err := json.Marshal(map[string]any{
			"protocol": 1, "release": release, "operation": "shell", "mode": mode, "name": name,
		})
		if err != nil {
			t.Fatal(err)
		}
		args := append(append([]string{"-o", "BatchMode=yes"}, sshOptions...), host,
			"sshore service base64:"+base64.RawURLEncoding.EncodeToString(request))
		var diagnostic strings.Builder
		probe := exec.CommandContext(ctx, "ssh.exe", args...)
		probe.Stderr = &diagnostic
		out, runErr := probe.Output()
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatalf("remote shell response: %v (%v)\n%s\n%s", err, runErr, out, diagnostic.String())
		}
		if runErr == nil || reply.Type != "fatal" {
			t.Fatalf("expected service rejection, got %s (exit %v)", out, runErr)
		}
		return reply
	}
	run := func(t *testing.T, verb, target string, args []string, steps func(func(string), terminalWait)) {
		t.Helper()
		args = append(append([]string{verb, target}, args...), "--")
		args = append(args, sshOptions...)
		if out, err := runWindowsTerminal(t, ctx, command(args...), steps); err != nil {
			t.Fatalf("Windows OpenSSH client: %v\n%s", err, out)
		}
	}
	t.Run("reject release mismatch", func(t *testing.T) {
		reply := raw(t, "0.0.1", "new", "wrong-release")
		if !strings.Contains(reply.Error, "0.0.0") || !strings.Contains(reply.Error, "0.0.1") {
			t.Fatalf("wrong release diagnostic: %s", reply.Error)
		}
	})
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
		run(t, "attach", endpoint, nil, func(send func(string), wait terminalWait) {
			wait("Connected " + endpoint)
			send("echo CI_RESUMED=%SSHORE_REMOTE_STATE%\r")
			wait("CI_RESUMED=retained")
			send("exit\r")
		})
	}) {
		return
	}
	t.Run("exited shell cannot be attached", func(t *testing.T) {
		reply := raw(t, "0.0.0", "attach", "ci-shell")
		if !strings.Contains(reply.Error, "does not exist") {
			t.Fatalf("expected missing task after shell exit, got %q", reply.Error)
		}
	})
}
