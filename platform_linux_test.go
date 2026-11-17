//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFTPDefaultMountParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "mnt")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	o, err := parseFTPOptions([]string{"alice@work:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(home, "state"))
	r, err := chooseTaskIdentity("mount", "")
	if err != nil {
		t.Fatal(err)
	}
	o.name = r.name
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if o.mountpoint != filepath.Join(root, o.name) {
		t.Fatalf("mountpoint: %s", o.mountpoint)
	}
	cleanup()
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0755)
	o.mountpoint = ""
	if _, err := prepareFTPPaths(&o); err == nil {
		t.Fatal("accepted other-user-writable mount parent")
	}
}

func TestPrivateStateRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "record")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := privateStatePath(link, false); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if err := privateStatePath(target, false); err == nil {
		t.Fatal("accepted public file")
	}
	if err := privateStatePath(dir, false); err == nil {
		t.Fatal("accepted directory as file")
	}
	t.Setenv("SSHORE_STATE_DIR", target)
	if _, err := stateDir("records"); err == nil {
		t.Fatal("accepted file as state root")
	}
}

func TestForwardInventoryOptional(t *testing.T) {
	for _, server := range []string{filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "different-release")} {
		if strings.HasSuffix(server, "different-release") {
			if e := os.WriteFile(server, []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 99\nprintf 'sshore 99.0.0\\n'\n"), 0700); e != nil {
				t.Fatal(e)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", forwardWatchCommand(server))
		cmd.Stdin = strings.NewReader("ignored metadata\n")
		b, e := cmd.Output()
		if e != nil || string(b) != forwardReadyMarker+"\n" {
			t.Fatalf("optional inventory broke transport: %q %v", b, e)
		}
	}
}

func TestRejectedTaskCleanupUsesSavedDirectory(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(t.TempDir(), "cleaned")
	config := filepath.Join(directory, "ssh.conf")
	if err := os.WriteFile(config, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\n[ -f ssh.conf ] || exit 1\nprintf done > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_SSH", ssh)
	removeRejectedPeerTask(requestRecord{Directory: t.TempDir(), Connection: recentConnection{Host: "work", Server: "sshore", Directory: directory, SSH: []string{"-F", "ssh.conf"}}, ID: strings.Repeat("a", 32)})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup did not use saved SSH working directory: %v", err)
	}
}
