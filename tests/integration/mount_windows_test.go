//go:build integration

package tests_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"golang.org/x/sys/windows"
)

func TestWindowsMount(t *testing.T) {
	if err := files.CheckMountPlatform(); err != nil {
		t.Skipf("requires WinFsp: %v", err)
	}

	t.Run("concurrent automatic mounts expose separate canonical paths and release their drives", func(t *testing.T) {
		source := t.TempDir()
		const name = "文档 with spaces.md"
		const original = "# Mounted document\n"
		if err := os.WriteFile(filepath.Join(source, name), []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		connecting := make(chan struct{}, 2)
		connect := make(chan struct{})
		// Both workers choose a candidate before their SFTP connections start.
		barrier := func(ctx context.Context) error {
			connecting <- struct{}{}
			select {
			case <-connect:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		readOnly := startWindowsMount(t, source, "", true, barrier)
		writable := startWindowsMount(t, source, "", false, barrier)
		for range 2 {
			select {
			case <-connecting:
			case <-time.After(8 * time.Second):
				t.Fatal("workers did not reach the connection barrier")
			}
		}
		close(connect)
		first, second := readOnly.point(t), writable.point(t)
		readonlyFile := filepath.Join(first+`\`, name)
		writableFile := filepath.Join(second+`\`, name)
		firstUNC, secondUNC := finalWindowsMountPath(t, readonlyFile), finalWindowsMountPath(t, writableFile)
		if strings.EqualFold(first, second) || strings.EqualFold(firstUNC, secondUNC) {
			t.Fatalf("mounts shared a drive or UNC path: %s and %s", first, second)
		}
		if err := files.ValidateMountpoint(strings.ToLower(first)); err == nil {
			t.Fatalf("accepted occupied drive %s as an explicit mountpoint", first)
		}
		if data, err := os.ReadFile(firstUNC); err != nil || string(data) != original {
			t.Fatalf("read canonical path: %q (%v)", data, err)
		}
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}
		sd, err := windows.GetNamedSecurityInfo(firstUNC, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		// UNC paths resolve outside the drive-letter namespace. Only the mounting
		// account and SYSTEM receive access, even for callers who know the UNC path.
		_, dacl, _ := strings.Cut(sd.String(), "D:")
		if want := "P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)"; dacl != want {
			t.Fatalf("unexpected UNC permissions: %s", sd.String())
		}
		if file, err := os.OpenFile(firstUNC, os.O_WRONLY, 0); err == nil {
			file.Close()
			t.Fatal("read-only UNC path allowed a write handle")
		}
		const added = "# New through canonical path\n"
		out := filepath.Join(filepath.Dir(secondUNC), "new.md")
		if err := os.WriteFile(out, []byte(added), 0600); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(source, "new.md")); err != nil || string(data) != added {
			t.Fatalf("write through canonical path: %q (%v)", data, err)
		}
		readOnly.stop()
		if _, err := os.Stat(first + `\`); !os.IsNotExist(err) {
			t.Fatalf("drive %s remained active after unmount: %v", first, err)
		}
		if data, err := os.ReadFile(secondUNC); err != nil || string(data) != original {
			t.Fatalf("second mount after unmounting the first: %q (%v)", data, err)
		}
	})

	t.Run("directory mounts can be read and remove their junction on unmount", func(t *testing.T) {
		source := t.TempDir()
		const name = "source.txt"
		if err := os.WriteFile(filepath.Join(source, name), []byte("directory mount"), 0600); err != nil {
			t.Fatal(err)
		}
		point := filepath.Join(t.TempDir(), "mounted")
		mount := startWindowsMount(t, source, point, true, nil)
		if data, err := os.ReadFile(filepath.Join(mount.point(t), name)); err != nil || string(data) != "directory mount" {
			t.Fatalf("read through directory mount: %q (%v)", data, err)
		}
		mount.stop()
		if _, err := os.Lstat(point); !os.IsNotExist(err) {
			t.Fatalf("junction remained after unmount: %v", err)
		}
	})
}

type windowsMountFixture struct {
	ready <-chan string
	done  <-chan struct{}
	err   error
	stop  func()
}

func startWindowsMount(t *testing.T, source, point string, readOnly bool, barrier func(context.Context) error) *windowsMountFixture {
	t.Helper()
	const version = "0.0.0"
	owner, done := make(chan struct{}), make(chan struct{})
	ready := make(chan string, 1)
	mount := &windowsMountFixture{ready: ready, done: done}
	mount.stop = sync.OnceFunc(func() {
		close(owner)
		select {
		case <-done:
			if mount.err != nil {
				t.Errorf("mount stopped with an error: %v", mount.err)
			}
		case <-time.After(12 * time.Second):
			t.Error("mount did not stop")
		}
	})
	t.Cleanup(mount.stop)
	go func() {
		defer close(done)
		_, mount.err = files.RunMount(files.MountConfig{
			Mountpoint: point, Remote: "~", Version: version, Name: "windows-mount",
			ReadOnly: readOnly, StartupTimeout: 10 * time.Second, IOTimeout: 5 * time.Second,
			Open: func(ctx context.Context) (io.ReadWriteCloser, error) {
				if barrier != nil {
					if err := barrier(ctx); err != nil {
						return nil, err
					}
				}
				client, server := net.Pipe()
				go func() {
					_ = files.ServeAgent(server, "mount_data", version, "fixture", "/"+filepath.ToSlash(source), nil, nil)
				}()
				return client, nil
			},
		}, files.MountLifecycle{OwnerDone: owner, Ready: func(_, point string) error {
			ready <- point
			return nil
		}})
	}()
	return mount
}

func (mount *windowsMountFixture) point(t *testing.T) string {
	t.Helper()
	select {
	case point := <-mount.ready:
		return point
	case <-mount.done:
		t.Fatalf("mount failed before readiness: %v", mount.err)
	case <-time.After(12 * time.Second):
		t.Fatal("mount did not become ready")
	}
	return ""
}

func finalWindowsMountPath(t *testing.T, name string) string {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var buffer [1024]uint16
	// Flags 0 ask for the normalized DOS path used by Windows canonicalization.
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0)
	if err != nil || n >= uint32(len(buffer)) {
		t.Fatalf("resolve final path of %s: length %d, error %v", name, n, err)
	}
	path := windows.UTF16ToString(buffer[:n])
	if !strings.HasPrefix(strings.ToLower(path), `\\?\unc\sshore\`) {
		t.Fatalf("expected a WinFsp UNC path, got %q", path)
	}
	return path
}
