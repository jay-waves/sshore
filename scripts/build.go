package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var fullVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*)){2}$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "go" {
		if len(args) == 1 {
			return fmt.Errorf("usage: go run ./scripts go <command> [args...]")
		}
		return withPatchedSFTP(func(workspace string) error {
			return workspaceGo(workspace, args[1:]...).Run()
		})
	}
	var tag string
	switch len(args) {
	case 0:
		cmd := exec.Command("git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*.[0-9]*.[0-9]*", "--exclude", "*-*", "--exclude", "*+*")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return err
		}
		tag = strings.TrimSpace(string(out))
	case 1:
		tag = args[0]
		if tag == "publish" {
			return publish()
		}
	default:
		return fmt.Errorf("usage: go run ./scripts [version|publish]")
	}
	version := strings.TrimPrefix(tag, "v")
	if !fullVersion.MatchString(version) {
		return fmt.Errorf("invalid release version %q", tag)
	}
	_, err := buildPackages(version)
	return err
}

func buildPackages(version string) ([]string, error) {
	for _, dir := range []string{"dist", "packages"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	var sums strings.Builder
	var assets []string
	err := withPatchedSFTP(func(workspace string) error {
		for _, target := range []string{"linux", "windows"} {
			archive, checksum, err := build(version, target, workspace)
			if err != nil {
				return err
			}
			assets = append(assets, archive)
			sums.WriteString(checksum)
			fmt.Println(archive)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	manifest := filepath.Join("packages", "SHA256SUMS")
	if err := os.WriteFile(manifest, []byte(sums.String()), 0644); err != nil {
		return nil, err
	}
	return append(assets, manifest), nil
}

func build(version, target, workspace string) (string, string, error) {
	name, ext := "sshore", ".tar.gz"
	if target == "windows" {
		name, ext = "sshore.exe", ".zip"
	}
	binary := filepath.Join("dist", name)
	cmd := workspaceGo(workspace, "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", binary, ".")
	cmd.Env = append(cmd.Env, "CGO_ENABLED=0", "GOARCH=amd64", "GOOS="+target)
	if err := cmd.Run(); err != nil {
		return "", "", err
	}
	archive := filepath.Join("packages", "sshore_"+version+"_"+target+"_amd64"+ext)
	f, err := os.Create(archive)
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	err = pack(io.MultiWriter(f, hash), binary, target)
	closeErr := f.Close()
	if err != nil {
		return "", "", err
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	return archive, fmt.Sprintf("%x  %s\n", hash.Sum(nil), filepath.Base(archive)), nil
}

func pack(w io.Writer, binary, target string) error {
	files := []string{binary, "LICENSE", "THIRD_PARTY_LICENSES.txt"}
	if target == "windows" {
		z := zip.NewWriter(w)
		for _, path := range files {
			entry, err := z.Create(filepath.Base(path))
			if err != nil {
				return err
			}
			if err := copyFile(entry, path); err != nil {
				return err
			}
		}
		return z.Close()
	}
	gz := gzip.NewWriter(w)
	t := tar.NewWriter(gz)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode := int64(0644)
		if path == binary {
			mode = 0755
		}
		if err := t.WriteHeader(&tar.Header{Name: filepath.Base(path), Mode: mode, Size: info.Size()}); err != nil {
			return err
		}
		if err := copyFile(t, path); err != nil {
			return err
		}
	}
	if err := t.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func copyFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
