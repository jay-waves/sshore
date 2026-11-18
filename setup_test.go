package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
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

type stubSetupInput struct {
	answers  []string
	closeErr error
	closed   bool
}

func (s *stubSetupInput) Read(string, bool) (string, error) {
	if len(s.answers) == 0 {
		return "", io.EOF
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	return answer, nil
}

func (s *stubSetupInput) Close() error { s.closed = true; return s.closeErr }

func TestSetupConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, choice, want string
		answers            []string
		closeErr           error
	}{
		{name: "update", choice: "update", answers: []string{"update"}, want: "update"},
		{name: "enter", choice: "update", answers: []string{""}, want: "cancel"},
		{name: "retry", choice: "update", answers: []string{"yes", "update"}, want: "update"},
		{name: "cancel", choice: "update"},
		{name: "restore-failure", choice: "update", answers: []string{"update"}, closeErr: errors.New("restore failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming, outgoing bytes.Buffer
			for _, m := range []message{
				{Type: "setup_choice", SoftwareVersion: version, Action: tc.choice},
				{Type: "setup_ready", SoftwareVersion: version, Action: "managed"},
			} {
				if err := writeMessage(&incoming, m); err != nil {
					t.Fatal(err)
				}
			}
			input := &stubSetupInput{answers: tc.answers, closeErr: tc.closeErr}
			err := negotiateSetupUsing(context.Background(), &incoming, &outgoing, &options{}, setupClientUI{Choose: func(m message) (string, error) {
				return chooseSetupAction(m, func() (setupInput, error) { return input, nil })
			}})
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Fatalf("lost terminal restoration error: %v", err)
			}
			if !input.closed {
				t.Fatal("input not closed before handoff")
			}
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "no update requested") || outgoing.Len() != 0 {
					t.Fatalf("failed input sent action: %v, %q", err, outgoing.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m, err := readMessage(&outgoing)
			if err != nil || m.Type != "setup_action" || m.Action != tc.want {
				t.Fatalf("action: %+v, %v", m, err)
			}
		})
	}
}

func TestSetupRejectsRecoveryMode(t *testing.T) {
	var stream bytes.Buffer
	writeMessage(&stream, message{Type: "setup_ready", SoftwareVersion: version, Action: "plain"})
	if err := negotiateSetup(&stream, &bytes.Buffer{}, &options{}); err == nil {
		t.Fatal("obsolete recovery mode accepted")
	}
}

func TestAttachSessionSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  []string
		want     string
		closeErr error
	}{
		{name: "choose", answers: []string{"#0", "#3", "#2"}, want: "eevee"},
		{name: "name", answers: []string{"eevee"}, want: "eevee"},
		{name: "filter", answers: []string{"/EEV", "#1", "#2"}, want: "eevee"},
		{name: "filter-no-auto-attach", answers: []string{"/eev", ""}},
		{name: "clear-filter", answers: []string{"/missing", "/", "pikachu"}, want: "pikachu"},
		{name: "cancel", answers: []string{""}},
		{name: "eof"},
		{name: "eof-and-restore", closeErr: errors.New("restore failed")},
		{name: "cancel-and-restore", answers: []string{""}, closeErr: errors.New("restore failed")},
		{name: "restore", answers: []string{"#1"}, closeErr: errors.New("restore failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming, outgoing bytes.Buffer
			writeMessage(&incoming, message{Type: "session_choice", SoftwareVersion: version, Names: []string{"pikachu", "eevee"}})
			writeMessage(&incoming, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"})
			input := &stubSetupInput{answers: tc.answers, closeErr: tc.closeErr}
			err := negotiateSetupWithInput(&incoming, &outgoing, &options{command: "attach"}, func() (setupInput, error) { return input, nil })
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Fatalf("lost terminal restoration error: %v", err)
			}
			if !input.closed {
				t.Fatal("input left open")
			}
			if tc.want == "" {
				if err == nil || outgoing.Len() != 0 {
					t.Fatalf("cancelled selection sent: %v", err)
				}
				return
			}
			m, readErr := readMessage(&outgoing)
			if err != nil || readErr != nil || m.Type != "session_select" || m.Name != tc.want {
				t.Fatal(m, err, readErr)
			}
		})
	}
}

func TestSessionSelectionNumericNames(t *testing.T) {
	for _, tc := range []struct{ answer, want string }{{"2", "2"}, {"#2", "eevee"}} {
		selected, err := chooseSetupSession(&stubSetupInput{answers: []string{tc.answer}}, []string{"2", "eevee"})
		if err != nil || selected != tc.want {
			t.Fatal(selected, err)
		}
	}
}

func TestReleaseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.4.9", false}, {"0.4.9", "0.4.0", false}, {"0.4.0", "0.4.0", true},
		{"0.4.0", "0.5.0", false}, {"1.4.0", "0.4.0", false},
		{"0.4.0", "0.40.0", false}, {"0.4.0", "0.4.00", false},
		{"0.4.0", "0.4.1-rc1", false}, {"0.4.0", "", false},
		{"0.4.1-dev-a", "0.4.1-dev-a", false}, {"0.4.1-dev-a", "0.4.1-dev-b", false},
		{"unknown", "unknown", false}, {"0.4", "0.4", false}, {"0.4.00", "0.4.00", false},
	} {
		if got := sameRelease(tc.a, tc.b); got != tc.want {
			t.Fatalf("%s / %s: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		order int
		known bool
	}{
		{"0.4.0", "0.4.0", 0, true}, {"0.4.1", "0.4.0", 1, true}, {"0.4.0", "0.4.1", -1, true},
		{"0.10.0", "0.9.9", 1, true}, {"2.0.0", "1.99.99", 1, true}, {"0.4.0-dev", "0.4.0", 0, false},
		{"unknown", "unknown", 0, false}, {"0.4.0-dev", "0.4.0-dev", 0, false}, {"0.4", "0.4", 0, false},
	} {
		got, known := compareReleaseVersions(tc.a, tc.b)
		if got != tc.order || known != tc.known {
			t.Fatal(tc, got, known)
		}
	}
}

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

func TestSetupDownloadNegotiation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var incoming, outgoing bytes.Buffer
		writeMessage(&incoming, message{Type: "setup_download", SoftwareVersion: version, Action: "linux-amd64"})
		writeMessage(&incoming, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"})
		err := negotiateSetupUsing(context.Background(), &incoming, &outgoing, &options{command: "setup"}, setupClientUI{Log: io.Discard, Download: func(context.Context) ([]byte, error) {
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
	action, err := chooseSetupAction(message{Type: "setup_choice", Action: "update"}, func() (setupInput, error) { return input, nil })
	if err != nil || action != "cancel" || !input.closed {
		t.Fatalf("%q %v", action, err)
	}
	_, err = chooseSetupAction(message{Type: "setup_choice", Action: "manual-only"}, func() (setupInput, error) { t.Fatal("unexpected update prompt"); return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "update manually") {
		t.Fatal(err)
	}
}

func TestSetupDownloadRetriesTruncatedBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "partial")
			return
		}
		_, _ = io.WriteString(w, "complete")
	}))
	defer server.Close()
	data, err := downloadSetupFile(context.Background(), server.Client(), server.URL, 64)
	if err != nil || string(data) != "complete" || calls.Load() != 2 {
		t.Fatalf("download %q: %v, requests=%d", data, err, calls.Load())
	}
}

func TestSetupDownloadDoesNotRetryOversizedBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "too large")
	}))
	defer server.Close()
	if _, err := downloadSetupFile(context.Background(), server.Client(), server.URL, 3); err == nil || calls.Load() != 1 {
		t.Fatalf("oversized response: %v, requests=%d", err, calls.Load())
	}
}
