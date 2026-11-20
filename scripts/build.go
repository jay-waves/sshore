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

func main() {
	var tag string
	switch len(os.Args) {
	case 1:
		cmd := exec.Command("git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*.[0-9]*.[0-9]*", "--exclude", "*-*", "--exclude", "*+*")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			log.Fatal(err)
		}
		tag = strings.TrimSpace(string(out))
	case 2:
		tag = os.Args[1]
	default:
		log.Fatal("usage: go run ./scripts [version]")
	}
	version := strings.TrimPrefix(tag, "v")
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*)){2}$`).MatchString(version) {
		log.Fatalf("invalid release version %q", tag)
	}
	for _, dir := range []string{"dist", "packages"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatal(err)
		}
	}
	for _, target := range []string{"linux", "windows"} {
		if err := build(version, target); err != nil {
			log.Fatal(err)
		}
	}
}

func build(version, target string) error {
	name, ext := "sshore", ".tar.gz"
	if target == "windows" {
		name, ext = "sshore.exe", ".zip"
	}
	binary := filepath.Join("dist", name)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", binary, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOARCH=amd64", "GOOS="+target)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	archive := filepath.Join("packages", "sshore_"+version+"_"+target+"_amd64"+ext)
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	hash := sha256.New()
	err = pack(io.MultiWriter(f, hash), binary, target)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sum := fmt.Sprintf("%x  %s\n", hash.Sum(nil), filepath.Base(archive))
	if err := os.WriteFile(archive+".sha256", []byte(sum), 0644); err != nil {
		return err
	}
	fmt.Println(archive)
	return nil
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
