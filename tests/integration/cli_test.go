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
		{"help past SSH delimiter is an SSH argument", []string{"shell", "work:", "--", "--help"}, false, 2, "unsupported SSH option after --"},
		{"duplicate SSH port", []string{"a", "ab:", "--port", "2222", "--", "-o", "Port=22"}, false, 2, "--port cannot be combined"},
		{"local scope rejects SSH options", []string{"attach", ":485bad", "--port=2222"}, false, 2, "local tasks do not accept SSH options"},
		{"cannot attach a forward", []string{"attach", "forward/485bad"}, false, 2, "attach requires a shell"},
		{"new alias cannot be an ID", []string{"rename", "work:485bad", "654321"}, false, 2, "reserved for task IDs"},
		{"shutdown accepts one account", []string{"kill", "--all", "one:", "two:"}, false, 2, "unexpected argument: two:"},
		{"account reset accepts no target", []string{"reset", "work:"}, false, 2, "unexpected argument: work:"},
		{"recovery rejects task creation", []string{"shell", "here:blocked"}, true, 1, "sshore features are disabled"},
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
