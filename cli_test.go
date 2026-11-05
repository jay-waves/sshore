package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIOptionsAndHelp(t *testing.T) {
	o, err := parseOptions([]string{"--session=build", "host", "--download-dir", "downloads", "--quiet", "--no-escape", "-v", "-l", "user"})
	if err != nil || o.session != "build" || o.downloadDir != "downloads" || !o.quiet || !o.noEscape || len(o.ssh) != 3 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{{"host", "--help"}, {"--help"}} {
		if _, err := parseOptions(args); !errors.Is(err, errHelp) {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"host", "list", "--quiet"}, {"host", "--session="}, {"--quiet=true"}, {"host", "--unknown"}, {"bad host"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, topic := range []string{"upload", "download", "detach"} {
		var b bytes.Buffer
		if err = showHelp([]string{topic}, &b); err != nil || !strings.Contains(b.String(), "Usage: sshore "+topic) {
			t.Fatal(topic, err)
		}
	}
}
func TestDownloadDirectoryValidation(t *testing.T) {
	dir := t.TempDir()
	got, err := prepareDownloadDir(dir)
	if err != nil || !filepath.IsAbs(got) {
		t.Fatal(got, err)
	}
	f := filepath.Join(dir, "file")
	_ = os.WriteFile(f, nil, 0600)
	for _, p := range []string{f, filepath.Join(dir, "missing")} {
		if _, err = prepareDownloadDir(p); err == nil {
			t.Fatal("accepted", p)
		}
	}
}

func TestDefaultSessions(t *testing.T) {
	automatic, err := parseOptions([]string{"host"})
	if err != nil || automatic.session != "" {
		t.Fatalf("automatic: %+v %v", automatic, err)
	}
	named, err := parseOptions([]string{"host", "-s", "eevee"})
	if err != nil || named.session != "eevee" {
		t.Fatalf("resume: %+v %v", named, err)
	}
	listing, err := parseOptions([]string{"host", "list"})
	if err != nil || listing.session != "" {
		t.Fatalf("list: %+v %v", listing, err)
	}
	if _, err := parseOptions([]string{"host", "list", "-s", "main"}); err == nil {
		t.Fatal("list accepted a session option")
	}
}
