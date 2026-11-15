package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func setupArchive(t *testing.T, name string, kind byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	data := []byte("verified executable fixture\x00\n")
	size := int64(len(data))
	if kind != tar.TypeReg {
		size = 0
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Size: size, Mode: 0755, Linkname: "/other"}); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSetupDownloadCache(t *testing.T) {
	archive := setupArchive(t, "sshore", tar.TypeReg)
	name := "sshore_" + version + "_linux_amd64.tar.gz"
	manifest := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), name)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v" + version + "/SHA256SUMS":
			io.WriteString(w, manifest)
		case "/v" + version + "/" + name:
			w.Write(archive)
		default:
			t.Errorf("unexpected release path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cache := t.TempDir()
	for i := 0; i < 2; i++ {
		binary, err := fetchSetupBinary(context.Background(), server.Client(), server.URL, cache, version)
		if err != nil || string(binary) != "verified executable fixture\x00\n" {
			t.Fatalf("%q %v", binary, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("valid cache made %d requests", requests.Load())
	}
	if err := os.WriteFile(filepath.Join(cache, version, name), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchSetupBinary(context.Background(), server.Client(), server.URL, cache, version); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 {
		t.Fatalf("bad cache not replaced: %d requests", requests.Load())
	}
}

func TestSetupDownloadRejectsInvalidArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		kind       byte
		corrupt    bool
	}{
		{"checksum", "sshore", tar.TypeReg, true},
		{"missing", "README.md", tar.TypeReg, false},
		{"path traversal", "../sshore", tar.TypeReg, false},
		{"symlink", "sshore", tar.TypeSymlink, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := setupArchive(t, tc.path, tc.kind)
			manifest := []byte(fmt.Sprintf("%x  release.tar.gz\n", sha256.Sum256(archive)))
			if tc.corrupt {
				archive = append(archive, '!')
			}
			if _, err := verifiedSetupArchive(manifest, archive, "release.tar.gz"); err == nil {
				t.Fatal("invalid artifact accepted")
			}
		})
	}
}

func TestSetupTransferBoundary(t *testing.T) {
	data := []byte("binary\x00\nbody\xff")
	var source, destination bytes.Buffer
	if err := sendSetupBinary(&source, data); err != nil {
		t.Fatal(err)
	}
	writeMessage(&source, message{Type: "setup_action", Action: "update"})
	if err := relaySetupBinary(&destination, &source); err != nil {
		t.Fatal(err)
	}
	next, err := readMessage(&source)
	if err != nil || next.Type != "setup_action" {
		t.Fatalf("relay consumed next frame: %+v %v", next, err)
	}
	if !bytes.HasSuffix(destination.Bytes(), data) {
		t.Fatal("relay changed bytes")
	}
	for _, header := range []string{"-1 bad\n", "67108865 " + strings.Repeat("0", 64) + "\n", "1 not-a-hash\n"} {
		if err := relaySetupBinary(io.Discard, strings.NewReader(header)); err == nil {
			t.Fatalf("accepted %q", header)
		}
	}
}

func TestSetupDownloadNegotiation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var incoming, outgoing bytes.Buffer
		writeMessage(&incoming, message{Type: "setup_download", SoftwareVersion: version, Action: "linux-amd64"})
		writeMessage(&incoming, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"})
		err := negotiateSetupUsing(context.Background(), &incoming, &outgoing, &options{}, setupClientUI{Log: io.Discard, Download: func(context.Context) ([]byte, error) {
			if fail {
				return nil, fmt.Errorf("offline")
			}
			return []byte("payload"), nil
		}, OpenInput: func() (setupInput, error) { t.Fatal("download requested confirmation"); return nil, nil }})
		if fail {
			if err == nil || !strings.Contains(err.Error(), "no shells stopped") || outgoing.String() != "0 -\n" {
				t.Fatalf("%q %v", outgoing.String(), err)
			}
		} else if err != nil || !bytes.HasSuffix(outgoing.Bytes(), []byte("payload")) {
			t.Fatalf("%q %v", outgoing.String(), err)
		}
	}
}

func TestMountSetupDeclineAndUnknownVersion(t *testing.T) {
	input := &stubSetupInput{answers: []string{""}}
	action, err := chooseSetupAction(message{Type: "setup_choice", Action: "update"}, &options{command: "mount-agent"}, func() (setupInput, error) { return input, nil })
	if err != nil || action != "cancel" || !input.closed {
		t.Fatalf("%q %v", action, err)
	}
	_, err = chooseSetupAction(message{Type: "setup_choice", Action: "manual-only"}, &options{command: "mount-agent", host: "host"}, func() (setupInput, error) { t.Fatal("unexpected update prompt"); return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "manual recovery") {
		t.Fatal(err)
	}
}
