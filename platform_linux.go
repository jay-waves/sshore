package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jay-waves/sshore/internal/sys"
)

func reportBootstrapCgroup() {
	if err := sys.ProbeShellCgroup(); err != nil {
		fmt.Fprintf(os.Stderr, "Linux shell cgroup unavailable: %v. Mounts and forwards remain available.\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "Linux shell cgroup available.")
}

func daemonTaskCgroup() (string, error) { return sys.DaemonTaskCgroup() }

func checkPlatform() error { return nil }

func runLocalInstaller(target string) error {
	if target == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		target = filepath.Join(filepath.Dir(executable), "sshore")
	}
	cmd := exec.Command("sh", "-c", linuxInstaller, "install.sh", "--path", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
