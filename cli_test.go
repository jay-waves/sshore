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
	o, err := parseOptions([]string{"new", "host", "build", "--download-dir", "downloads", "--quiet", "--no-escape", "--", "-v", "-l", "user"})
	if err != nil || o.session != "build" || o.downloadDir != "downloads" || !o.quiet || !o.noEscape || len(o.ssh) != 3 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{{"new", "host", "--help"}, {"attach", "--help"}} {
		if _, err := parseOptions(args); !errors.Is(err, errHelp) {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"list", "host", "--quiet"}, {"new", "host", "--session="}, {"new", "--quiet=true"}, {"new", "host", "--unknown"}, {"new", "bad host"}, {"new", "host", ""}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, topic := range []string{"new", "list", "attach", "upload", "download", "detach", "forward", "review", "cancel", "mount"} {
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
	automatic, err := parseOptions([]string{"new", "host"})
	if err != nil || automatic.session != "" {
		t.Fatalf("automatic: %+v %v", automatic, err)
	}
	named, err := parseOptions([]string{"attach", "host", "eevee"})
	if err != nil || named.session != "eevee" {
		t.Fatalf("resume: %+v %v", named, err)
	}
	listing, err := parseOptions([]string{"list", "host"})
	if err != nil || listing.session != "" {
		t.Fatalf("list: %+v %v", listing, err)
	}
	if _, err := parseOptions([]string{"list", "host", "main"}); err == nil {
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
		for _, args := range [][]string{{"host", "--", flag, value}, {"host", "--", flag + value}} {
			o, err := parseOptions(append([]string{"new"}, args...))
			if err != nil || !reflect.DeepEqual(o.ssh, []string{flag, value}) {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
			cmd := sshCommand(o)
			if !reflect.DeepEqual(cmd.Args[7:9], o.ssh) {
				t.Fatal(cmd.Args)
			}
		}
		if _, err := parseOptions([]string{"new", "host", "--", flag}); err == nil {
			t.Fatal("missing value accepted:", flag)
		}
	}
	flags := []string{"-C", "-vv", "-vvv", "-v", "-4", "-6"}
	o, err := parseOptions(append([]string{"new", "host", "--"}, flags...))
	if err != nil || !reflect.DeepEqual(o.ssh, flags) {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"new", "host", "--", "-R9000:localhost:3000", "-R", "9001:localhost:3001", "-D1080", "-D", "1081"})
	want := []string{"-R", "9000:localhost:3000", "-R", "9001:localhost:3001", "-D", "1080", "-D", "1081"}
	if err != nil || !reflect.DeepEqual(o.ssh, want) {
		t.Fatal(o, err)
	}
	for _, flag := range []string{"-N", "-n", "-f", "-t", "-tt", "-W", "-O", "-X"} {
		if _, err := parseOptions([]string{"new", "host", "--", flag}); err == nil {
			t.Fatal("unsupported SSH mode accepted:", flag)
		}
	}
}

func TestExplicitSessionRouting(t *testing.T) {
	for _, host := range []string{"forward", "review", "help", "connect"} {
		_, err := run([]string{"new", host, "--invalid"})
		if err == nil || !strings.Contains(err.Error(), "unexpected argument: --invalid") {
			t.Fatalf("new %s routed as a subcommand: %v", host, err)
		}
	}
	if _, err := run([]string{"new"}); err == nil {
		t.Fatal("missing host accepted")
	}
}

func TestSourceVersionMatchesRelease(t *testing.T) {
	data, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != version {
		t.Fatalf("VERSION=%s, executable=%s", got, version)
	}
}

func TestSessionCommandSemantics(t *testing.T) {
	for _, command := range []string{"new", "attach"} {
		for _, args := range [][]string{{command, "host"}, {command, "host", "build"}, {command, "host", "build", "--", "-p2222"}} {
			o, err := parseOptions(args)
			if err != nil || o.command != command || o.list {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
			if !strings.Contains(remoteCommand(o), shellQuote(command)) {
				t.Fatalf("wrong remote action for %s", command)
			}
		}
	}
	for _, args := range [][]string{{"host"}, {"host", "list"}, {"connect", "host"}, {"new", "host", "-s", "build"}, {"attach", "host", "--session=build"}, {"new", "host", "a", "b"}, {"list", "host", "--quiet"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted obsolete/invalid syntax: %v", args)
		}
	}
	for command := range commandHelp {
		for _, flag := range []string{"--help", "-h"} {
			if code, err := run([]string{command, flag}); code != 0 || err != nil {
				t.Fatalf("%s %s: %d %v", command, flag, code, err)
			}
		}
	}
	t.Setenv("SSHORE_DISABLED", "1")
	if code, err := run([]string{"new", "--help"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := run([]string{"new", "host"}); err == nil {
		t.Fatal("recovery allowed new")
	}
	if _, err := run([]string{"mount", "host", "/remote", "mount"}); err == nil {
		t.Fatal("recovery allowed ftp")
	}
}

func TestKillOptions(t *testing.T) {
	o, err := parseOptions([]string{"cancel", "host", "build", "--server", "/a b/sshore", "--", "-p", "2222"})
	if err != nil || o.command != "cancel" || o.session != "build" || o.list {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{
		{"cancel"}, {"cancel", "host"}, {"cancel", "host", "../other"},
		{"cancel", "host", "one", "two"}, {"cancel", "host", "one", "--quiet"},
		{"cancel", "host", "one", "--download-dir", "/tmp"}, {"cancel", "host", "one", "--no-escape"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"cancel"}, &out); err != nil || !strings.Contains(out.String(), "without confirmation") {
		t.Fatal(out.String(), err)
	}
}
