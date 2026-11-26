package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func publish(build func(string) ([]string, error)) error {
	ref := os.Getenv("GITHUB_REF")
	version, ok := strings.CutPrefix(ref, "refs/tags/v")
	if !ok || version == "0.0.0" || !fullVersion.MatchString(version) {
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

Use the platform installer on each endpoint to install %[1]s for managed connections.
Bootstrap ends the target account's tasks, including pinned shells, and clears
its sshore identity, history and cached releases. Save work before bootstrapping.
Commit: %[2]s
`, version, commit))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
