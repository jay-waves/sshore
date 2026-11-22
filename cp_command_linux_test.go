package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/tests/testutil"
	sftp "github.com/pkg/sftp/v2"
)

func TestCopyStreams(t *testing.T) {
	backend := testCopyCommandBackend(t)
	for _, data := range [][]byte{nil, bytes.Repeat([]byte("pipe\x00\xff\n"), 20000)} {
		target := filepath.ToSlash(filepath.Join(backend.root, "stream"))
		upload, err := parseCopyCommand([]string{".:-", "work:" + target})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = executeStreamCopy(backend.client, upload, bytes.NewReader(data), &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatal("upload polluted stdout")
		}
		download, err := parseCopyCommand([]string{"work:" + target, ".:-"})
		if err != nil {
			t.Fatal(err)
		}
		if err = executeStreamCopy(backend.client, download, nil, &out); err != nil || !bytes.Equal(out.Bytes(), data) {
			t.Fatal("stdout mismatch", err)
		}
		if err = executeStreamCopy(backend.client, upload, bytes.NewReader([]byte("overwrite")), &out); err == nil {
			t.Fatal("overwrote stream destination")
		}
		if len(data) > 0 {
			reader, writer := io.Pipe()
			reader.Close()
			err = executeStreamCopy(backend.client, download, nil, writer)
			writer.Close()
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal("stdout failure ignored", err)
			}
		}
		if err = os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("input interrupted")
	o, _ := parseCopyCommand([]string{".:-", "work:" + backend.root + "/failed"})
	if err := executeStreamCopy(backend.client, o, io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(failure)), io.Discard); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(backend.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed stdin left files", entries, err)
	}
}

func testCopyCommandBackend(t *testing.T) *struct {
	client *sftp.Client
	root   string
} {
	t.Helper()
	a, b := testutil.TCPPair(t)
	go func() {
		_ = files.ServeCopy(a)
		_ = a.Close()
	}()
	client, err := files.NewCopyClient(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); a.Close() })
	return &struct {
		client *sftp.Client
		root   string
	}{client, filepath.ToSlash(t.TempDir())}
}
