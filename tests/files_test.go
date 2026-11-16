//go:build sshoretest

package tests_test

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/files"
)

func TestTransferSFTPScope(t *testing.T) {
	dir := t.TempDir()
	backend := files.NewTargetFS(filepath.Join(dir, "target"), 2)
	a, b := net.Pipe()
	server := sftp.NewRequestServer(a, sftp.Handlers{FileGet: backend, FilePut: backend, FileCmd: backend, FileList: backend})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve() }()
	client, err := files.NewSFTP(b, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(); client.Close(); server.Close(); <-done })
	if _, err = client.Open(filepath.ToSlash(filepath.Join(dir, "outside"))); err == nil {
		t.Fatal("arbitrary read allowed")
	}
	if _, err = client.Create("/outside"); err == nil {
		t.Fatal("arbitrary write allowed")
	}
	file, err := client.OpenFile("/.sshore-part", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/target"); err == nil {
		t.Fatal("partial upload published")
	}
	if _, err = file.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = client.Link("/.sshore-part", "/target"); err == nil {
		t.Fatal("unverified publication allowed")
	}
	if err = client.SetExtendedData("/.sshore-part", []sftp.StatExtended{{ExtType: "sha256@sshore", ExtData: fmt.Sprintf("2:%x", sha256.Sum256([]byte("xy")))}}); err != nil {
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
