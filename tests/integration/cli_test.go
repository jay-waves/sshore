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

func TestCLI(t *testing.T) {
	binary := os.Getenv("SSHORE_TEST_OPENSSH")
	if binary == "" {
		binary = os.Getenv("SSHORE_TEST_WINDOWS_CLIENT")
	}
	if binary == "" {
		t.Skip("set SSHORE_TEST_OPENSSH or SSHORE_TEST_WINDOWS_CLIENT to a built executable")
	}

	for _, tc := range []struct {
		name     string
		args     []string
		recovery bool
		status   int
		output   string
	}{
		{"main help groups commands", nil, false, 0, "Account maintenance"},
		{"alias help in recovery", []string{"ps", "--help"}, true, 0, "Usage: sshore pstree"},
		{"help before invalid switch", []string{"kill", "--unrecognized", "--help"}, false, 0, "Usage: sshore kill"},
		{"examples use alias", []string{"sh", "--tldr"}, true, 0, "Open a remote shell"},
		{"help past SSH delimiter is an SSH argument", []string{"shell", "work", "--ssh", "--help"}, false, 2, "unsupported SSH option after --ssh"},
		{"duplicate SSH port", []string{"a", "work", "--port", "2222", "--ssh", "-o", "Port=22"}, false, 2, "--port cannot be combined"},
		{"local scope rejects SSH options", []string{"attach", "--port=2222"}, false, 2, "local tasks do not accept SSH options"},
		{"cannot attach a typed selector", []string{"attach", "forward/485bad"}, false, 2, "attach expects a task ID, name or host"},
		{"cannot attach a scoped selector", []string{"attach", "work:485bad"}, false, 2, "invalid host"},
		{"cannot attach with two operands", []string{"attach", "work", "485bad"}, false, 2, "unexpected argument: 485bad"},
		{"host has no trailing colon", []string{"attach", "work:"}, false, 2, "invalid host"},
		{"cannot kill a scoped selector", []string{"kill", "work:485bad"}, false, 2, "kill requires"},
		{"cannot kill with two operands", []string{"kill", "work", "485bad"}, false, 2, "unexpected argument: 485bad"},
		{"creation name cannot be an ID", []string{"shell", "work", "--name", "db"}, false, 2, "reserved for task IDs"},
		{"creation name uses an option", []string{"shell", "work:/tmp", "editor"}, false, 2, "unexpected argument: editor"},
		{"old shell name syntax is rejected", []string{"shell", "work:editor"}, false, 2, "use --name for a task name"},
		{"old ssh delimiter is rejected", []string{"shell", "work", "--", "-J", "jump"}, false, 2, "shell does not accept --"},
		{"new rejects an empty tail", []string{"new", "work", "--ssh", "-J", "jump", "--"}, false, 2, "expected -- followed by an executable"},
		{"shell rejects an empty delimiter", []string{"shell", "--"}, false, 2, "shell does not accept --"},
		{"shell rejects a host after the delimiter", []string{"sh", "--", "work"}, false, 2, "shell does not accept --"},
		{"new documents shell startup options", []string{"new", "--help"}, false, 0, "--no-login"},
		{"new rejects shell flags for an explicit program", []string{"new", "--no-profile", "--", "vim"}, false, 2, "require starting the default shell"},
		{"new rejects an empty executable", []string{"new", "work", "--", ""}, false, 2, "expected -- followed by an executable"},
		{"exec requires a delimiter", []string{"exec", "work", "printf hello"}, false, 2, "expected HOST followed by -- and one nonempty command string"},
		{"exec rejects an empty tail", []string{"exec", "work", "--"}, false, 2, "expected HOST followed by -- and one nonempty command string"},
		{"exec rejects an empty command", []string{"exec", "work", "--", ""}, false, 2, "expected HOST followed by -- and one nonempty command string"},
		{"exec rejects multiple command strings", []string{"exec", "work", "--", "printf", "hello"}, false, 2, "expected HOST followed by -- and one nonempty command string"},
		{"rename is removed", []string{"rename", "485bad", "editor"}, false, 2, "unknown command: rename"},
		{"shutdown accepts one account", []string{"kill", "--all", "one", "two"}, false, 2, "unexpected argument: two"},
		{"empty host cannot become a local shutdown", []string{"kill", "--all", "--", ""}, false, 2, "empty argument"},
		{"account reset accepts no target", []string{"reset", "work:"}, false, 2, "unexpected argument: work:"},
		{"bootstrap rejects a bare host", []string{"bootstrap", "work"}, false, 2, "bootstrap installs on this machine"},
		{"bootstrap rejects a remote path", []string{"bootstrap", "work:/usr/bin/sshore"}, false, 2, "bootstrap installs on this machine"},
		{"bootstrap apply requires an explicit local path", []string{"bootstrap", "--apply"}, false, 2, "requires here:PATH"},
		{"recovery rejects task creation", []string{"shell", "--name", "blocked"}, true, 1, "sshore features are disabled"},
		{"forwards reject foreground mode", []string{"fwd", "8080", "work:80", "--foreground"}, false, 2, "unsupported option: --foreground"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			t.Setenv("SSHORE_STATE_DIR", state)
			t.Setenv("SSHORE_DISABLED", "")
			t.Setenv("SSHORE_SOCKET", "")
			if tc.recovery {
				t.Setenv("SSHORE_DISABLED", "1")
			}
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
