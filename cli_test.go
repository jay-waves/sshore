package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	for _, topic := range []string{"upload", "download", "detach", "forward", "approve"} {
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

func TestSSHLightweightPassthrough(t *testing.T) {
	values := map[string]string{
		"-p": "2222", "-i": "key with spaces", "-J": "jump1,jump2", "-F": "ssh config",
		"-l": "alice", "-o": "ServerAliveInterval=15", "-L": "127.0.0.1:8080:localhost:80",
		"-R": "127.0.0.1:9000:localhost:3000", "-D": "127.0.0.1:1080", "-E": "debug log.txt",
	}
	for flag, value := range values {
		for _, args := range [][]string{{"host", flag, value}, {flag + value, "host"}} {
			o, err := parseOptions(args)
			if err != nil || !reflect.DeepEqual(o.ssh, []string{flag, value}) {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
			cmd := sshCommand(o)
			if !reflect.DeepEqual(cmd.Args[7:9], o.ssh) {
				t.Fatal(cmd.Args)
			}
		}
		if _, err := parseOptions([]string{"host", flag}); err == nil {
			t.Fatal("missing value accepted:", flag)
		}
	}
	flags := []string{"-C", "-vv", "-vvv", "-v", "-4", "-6"}
	o, err := parseOptions(append([]string{"host"}, flags...))
	if err != nil || !reflect.DeepEqual(o.ssh, flags) {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"host", "-R9000:localhost:3000", "-R", "9001:localhost:3001", "-D1080", "-D", "1081"})
	want := []string{"-R", "9000:localhost:3000", "-R", "9001:localhost:3001", "-D", "1080", "-D", "1081"}
	if err != nil || !reflect.DeepEqual(o.ssh, want) {
		t.Fatal(o, err)
	}
	for _, flag := range []string{"-N", "-n", "-f", "-t", "-tt", "-W", "-O", "-X"} {
		if _, err := parseOptions([]string{"host", flag}); err == nil {
			t.Fatal("unsupported SSH mode accepted:", flag)
		}
	}
}
