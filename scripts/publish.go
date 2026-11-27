package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jay-waves/sshore/internal/release"
)

func publish(build func(string) ([]string, error)) error {
	ref := os.Getenv("GITHUB_REF")
	version, ok := strings.CutPrefix(ref, "refs/tags/v")
	if !ok || version == "0.0.0" || !release.Valid(version) {
		return fmt.Errorf("expected a full release tag in GITHUB_REF, got %q", ref)
	}
	commit := os.Getenv("GITHUB_SHA")
	if commit == "" {
		return fmt.Errorf("GITHUB_SHA is required to publish")
	}
	token := os.Getenv("GH_TOKEN")
	os.Unsetenv("GH_TOKEN")
	assets, err := build(version)
	if err != nil {
		return err
	}
	tag := "v" + version
	args := []string{"release", "create", tag, "--verify-tag", "--generate-notes",
		"--title", "sshore " + tag, "--notes-file", "-", "--latest"}
	cmd := exec.Command("gh", append(args, assets...)...)
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`sshore %s

Includes Linux and Windows amd64 binaries. Verify archives against SHA256SUMS.

Use sshore kill with an ID or name for shells, mounts and forwards.
Use sshore kill HOST --all to stop a remote account's daemon.

Omitted copy paths open a picker after SSH authentication.
For non-interactive mounts, use HOST:~ to select the remote home and .: to
allocate a local mountpoint automatically.

Install the client locally. Managed SSH connections deploy the matching server automatically.
The installer reviews active tasks before stopping them for replacement.
Installation preserves settings and history. Use sshore kill --all --reset to clear state.
Commit: %[2]s
`, version, commit))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
