package main

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestShellRetentionAcrossClientLoss(t *testing.T) {
	for _, host := range []string{"here:", "fixture"} {
		for _, pinned := range []bool{false, true} {
			name := host + "/temporary"
			if pinned {
				name = host + "/pinned"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("SSHORE_LOCAL_SESSION", "")
				e := newEnv(t)
				args := []string{"fixture-sh", host, "retained", "--no-profile"}
				if pinned {
					args = append(args, "--pin")
				}
				if host != "here:" {
					args = append(args, "--server", e.bin)
				}
				cmd, _, wait := startSessionClient(t, e, args)
				wait(":retained")
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				env := e.env
				if host == "here:" {
					env = cmd.Env
				}
				waitForFixtureShell(t, e.bin, env, "retained", pinned, pinned)
			})
		}
	}
}

func waitForFixtureShell(t *testing.T, bin string, env []string, name string, present, pinned bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd := exec.Command(bin, "list", "--json")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var tasks inventory
		if err := json.Unmarshal(out, &tasks); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, task := range tasks.Shells {
			found = found || !task.Observed && task.Name == name && (!present || task.Pinned == pinned && task.State == taskDetached)
		}
		for _, task := range tasks.LocalShells {
			found = found || task.Name == name && (!present || task.Pinned == pinned && task.State == taskDetached)
		}
		if found == present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell %q present=%v: %s", name, present, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRootConsoleAfterShellDetachAndExit(t *testing.T) {
	for _, host := range []string{"here:", "fixture"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("SSHORE_LOCAL_SESSION", "")
			e := newEnv(t)
			args := []string{"fixture-sh", host, "resumable", "--no-profile"}
			label, config := "local", ""
			if host != "here:" {
				label, config = host, sessionSSH(t, e)
				args = append(args, "--server", e.bin)
			}
			cmd, terminal, wait := startSessionClientWithSSH(t, e, args, config, true)
			endpoint := host
			if host != "here:" {
				endpoint += ":"
			}
			wait("Connected " + endpoint + "resumable")
			io.WriteString(terminal, "export SHELL_MARKER=original\n\x02d")
			wait("Detached from")
			wait("No shell attached.")
			wait("sshore on " + label + "> ")
			io.WriteString(terminal, "attach resumable\r")
			wait("Connected " + endpoint + "resumable")
			io.WriteString(terminal, "printf 'ROOT_%s\\n' \"$SHELL_MARKER\"\n")
			wait("ROOT_original")
			io.WriteString(terminal, "exit\n")
			wait("Exited ")
			wait("No shell attached.")
			wait("sshore on " + label + "> ")
			io.WriteString(terminal, "exit\r")
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTimedDetachExpiresWhileRootConsoleStaysOpen(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	e := newEnv(t)
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-sh", "here:", "timed"}, "", true)
	wait("Connected here:timed")
	io.WriteString(terminal, "\x02:detach 200ms\r")
	wait("Detached from")
	wait("No shell attached.")
	wait("sshore on local> ")
	waitForFixtureShell(t, e.bin, cmd.Env, "timed", false, false)
	io.WriteString(terminal, "exit\r")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteNestedPinReturnsAndSurvivesParent(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	e := newEnv(t)
	config := sessionSSH(t, e)
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-sh", "fixture", "parent", "--server", e.bin}, config, true)
	wait("Connected fixture:parent")
	io.WriteString(terminal, shellQuote(e.bin)+" sh here:child --pin --no-profile\n")
	wait("Connected fixture:child")
	io.WriteString(terminal, "export SHELL_MARKER=retained\n\x02d")
	wait("Connected fixture:parent")
	io.WriteString(terminal, "exit\n")
	wait("Exited fixture:parent")
	wait("sshore on fixture> ")
	io.WriteString(terminal, "attach child\r")
	wait("Connected fixture:child")
	io.WriteString(terminal, "printf 'CHILD_%s\\n' \"$SHELL_MARKER\"\n")
	wait("CHILD_retained")
	io.WriteString(terminal, "exit\n")
	wait("Exited fixture:child")
	wait("sshore on fixture> ")
	io.WriteString(terminal, "exit\r")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedParentRetainsRemoteSubtree(t *testing.T) {
	t.Setenv("SSHORE_LOCAL_SESSION", "")
	e := newEnv(t)
	config := sessionSSH(t, e)
	cmd, terminal, wait := startSessionClientWithSSH(t, e, []string{"fixture-sh", "fixture", "parent", "--pin", "--server", e.bin}, config)
	wait("Connected fixture:parent")
	io.WriteString(terminal, shellQuote(e.bin)+" sh here:child --no-profile\n")
	wait("Connected fixture:child")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitForFixtureShell(t, e.bin, e.env, "parent", true, true)
	waitForFixtureShell(t, e.bin, e.env, "child", true, false)
	reopened, terminal, wait := startSessionClientWithSSH(t, e, []string{"attach", "fixture:parent", "--server", e.bin}, config)
	wait("Connected fixture:parent")
	io.WriteString(terminal, "exit\n")
	if err := reopened.Wait(); err != nil {
		t.Fatal(err)
	}
	waitForFixtureShell(t, e.bin, e.env, "child", false, false)
}

func TestDetachDurationGrammar(t *testing.T) {
	for _, arg := range []string{"30s", "60m", "2h"} {
		m, err := parseDetachCommand([]string{arg})
		if err != nil || m.TTL != mustDuration(t, arg) {
			t.Fatalf("%s: %+v, %v", arg, m, err)
		}
	}
	for _, args := range [][]string{{"0"}, {"-1s"}, {"oops"}, {"2h", "30s"}} {
		if _, err := parseDetachCommand(args); err == nil {
			t.Fatalf("accepted %s", strings.Join(args, " "))
		}
	}
}

func mustDuration(t *testing.T, text string) time.Duration {
	t.Helper()
	value, err := time.ParseDuration(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
