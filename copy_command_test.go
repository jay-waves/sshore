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
)

func TestCopyParsing(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		upload bool
		host   string
		count  int
	}{
		{[]string{"a", "work:/tmp"}, true, "work", 1},
		{[]string{"a", "b", "work:/tmp"}, true, "work", 2},
		{[]string{"work:/tmp/a", "."}, false, "work", 1},
		{[]string{"work:/a", "work:/b", "origin:./out"}, false, "work", 2},
		{[]string{"origin:", "user@[::1]:/tmp"}, true, "user@::1", 1},
		{[]string{"work:/a", ":"}, false, "work", 1},
		{[]string{"origin:./a", "work:/b", "--", "-p2222"}, true, "work", 1},
	} {
		o, err := parseCopyCommand(tc.args)
		if err != nil || o.upload != tc.upload || o.host != tc.host || len(o.sources) != tc.count {
			t.Fatal(tc.args, o, err)
		}
	}
	for _, args := range [][]string{{"a"}, {"a", "b"}, {"a:/x", "b:/y"}, {":", ":"}, {"work:/a", "other:/b", "."}, {"a", ":", "work:/b"}, {"a", "work:/b", "--", "-L8080:localhost:80"}, {"a", "work:/b", "-p2222"}} {
		if _, err := parseCopyCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestCopyFiles(t *testing.T) {
	backend := testFTPBackend(t)
	local := t.TempDir()
	source := filepath.Join(local, "source")
	data := bytes.Repeat([]byte("copy data\x00\xff"), 10000)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.ToSlash(filepath.Join(backend.root, "renamed"))
	o, err := parseCopyCommand([]string{source, "work:" + remote})
	if err != nil {
		t.Fatal(err)
	}
	if err = executeCopy(backend.client, o); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(remote)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("upload mismatch", err)
	}
	target := filepath.Join(local, "received")
	o, err = parseCopyCommand([]string{"work:" + remote, target})
	if err != nil {
		t.Fatal(err)
	}
	if err = executeCopy(backend.client, o); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("download mismatch", err)
	}
	// The atomic publication itself refuses overwrite, independently of preflight.
	if err = copyUpload(backend.client, source, remote, int64(len(data)), nil); err == nil {
		t.Fatal("overwrote remote")
	}
	if err = copyDownload(backend.client, remote, target, int64(len(data)), nil); err == nil {
		t.Fatal("overwrote local")
	}
	got, _ = os.ReadFile(remote)
	if !bytes.Equal(got, data) {
		t.Fatal("remote changed")
	}
	entries, _ := os.ReadDir(backend.root)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sshore-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestCopyPreflight(t *testing.T) {
	backend := testFTPBackend(t)
	local := t.TempDir()
	a := filepath.Join(local, "a")
	if err := os.WriteFile(a, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := parseCopyCommand([]string{a, filepath.Join(local, "missing"), "work:" + backend.root})
	if err != nil {
		t.Fatal(err)
	}
	if err = executeCopy(backend.client, o); err == nil {
		t.Fatal("accepted missing source")
	}
	if _, err = os.Stat(filepath.Join(backend.root, "a")); !os.IsNotExist(err) {
		t.Fatal("wrote before preflight completed")
	}
	o, _ = parseCopyCommand([]string{a, a, "work:" + backend.root})
	if err = executeCopy(backend.client, o); err == nil {
		t.Fatal("accepted duplicate target")
	}
	if _, err = os.Stat(filepath.Join(backend.root, "a")); !os.IsNotExist(err) {
		t.Fatal("duplicate batch partly written")
	}
	o, _ = parseCopyCommand([]string{local, "work:" + backend.root})
	if err = executeCopy(backend.client, o); err == nil {
		t.Fatal("accepted directory source")
	}
}

func TestLocalEndpointHints(t *testing.T) {
	kind, paths, hint, err := remoteCopyRequest([]string{"origin:/client/source", "/tmp"})
	if err != nil || kind != "upload" || len(paths) != 1 || hint != "/client/source" {
		t.Fatal(kind, paths, hint, err)
	}
	kind, _, hint, err = remoteCopyRequest([]string{"report", "origin:./downloads"})
	if err != nil || kind != "download" || hint != "./downloads" {
		t.Fatal(kind, hint, err)
	}
	for _, args := range [][]string{{"origin:8080", "3000"}, {"3000", "origin:8080"}} {
		op, err := remoteForwardRequest(args)
		if err != nil || op.LocalHint != "8080" || !op.localPortPending() || op.serverPort() != 3000 {
			t.Fatal(op, err)
		}
		if err = validateOperations([]operation{op}); err != nil {
			t.Fatal(err)
		}
		if _, err = runClient(options{approved: []operation{op}}); err == nil {
			t.Fatal("hint authorized execution")
		}
		reverse := op.Reverse
		resolveLocalPort(&op, 9090)
		if op.LocalHint != "" || op.localPortPending() {
			t.Fatal("hint not cleared")
		}
		if reverse {
			if op.Port != 3000 || op.TargetPort != 9090 {
				t.Fatal(op)
			}
		} else {
			if op.Port != 9090 || op.TargetPort != 3000 {
				t.Fatal(op)
			}
		}
	}
	for _, args := range [][]string{{"origin:0", "3000"}, {"3000", "origin:65536"}, {"origin:8080", "origin:3000"}} {
		if _, err := remoteForwardRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestCopyStreams(t *testing.T) {
	backend := testFTPBackend(t)
	for _, data := range [][]byte{nil, bytes.Repeat([]byte("pipe\x00\xff\n"), 20000)} {
		target := filepath.ToSlash(filepath.Join(backend.root, "stream"))
		upload, err := parseCopyCommand([]string{"-", "work:" + target})
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
		download, err := parseCopyCommand([]string{"work:" + target, "-"})
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
	o, _ := parseCopyCommand([]string{"-", "work:" + backend.root + "/failed"})
	if err := executeStreamCopy(backend.client, o, io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(failure)), io.Discard); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(backend.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed stdin left files", entries, err)
	}
	for _, args := range [][]string{{"-", "a", "work:/tmp/file"}, {"work:/a", "work:/b", "-"}, {"-", "-"}, {"-", ":"}} {
		if _, err := parseCopyCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, literal := range []string{"./-", "origin:-", "work:-"} {
		e, err := parseCopyEndpoint(literal)
		if err != nil || e.stream {
			t.Fatal(literal, e, err)
		}
	}
	if _, _, _, err := remoteCopyRequest([]string{"-", "origin:"}); err == nil {
		t.Fatal("remote request accepted stdin")
	}
}
