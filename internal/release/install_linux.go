package release

import (
	"embed"
	"os"
	"os/exec"
	"path/filepath"
)

func RunInstaller(target string, scripts embed.FS) error {
	script, err := scripts.ReadFile("install.sh")
	if err != nil {
		return err
	}

	if target == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		target = filepath.Join(filepath.Dir(executable), "sshore")
	}
	cmd := exec.Command("sh", "-c", string(script), "install.sh", "--path", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
