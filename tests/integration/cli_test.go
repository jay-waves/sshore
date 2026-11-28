//go:build integration

package tests_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI must resolve help and reject invalid requests before touching account state.
func TestCLI(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_OPENSSH")
	if binary == "" {
		binary = os.Getenv("SSHORE_TEST_WINDOWS_CLIENT")
	}
	if binary == "" {
		t.Skip("set SSHORE_TEST_OPENSSH or SSHORE_TEST_WINDOWS_CLIENT to a built executable")
	}

	for _, tc := range []struct {
		name   string
		args   []string
		status int
		output string
	}{
		{"main help groups commands", nil, 0, "Account maintenance"},
		{"alias help", []string{"ps", "--help"}, 0, "Usage: sshore pstree"},
		{"go help shows the canonical start command", []string{"go", "--help"}, 0, "Usage: sshore start"},
		{"embark is no longer a command", []string{"embark"}, 2, "unknown command: embark"},
		{"retired launch command is rejected", []string{"new"}, 2, "unknown command: new"},
		{"help before invalid switch", []string{"kill", "--unrecognized", "--help"}, 0, "Usage: sshore kill"},
		{"help past SSH delimiter is an SSH argument", []string{"shell", "work", "--ssh", "--help"}, 2, "unsupported SSH option after --ssh"},
		{"duplicate SSH port", []string{"a", "work", "--port", "2222", "--ssh", "-o", "Port=22"}, 2, "--port cannot be combined"},
		{"port short and long forms share one setting", []string{"sh", "work", "-p", "2222", "--port", "22"}, 2, "--port may only be specified once"},
		{"attached port value is validated", []string{"sh", "work", "-p65536"}, 2, "requires a port from 1 to 65535"},
		{"identity files can be repeated before and after the SSH delimiter", []string{"start", "-i", "first key", "-isecond-key", "--ssh", "-i", "third-key"}, 2, "local tasks do not accept SSH options"},
		{"jump requires a value before the next option", []string{"start", "work", "-J", "-p", "2222"}, 2, "missing value for -J"},
		{"remote shell accepts a host with an empty startup path", []string{"sh", "-F", "none", "work:"}, 1, "an interactive terminal is required"},
		{"program launch accepts a host with an empty startup path", []string{"start", "work:", "--", "vim"}, 2, "start requires an interactive terminal"},
		{"empty local startup path selects the local account", []string{"sh", ":", "-F", "none"}, 2, "local tasks do not accept SSH options"},
		{"SSH config cannot be repeated across option blocks", []string{"sh", "work", "-F", "none", "--ssh", "-F", "other"}, 2, "-F cannot be combined"},
		{"local scope rejects SSH options", []string{"attach", "--port=2222"}, 2, "local tasks do not accept SSH options"},
		{"cannot attach a typed selector", []string{"attach", "forward/485bad"}, 2, "attach expects a task ID, name or host"},
		{"cannot attach a scoped selector", []string{"attach", "work:485bad"}, 2, "invalid host"},
		{"cannot kill a scoped selector", []string{"kill", "work:485bad"}, 2, "kill requires"},
		{"creation name cannot be an ID", []string{"shell", "work", "--name", "db"}, 2, "reserved for task IDs"},
		{"old shell name syntax is rejected", []string{"shell", "work:editor"}, 2, "use --name for a task name"},
		{"go uses the same launch review as start", []string{"go"}, 2, "start requires an interactive terminal"},
		{"start rejects shell flags for an explicit program", []string{"start", "--no-profile", "--", "vim"}, 2, "require starting the default shell"},
		{"shell command requires a host before the delimiter", []string{"sh", "--", "work"}, 2, "requires one SSH host"},
		{"shell command rejects an empty tail", []string{"sh", "work", "--"}, 2, "expected a command after --"},
		{"shell command rejects pin", []string{"sh", "work", "--pin", "--", "uptime"}, 2, "--pin requires an interactive shell"},
		{"shutdown accepts one account", []string{"kill", "--all", "one", "two"}, 2, "unexpected argument: two"},
		{"empty host cannot become a local shutdown", []string{"kill", "--all", "--", ""}, 2, "empty argument"},
		{"reset requires daemon shutdown scope", []string{"kill", "work", "--reset"}, 2, "--reset requires --all"},
		{"reset help is read-only", []string{"kill", "--all", "--reset", "--help"}, 0, "--reset also clears"},
		{"install help is read-only", []string{"install", "--help"}, 0, "Usage: sshore install"},
		{"install rejects a remote path", []string{"install", "work:/usr/bin/sshore"}, 2, "install applies to this machine"},
		{"install apply requires an explicit local path", []string{"install", "--apply"}, 2, "requires :PATH"},
		{"client endpoint requires an attached shell", []string{"cp", "./file", "::"}, 2, "requires an attached sshore shell"},
		{"numeric client endpoint is retired", []string{"cp", "./file", "0:"}, 2, "use :: in place of 0:"},
		{"IPv6-shaped origin is rejected without an attachment", []string{"fwd", "3000", "::1"}, 2, "requires an attached sshore shell"},
		{"startup reserves IPv6-shaped origin", []string{"sh", "::1"}, 2, "select : or an SSH host"},
		{"bracketed IPv6 accepts a startup path", []string{"sh", "[::1]:/tmp"}, 1, "an interactive terminal is required"},
		{"bracketed IPv6 accepts a forward port", []string{"fwd", "[::1]:70000", ":"}, 2, "port must be an integer"},
		{"retired dot endpoint does not become an SSH host", []string{"cp", "file", ".:"}, 2, "use : for the local endpoint"},
		{"retired double dot endpoint does not become an SSH host", []string{"cp", "file", "..:"}, 2, "use :: for the attached client"},
		{"retired here alias does not become an SSH host", []string{"cp", "file", "here:path"}, 2, "use : for the local endpoint"},
		{"retired origin alias does not become an SSH host", []string{"cp", "file", "origin:path"}, 2, "use :: for the attached client"},
		{"forward batch requires a target without a port", []string{"fwd", "100", "200", "::3000"}, 2, "require a target without a port"},
		{"reverse forward batch requires distinct source ports", []string{"fwd", "100", ":0100", "work:"}, 2, "duplicate source port"},
		{"forward batch requires a single source host", []string{"fwd", "work:100", "other:200", ":"}, 2, "must be on the same machine"},
		{"forward batch validates every source before startup", []string{"fwd", "work:100", "work:70000", ":"}, 2, "port must be an integer"},
		{"LF copy requires a terminal before dialing", []string{"copy", "--lf", ":", "work:"}, 2, "file selection requires an interactive terminal"},
		{"LF mount requires a terminal before dialing", []string{"mount", "--lf", "work:", ":"}, 2, "file selection requires an interactive terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("SSHORE_STATE_DIR", state)
			t.Setenv("SSHORE_SOCKET", "")
			cmd := exec.CommandContext(t.Context(), binary, tc.args...)
			output, err := cmd.CombinedOutput()
			status := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				status = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if status != tc.status || !strings.Contains(string(output), tc.output) {
				t.Fatalf("%v exited %d, expected %d with %q\n%s", tc.args, status, tc.status, tc.output, output)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("parsing changed task state: %v", err)
			}
		})
	}
}
