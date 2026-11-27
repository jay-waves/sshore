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
		{"help before invalid switch", []string{"kill", "--unrecognized", "--help"}, 0, "Usage: sshore kill"},
		{"examples use alias", []string{"sh", "--tldr"}, 0, "Open a remote shell"},
		{"help past SSH delimiter is an SSH argument", []string{"shell", "work", "--ssh", "--help"}, 2, "unsupported SSH option after --ssh"},
		{"duplicate SSH port", []string{"a", "work", "--port", "2222", "--ssh", "-o", "Port=22"}, 2, "--port cannot be combined"},
		{"port short and long forms share one setting", []string{"sh", "work", "-p", "2222", "--port", "22"}, 2, "--port may only be specified once"},
		{"attached port value is validated", []string{"sh", "work", "-p65536"}, 2, "requires a port from 1 to 65535"},
		{"attached jump value is recognized", []string{"new", "-Jjump"}, 2, "local tasks do not accept SSH options"},
		{"identity files can be repeated before and after the SSH delimiter", []string{"new", "-i", "first key", "-isecond-key", "--ssh", "-i", "third-key"}, 2, "local tasks do not accept SSH options"},
		{"jump requires a value before the next option", []string{"new", "work", "-J", "-p", "2222"}, 2, "missing value for -J"},
		{"duplicate jump option", []string{"sh", "work", "-J", "jump", "--ssh", "-J", "other"}, 2, "-J cannot be combined"},
		{"SSH config can be selected before the host", []string{"shell", "-F", "none", "work:"}, 2, "invalid startup path"},
		{"attached SSH config is recognized", []string{"new", "-Fnone"}, 2, "local tasks do not accept SSH options"},
		{"SSH config requires a value before the next option", []string{"new", "-F", "-p", "2222"}, 2, "missing value for -F"},
		{"SSH config cannot be repeated across option blocks", []string{"sh", "work", "-F", "none", "--ssh", "-F", "other"}, 2, "-F cannot be combined"},
		{"local scope rejects SSH options", []string{"attach", "--port=2222"}, 2, "local tasks do not accept SSH options"},
		{"cannot attach a typed selector", []string{"attach", "forward/485bad"}, 2, "attach expects a task ID, name or host"},
		{"cannot attach a scoped selector", []string{"attach", "work:485bad"}, 2, "invalid host"},
		{"cannot attach with two operands", []string{"attach", "work", "485bad"}, 2, "unexpected argument: 485bad"},
		{"host has no trailing colon", []string{"attach", "work:"}, 2, "invalid host"},
		{"cannot kill a scoped selector", []string{"kill", "work:485bad"}, 2, "kill requires"},
		{"cannot kill with two operands", []string{"kill", "work", "485bad"}, 2, "unexpected argument: 485bad"},
		{"creation name cannot be an ID", []string{"shell", "work", "--name", "db"}, 2, "reserved for task IDs"},
		{"creation name uses an option", []string{"shell", "work:/tmp", "editor"}, 2, "unexpected argument: editor"},
		{"old shell name syntax is rejected", []string{"shell", "work:editor"}, 2, "use --name for a task name"},
		{"new rejects an empty tail", []string{"new", "work", "--ssh", "-J", "jump", "--"}, 2, "expected -- followed by an executable"},
		{"new documents shell startup options", []string{"new", "--help"}, 0, "--no-login"},
		{"new requires a terminal for launch review", []string{"new"}, 2, "new requires an interactive terminal"},
		{"new rejects shell flags for an explicit program", []string{"new", "--no-profile", "--", "vim"}, 2, "require starting the default shell"},
		{"shell command requires a host before the delimiter", []string{"sh", "--", "work"}, 2, "requires one SSH host"},
		{"shell command rejects an empty tail", []string{"sh", "work", "--"}, 2, "expected a command after --"},
		{"shell command rejects pin", []string{"sh", "work", "--pin", "--", "uptime"}, 2, "--pin requires an interactive shell"},
		{"shutdown accepts one account", []string{"kill", "--all", "one", "two"}, 2, "unexpected argument: two"},
		{"empty host cannot become a local shutdown", []string{"kill", "--all", "--", ""}, 2, "empty argument"},
		{"reset requires daemon shutdown scope", []string{"kill", "work", "--reset"}, 2, "--reset requires --all"},
		{"reset help is read-only", []string{"kill", "--all", "--reset", "--help"}, 0, "--reset also clears"},
		{"install help is read-only", []string{"install", "--help"}, 0, "Usage: sshore install"},
		{"install rejects a bare host", []string{"install", "work"}, 2, "install applies to this machine"},
		{"install rejects a remote path", []string{"install", "work:/usr/bin/sshore"}, 2, "install applies to this machine"},
		{"install apply requires an explicit local path", []string{"install", "--apply"}, 2, "requires here:PATH"},
		{"forwards reject foreground mode", []string{"fwd", "8080", "work:80", "--foreground"}, 2, "unsupported option: --foreground"},
		{"mounts only start in the background", []string{"mount", "work:/srv", ".:", "--foreground"}, 2, "unsupported option: --foreground"},
		{"origin shorthand requires an attached shell", []string{"cp", "./file", "0:"}, 2, "requires an attached sshore shell"},
		{"retired origin shorthand is rejected", []string{"cp", "./file", "::file"}, 2, "use 0: or origin:"},
		{"cp opens help for copy", []string{"cp", "--help"}, 0, "Usage: sshore copy"},
		{"omitted copy paths require a terminal before dialing", []string{"copy", "here:", "work:"}, 2, "file selection requires an interactive terminal"},
		{"omitted mount paths require a terminal before dialing", []string{"mount", "work:", "here:"}, 2, "file selection requires an interactive terminal"},
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
