package cp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSourceOpenDoesNotBlockOnReplacementFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() { file, err := openSourceFile(path); done <- result{file, err} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		defer r.file.Close()
		info, err := r.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			t.Fatal("FIFO classified as regular")
		}
	case <-time.After(time.Second):
		// Unblock a regressed blocking open before failing, avoiding a leaked reader.
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			unix.Close(fd)
		}
		select {
		case r := <-done:
			if r.file != nil {
				r.file.Close()
			}
		case <-time.After(time.Second):
		}
		t.Fatal("source open blocked on FIFO")
	}
}
