package state

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if err := Private(link, false); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Private(target, false); err == nil {
		t.Fatal("accepted public file")
	}
	if err := Private(dir, false); err == nil {
		t.Fatal("accepted directory as file")
	}
	t.Setenv("SSHORE_STATE_DIR", target)
	if _, err := Dir("records"); err == nil {
		t.Fatal("accepted file as state root")
	}
}
