package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestBareFilePathsAndExplicitRemoteEndpoints(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		host   string
		upload bool
		paths  []string
		target string
	}{
		{[]string{"report", "work:"}, "work", true, []string{"report"}, "."},
		{[]string{"one", "two", "work:/dest"}, "work", true, []string{"one", "two"}, "/dest"},
		{[]string{"work:/one", "work:/two", "out"}, "work", false, []string{"/one", "/two"}, "out"},
		{[]string{`C:\Users\me\report`, "work:"}, "work", true, []string{`C:\Users\me\report`}, "."},
		{[]string{"user@[::1]:/file", "out"}, "user@::1", false, []string{"/file"}, "out"},
	} {
		o, err := parseCopyCommand(tc.args)
		if err != nil {
			t.Errorf("%q: %v", tc.args, err)
			continue
		}
		var paths []string
		for _, source := range o.sources {
			paths = append(paths, source.path)
		}
		if o.host != tc.host || o.upload != tc.upload || !reflect.DeepEqual(paths, tc.paths) || o.target.path != tc.target {
			t.Errorf("%q: got %+v, paths %q", tc.args, o, paths)
		}
	}
	for _, args := range [][]string{
		{"one", "two"}, {"work:a", "other:b", "out"}, {"work:a", "other:b"},
		{"origin:a", "out"}, {"work:a", "origin:b"},
	} {
		if _, err := parseCopyCommand(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	for _, args := range [][]string{{"work:/data", "mountpoint"}, {"user@[::1]:", `C:\mount`}} {
		o, err := parseFTPOptions(args)
		if err != nil || o.Mountpoint != args[1] {
			t.Errorf("%q: %+v %v", args, o, err)
		}
	}
	for _, args := range [][]string{{"data", "mountpoint"}, {"work:data", "other:mount"}, {"origin:data", "mountpoint"}} {
		if _, err := parseFTPOptions(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestOriginRequestsUseExecutionHostPaths(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		kind  string
		paths []string
		hint  string
	}{
		{[]string{"one", "two", "origin:/dest"}, "download", []string{"one", "two"}, "/dest"},
		{[]string{"origin:/file", "out"}, "upload", []string{"out"}, "/file"},
	} {
		kind, paths, hint, err := remoteCopyRequest(tc.args)
		if err != nil || kind != tc.kind || !reflect.DeepEqual(paths, tc.paths) || hint != tc.hint {
			t.Errorf("%q: %q %q %q %v", tc.args, kind, paths, hint, err)
		}
	}
	for _, args := range [][]string{{"one", "two"}, {"work:a", "origin:b"}, {"origin:a", "work:b"}, {"origin:a", "origin:b"}} {
		if _, _, _, err := remoteCopyRequest(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	op, err := remoteMountRequest([]string{"data", "origin:/mnt", "--read-only"})
	if err != nil || op.Path != "data" || op.LocalHint != "/mnt" || !op.ReadOnly {
		t.Fatalf("%+v %v", op, err)
	}
	for _, args := range [][]string{{"data", "mnt"}, {"work:data", "origin:mnt"}, {"data", "origin:mnt", "--port", "22"}} {
		if _, err := remoteMountRequest(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestShellSingleTargetGrammar(t *testing.T) {
	for _, command := range []string{"launch", "spawn"} {
		for _, tc := range []struct {
			target, host, name string
			local              bool
		}{
			{"", "local", "", true}, {"work", "work", "", false},
			{"work:build", "work", "build", false}, {":build", "local", "build", true},
			{"user@[::1]:build", "user@::1", "build", false},
		} {
			args := []string{command}
			if tc.target != "" {
				args = append(args, tc.target)
			}
			o, err := parseOptions(args)
			if err != nil || o.host != tc.host || o.session != tc.name || o.local != tc.local {
				t.Errorf("%q: %+v %v", args, o, err)
			}
		}
	}
}

func TestConsoleDoesNotOfferPing(t *testing.T) {
	complete := consoleCompleter(func() []sessionTarget { return nil })
	got, _ := complete.Do([]rune("ping"), 4)
	if len(got) != 0 || strings.Contains(consoleHelp, "ping") {
		t.Fatalf("console offers ping: %q, %q", got, consoleHelp)
	}
	if _, err := parseCLI([]string{"work:"}, "ping"); err != nil {
		t.Fatalf("standalone ping: %v", err)
	}
}

func TestAttachTargetAndSSHPort(t *testing.T) {
	for _, tc := range []struct {
		target, host, name string
		local              bool
	}{
		{"work", "work", "", false},
		{"work:build", "work", "build", false},
		{":build", "local", "build", true},
		{"user@[::1]:build", "user@::1", "build", false},
	} {
		o, err := parseOptions([]string{"attach", tc.target})
		if err != nil || o.host != tc.host || o.session != tc.name || o.local != tc.local {
			t.Errorf("%q: %+v %v", tc.target, o, err)
		}
	}
	o, err := parseCopyCommand([]string{"file", "work:", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(o.ssh, []string{"-p", "2222"}) {
		t.Fatalf("copy SSH port: %+v %v", o, err)
	}
	if _, err := parseCopyCommand([]string{"file", "work:", "--port", "2222", "--", "-p", "22"}); err == nil {
		t.Fatal("accepted conflicting SSH ports")
	}
}
