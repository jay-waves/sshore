package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func fixtureStateDir() (string, error) {
	root := os.Getenv("SSHORE_STATE_DIR")
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("CLI test fixture requires an absolute SSHORE_STATE_DIR")
	}
	return root, nil
}

func fixtureBinaryName() string {
	if runtime.GOOS == "windows" {
		return "sshore.exe"
	}
	return "sshore"
}

func TestMain(m *testing.M) {
	version = "0.0.0"
	daemonLocation = func() (string, error) {
		root, err := fixtureStateDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "daemon"), nil
	}
	resetCachePath = func() (string, error) {
		root, err := fixtureStateDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "release-cache"), nil
	}
	// Staging and replacement may rename the executable. The explicit
	// fixture environment follows it through those operations.
	if os.Getenv("SSHORE_TEST_CLI") == "1" &&
		(len(os.Args) == 1 || !strings.HasPrefix(os.Args[1], "-test.")) {
		if _, err := fixtureStateDir(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(os.Args) >= 4 && (os.Args[1] == "fixture-launch" || os.Args[1] == "fixture-spawn") {
			// Named fixtures let lifecycle tests locate processes independently of shell ID generation.
			target := os.Args[2] + ":"
			if os.Args[2] == "here:" {
				target = "here:"
			}
			o, err := parseOptions(append([]string{strings.TrimPrefix(os.Args[1], "fixture-"), target}, os.Args[4:]...))
			if err == nil {
				o.session = os.Args[3]
				code, runErr := withDaemonActivity(func() (int, error) { return executeShellCommand(o) })
				if runErr == nil {
					os.Exit(code)
				}
				err = runErr
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		main()
	}
	// Tests opt in to an isolated root instead of using the caller's setting.
	if err := os.Unsetenv("SSHORE_STATE_DIR"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if cliFixtureBinary.path != "" {
		os.RemoveAll(filepath.Dir(cliFixtureBinary.path))
	}
	os.Exit(code)
}

var cliFixtureBinary struct {
	sync.Once
	path string
	err  error
}

func testBinary(t *testing.T) string {
	t.Helper()
	t.Setenv("SSHORE_TEST_CLI", "1")
	cliFixtureBinary.Do(func() {
		cliFixtureBinary.path, cliFixtureBinary.err = copyFixtureBinary()
	})
	if cliFixtureBinary.err != nil {
		t.Fatal(cliFixtureBinary.err)
	}
	return cliFixtureBinary.path
}

func copyFixtureBinary() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	src, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dir, err := os.MkdirTemp("", "sshore-bin-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fixtureBinaryName())
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err == nil {
		_, err = io.Copy(dst, src)
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}
