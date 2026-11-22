package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const sftpModule = "github.com/pkg/sftp/v2"

func withPatchedSFTP(run func(workspace string) error) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	download := workspaceGo("off", "mod", "download", "-json", sftpModule)
	download.Stdout = nil
	data, err := download.Output()
	if err != nil {
		return fmt.Errorf("download pinned SFTP module: %w\n%s", err, data)
	}
	var mod struct{ Dir, Version string }
	if err := json.Unmarshal(data, &mod); err != nil {
		return fmt.Errorf("read SFTP module metadata: %w", err)
	}
	if mod.Dir == "" || mod.Version == "" {
		return fmt.Errorf("Go did not report a directory and version for %s", sftpModule)
	}

	temp, err := os.MkdirTemp("", "sshore-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	source := filepath.Join(temp, "sftp")
	if err := os.CopyFS(source, os.DirFS(mod.Dir)); err != nil {
		return fmt.Errorf("copy SFTP module: %w", err)
	}
	apply := exec.Command("git", "apply", filepath.Join(root, "scripts", "patches", "pkg-sftp-v2.patch"))
	apply.Dir, apply.Stdout, apply.Stderr = source, os.Stdout, os.Stderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("apply SFTP patch to %s: %w", mod.Version, err)
	}

	workspace := filepath.Join(temp, "go.work")
	for _, args := range [][]string{
		{"work", "init", root},
		{"work", "edit", "-replace=" + sftpModule + "@" + mod.Version + "=./sftp"},
	} {
		cmd := workspaceGo(workspace, args...)
		cmd.Dir = temp
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("prepare build workspace: %w", err)
		}
	}
	return run(workspace)
}

func workspaceGo(workspace string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOWORK="+workspace)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}
