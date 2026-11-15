package cp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSourceOpenDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "source")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	// Exercise the open after a preflight check, where the path can have changed.
	file, err := openSourceFile(link)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().IsRegular() {
		t.Fatal("source open followed symlink")
	}
}
