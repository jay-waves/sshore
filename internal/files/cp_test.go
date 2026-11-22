package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/jay-waves/sshore/tests/testutil"
	sftp "github.com/pkg/sftp/v2"
)

func TestExpandRemoteHome(t *testing.T) {
	for _, tc := range []struct{ name, home, want string }{
		{"~", "/home/user", "/home/user"},
		{"~/directory with spaces", "/home/user", "/home/user/directory with spaces"},
		{"~/file", "/", "/file"},
		{"~/link/../file", "/home/user", "/home/user/link/../file"},
		{"./~", "", "./~"},
		{"~user", "", "~user"},
		{`~\file`, "", `~\file`},
	} {
		got, err := ExpandRemoteHome(tc.name, tc.home)
		if got != tc.want || err != nil {
			t.Errorf("ExpandRemoteHome(%q, %q) = %q, %v; want %q", tc.name, tc.home, got, err, tc.want)
		}
	}
	for _, home := range []string{"", "relative"} {
		if _, err := ExpandRemoteHome("~", home); err == nil {
			t.Errorf("accepted remote home %q", home)
		}
	}
}

func TestFileRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 64*1024*3 + 11} {
		t.Run(string(rune('a'+size%26)), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "\u4e2d\u6587 source.bin")
			dst := filepath.Join(dir, "target.bin")
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i)
			}
			if err := os.WriteFile(src, data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := OpenSource(src)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := stageCopyFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer discardCopyFile(r)
			if err = CopyExactSize(r, s.file, s.size); err != nil {
				t.Fatal(err)
			}
			if err = publishCopyFile(context.Background(), r, dst, s.size); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dst)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("round trip failed", err)
			}
		})
	}
}
func TestFileFailureAndNoClobber(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "target")
	r, err := stageCopyFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteString("bad"); err != nil {
		t.Fatal(err)
	}
	if publishCopyFile(context.Background(), r, dst, 2) == nil {
		t.Fatal("accepted excess data")
	}
	discardCopyFile(r)
	r, err = stageCopyFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteString("o"); err != nil {
		t.Fatal(err)
	}
	if publishCopyFile(context.Background(), r, dst, 2) == nil {
		t.Fatal("accepted incomplete file")
	}
	discardCopyFile(r)
	if _, err = os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("published corrupt file")
	}
	r, err = stageCopyFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dst, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if publishCopyFile(context.Background(), r, dst, 0) == nil {
		t.Fatal("overwrote race-created file")
	}
	discardCopyFile(r)
	got, _ := os.ReadFile(dst)
	if string(got) != "keep" {
		t.Fatal("existing file changed")
	}
	if _, err = stageCopyFile(dst); err == nil {
		t.Fatal("accepted existing file")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
	for _, name := range []string{"../x", "a/b", `a\b`, "CON", "LPT1.txt", "COM¹", "com².txt", "LPT³.log", "a:b", "a\n", "..", "file."} {
		if ValidateName(name) == nil {
			t.Fatal("unsafe filename", name)
		}
	}
}

func TestCopyExactSize(t *testing.T) {
	for _, tc := range []struct {
		data  string
		size  int64
		valid bool
	}{
		{"", 0, true}, {"abc", 3, true}, {"abc", 2, false}, {"abc", 4, false}, {"", 1, false},
		{"", -1, false}, {"abc", 1<<63 - 1, false},
	} {
		var out bytes.Buffer
		err := CopyExactSize(&out, strings.NewReader(tc.data), tc.size)
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
		if tc.valid && out.String() != tc.data {
			t.Fatal(out.String())
		}
	}
}

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

func TestSFTPCopyIntegrity(t *testing.T) {
	backend := testCopyBackend(t)
	data := make([]byte, 3*1024*1024+17)
	for i := range data {
		data[i] = byte(i*31 + i/1024)
	}
	for _, size := range []int{0, 1, len(data)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			target := path.Join(backend.root, fmt.Sprint(size))
			if err := UploadReader(backend.client, bytes.NewReader(data[:size]), target, int64(size)); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			var progress atomic.Int64
			if err := DownloadTo(backend.client, target, &out, int64(size), &progress); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), data[:size]) || progress.Load() != int64(size) {
				t.Fatal("content or progress mismatch")
			}
		})
	}
	for _, size := range []int64{int64(len(data) - 1), int64(len(data) + 1)} {
		if err := UploadReader(backend.client, bytes.NewReader(data), path.Join(backend.root, "invalid"), size); err == nil {
			t.Fatal("accepted wrong upload size", size)
		}
	}
	failure := errors.New("source failed")
	if err := UploadReader(backend.client, io.MultiReader(bytes.NewReader(data), iotest.ErrReader(failure)), path.Join(backend.root, "failed"), -1); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(backend.localRoot)
	if err != nil || len(entries) != 3 {
		t.Fatal("failed upload published or leaked temporary files", entries, err)
	}
}

func TestSFTPDownloadSizeAndWriterFailures(t *testing.T) {
	backend := testCopyBackend(t)
	data := bytes.Repeat([]byte("content"), 10000)
	name := path.Join(backend.root, "source")
	if err := os.WriteFile(filepath.Join(backend.localRoot, "source"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{0, int64(len(data) - 1), int64(len(data) + 1)} {
		file, err := backend.client.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = copySFTPExact(&out, file, size, nil)
		file.Close()
		if err == nil || int64(out.Len()) > size {
			t.Fatal("accepted changed size or emitted excess data", size, out.Len(), err)
		}
	}
	failure := errors.New("destination failed")
	for _, tc := range []struct {
		writer io.Writer
		want   error
	}{
		{copyFailWriter{failure}, failure}, {copyFailWriter{}, io.ErrShortWrite},
	} {
		if err := DownloadTo(backend.client, name, tc.writer, int64(len(data)), nil); !errors.Is(err, tc.want) {
			t.Fatalf("writer failure: got %v, want %v", err, tc.want)
		}
	}
	// A failed pipeline must finish its workers without poisoning the connection.
	var out bytes.Buffer
	if err := DownloadTo(backend.client, name, &out, int64(len(data)), nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatal("connection unusable after failed copy", err)
	}
}

type copyFailWriter struct{ err error }

func (w copyFailWriter) Write(p []byte) (int, error) { return 0, w.err }

type copyTestBackend struct {
	client          *sftp.Client
	root, localRoot string
}

func testCopyBackend(t *testing.T) *copyTestBackend {
	t.Helper()
	root := t.TempDir()
	client := testTransferConnection(t, newCopyFS())
	return &copyTestBackend{client: client, root: filepath.ToSlash(root), localRoot: root}
}

func testTransferConnection(t *testing.T, backend *FS) *sftp.Client {
	t.Helper()
	a, b := testutil.TCPPair(t)
	done := make(chan struct{})
	go func() { defer close(done); _ = ServeTransfer(a, backend) }()
	t.Cleanup(func() { b.Close(); <-done })
	client, err := NewCopyClient(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestUploadReceiverVerification(t *testing.T) {
	backend := testCopyBackend(t)
	client := backend.client
	localRoot, root := backend.localRoot, backend.root
	data := bytes.Repeat([]byte("verified"), 10000)
	virtual := path.Join(root, ".sshore-corrupt.part")
	file, err := client.OpenFile(virtual, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	wrongDigest := sha256.Sum256([]byte("different"))
	if _, err := file.WriteAt(wrongDigest[:], 3); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep the corrupt upload registered while a second upload publishes.
	if err := UploadReader(client, bytes.NewReader(data), path.Join(root, "good"), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(localRoot, "good"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("published data mismatch", err)
	}
	if err = client.Link(virtual, path.Join(root, "bad")); err == nil {
		t.Fatal("published unverified file")
	}
	if err = client.Remove(virtual); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(localRoot)
	if err != nil || len(entries) != 1 {
		t.Fatal("verification failure leaked file", entries, err)
	}
}

func TestTransferSFTPScope(t *testing.T) {
	dir := t.TempDir()
	backend := NewTargetFS(filepath.Join(dir, "target"), 2)
	client := testTransferConnection(t, backend)
	var err error
	if _, err = client.Open(filepath.ToSlash(filepath.Join(dir, "outside"))); err == nil {
		t.Fatal("arbitrary read allowed")
	}
	if _, err = client.Create("/outside"); err == nil {
		t.Fatal("arbitrary write allowed")
	}
	file, err := client.OpenFile("/.sshore-part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(0); err == nil {
		t.Fatal("approved handle allowed arbitrary truncation")
	}
	if err = client.Link("/.sshore-part", "/target"); err == nil {
		t.Fatal("partial upload published")
	}
	if _, err = file.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("xy"))
	if _, err = file.WriteAt(sum[:], 2); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/outside"); err == nil {
		t.Fatal("arbitrary publication allowed")
	}
	if err = client.Link("/.sshore-part", "/target"); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/target"); err == nil {
		t.Fatal("overwrite allowed")
	}
	if err = client.Remove("/target"); err == nil {
		t.Fatal("published file removed")
	}
	data, err := os.ReadFile(filepath.Join(dir, "target"))
	if err != nil || string(data) != "xy" {
		t.Fatal(string(data), err)
	}
}
