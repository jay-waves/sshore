package main

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func withPatchedModules(run func(workspace string) error) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "sshore-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	workspace := filepath.Join(temp, "go.work")
	init := workspaceGo(workspace, "work", "init", root)
	init.Dir = temp
	if err := init.Run(); err != nil {
		return err
	}
	for _, patch := range []struct{ module, name string }{
		{"github.com/pkg/sftp/v2", "pkg-sftp-v2"},
		{"github.com/winfsp/go-winfsp", "go-winfsp"},
		{"golang.org/x/crypto", "x-crypto-ssh"},
	} {
		if err := patchModule(root, workspace, patch.module, patch.name); err != nil {
			return err
		}
	}
	return run(workspace)
}

func patchModule(root, workspace, module, name string) error {
	download := workspaceGo("off", "mod", "download", "-json", module)
	download.Stdout = nil
	data, err := download.Output()
	if err != nil {
		return fmt.Errorf("download %s: %w\n%s", module, err, data)
	}
	var mod struct{ Dir, Version string }
	if err := json.Unmarshal(data, &mod); err != nil {
		return err
	}
	if mod.Dir == "" || mod.Version == "" {
		return fmt.Errorf("Go did not report a directory and version for %s", module)
	}
	source := filepath.Join(filepath.Dir(workspace), name)
	if err := os.CopyFS(source, os.DirFS(mod.Dir)); err != nil {
		return err
	}
	apply := exec.Command("git", "apply", filepath.Join(root, "scripts", "patches", name+".patch"))
	apply.Dir, apply.Stdout, apply.Stderr = source, os.Stdout, os.Stderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("patch %s@%s: %w", module, mod.Version, err)
	}
	edit := workspaceGo(workspace, "work", "edit", "-replace="+module+"@"+mod.Version+"=./"+name)
	edit.Dir = filepath.Dir(workspace)
	return edit.Run()
}

func workspaceGo(workspace string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOWORK="+workspace)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}
