//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFTPDefaultMountParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "mnt")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	o, err := parseFTPOptions([]string{"alice@work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(home, "state"))
	r, err := reserveResourceName("mount", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
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
