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

Each shell tree has one foreground attachment. Detach keeps a shell under its
parent. When the parent exits, pinned children become independent roots and
the command prompt shows what stayed alive.

Use pstree for shell relationships and list for task details. The list shows
the parent short ID and includes forward direction in each port mapping.

Use sshore bootstrap to update both endpoints to %[1]s for managed connections.
Bootstrap ends the target account's tasks, including pinned shells, and clears
its sshore identity, history and cached releases. Save work before bootstrapping.
Commit: %[2]s
`, version, commit))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
