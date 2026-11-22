package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func publish() error {
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
	assets, err := buildPackages(version)
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
Update both endpoints to %[1]s for managed connections. Save work before
installing because the installer stops the target account daemon.
Remote releases before 0.7.22 do not support the current service entry. If remote
maintenance is needed before upgrading, run that release's sshore kill --all on the host.
Commit: %[2]s
`, version, commit))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
