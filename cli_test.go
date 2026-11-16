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
	o, err := parseOptions([]string{"launch", "host", "build", "--download-dir", "downloads", "--quiet", "--no-escape", "--", "-v", "-l", "user"})
	if err != nil || o.session != "build" || o.downloadDir != "downloads" || !o.quiet || !o.noEscape || len(o.ssh) != 3 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{{"launch", "host", "--help"}, {"attach", "--help"}} {
		if _, err := parseOptions(args); !errors.Is(err, errHelp) {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"list", "host", "--quiet"}, {"launch", "host", "--session="}, {"launch", "--quiet=true"}, {"launch", "host", "--unknown"}, {"launch", "bad host"}, {"launch", "host", ""}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, topic := range []string{"launch", "list", "attach", "cp", "detach", "forward", "review", "cancel", "mount"} {
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
	automatic, err := parseOptions([]string{"launch", "host"})
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
		t.Fatal("list accepted a shell option")
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
			o, err := parseOptions(append([]string{"launch"}, args...))
			if err != nil || !reflect.DeepEqual(o.ssh, []string{flag, value}) {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
			cmd := sshCommand(o)
			if !reflect.DeepEqual(cmd.Args[7:9], o.ssh) {
				t.Fatal(cmd.Args)
			}
		}
		if _, err := parseOptions([]string{"launch", "host", "--", flag}); err == nil {
			t.Fatal("missing value accepted:", flag)
		}
	}
	flags := []string{"-C", "-vv", "-vvv", "-v", "-4", "-6"}
	o, err := parseOptions(append([]string{"launch", "host", "--"}, flags...))
	if err != nil || !reflect.DeepEqual(o.ssh, flags) {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"launch", "host", "--", "-R9000:localhost:3000", "-R", "9001:localhost:3001", "-D1080", "-D", "1081"})
	want := []string{"-R", "9000:localhost:3000", "-R", "9001:localhost:3001", "-D", "1080", "-D", "1081"}
	if err != nil || !reflect.DeepEqual(o.ssh, want) {
		t.Fatal(o, err)
	}
	for _, flag := range []string{"-N", "-n", "-f", "-t", "-tt", "-W", "-O", "-X"} {
		if _, err := parseOptions([]string{"launch", "host", "--", flag}); err == nil {
			t.Fatal("unsupported SSH mode accepted:", flag)
		}
	}
}

func TestExplicitSessionRouting(t *testing.T) {
	for _, host := range []string{"forward", "review", "help", "connect"} {
		_, err := run([]string{"launch", host, "--invalid"})
		if err == nil || !strings.Contains(err.Error(), "unsupported option: --invalid") {
			t.Fatalf("shell %s routed as a subcommand: %v", host, err)
		}
	}
	if _, err := run([]string{"launch"}); err == nil {
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
	for _, command := range []string{"launch", "attach"} {
		for _, args := range [][]string{{command, "host"}, {command, "host", "build"}, {command, "host", "build", "--", "-p2222"}} {
			o, err := parseOptions(args)
			if err != nil || o.command != command || o.list {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
			mode := command
			if command == "launch" {
				mode = "new"
			}
			if !strings.Contains(remoteCommand(o), shellQuote(mode)) {
				t.Fatalf("wrong remote action for %s", command)
			}
		}
	}
	for _, args := range [][]string{{"new"}, {"new", "host"}, {"host"}, {"host", "list"}, {"connect", "host"}, {"launch", "host", "-s", "build"}, {"attach", "host", "--session=build"}, {"launch", "host", "a", "b"}, {"list", "host", "--quiet"}} {
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
	if code, err := run([]string{"launch", "--help"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := run([]string{"launch", "host"}); err == nil {
		t.Fatal("recovery allowed shell")
	}
	if _, err := run([]string{"mount", "host:/remote", "mount"}); err == nil {
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

func TestCleanTerminalText(t *testing.T) {
	if got := cleanText("中文\x1b[31m\r\n"); got != "中文�[31m��" {
		t.Fatalf("unsafe terminal text: %q", got)
	}
}

func TestSharedCLIGrammar(t *testing.T) {
	parsers := []struct {
		name  string
		args  []string
		parse func([]string) error
	}{
		{"shell", []string{"launch", "work"}, func(a []string) error { _, err := parseOptions(a); return err }},
		{"mount", []string{"work:"}, func(a []string) error { _, err := parseFTPOptions(a); return err }},
		{"forward", []string{"8080", "work:3000"}, func(a []string) error { _, _, err := parseForwardCommand(a); return err }},
		{"cp", []string{"work:source", "target"}, func(a []string) error { _, err := parseCopyCommand(a); return err }},
		{"review", []string{"request"}, func(a []string) error { _, err := parseReview(a); return err }},
	}
	for _, parser := range parsers {
		t.Run(parser.name, func(t *testing.T) {
			for _, flag := range []string{"--help", "-h"} {
				if err := parser.parse(append(append([]string{}, parser.args...), flag)); !errors.Is(err, errHelp) {
					t.Fatalf("%s: %v", flag, err)
				}
			}
			if err := parser.parse(append(append([]string{}, parser.args...), "--unknown")); err == nil {
				t.Fatal("accepted unknown option")
			}
		})
	}
	mount, err := parseFTPOptions([]string{"work:", "--io-timeout=5s", "--startup-timeout=1m", "--server=/bin/sshore"})
	if err != nil || mount.name != "" || mount.ioTimeout.String() != "5s" || mount.startupTimeout.String() != "1m0s" || mount.server != "/bin/sshore" {
		t.Fatalf("mount equals values: %+v %v", mount, err)
	}
	forward, _, err := parseForwardCommand([]string{"8080", "work:3000", "--retries=2"})
	if err != nil || forward.Name != "" || forward.Retries != 2 {
		t.Fatalf("forward equals values: %+v %v", forward, err)
	}
	for _, args := range [][]string{
		{"work:", "--name=a"},
		{"work:", "--read-only=false"},
		{"work:", "--server", "--read-only"},
		{"work:", "--server="},
		{"work:", "--server=x", "--no-watch"},
	} {
		if _, err := parseFTPOptions(args); err == nil {
			t.Fatalf("accepted mount %v", args)
		}
	}
	for _, args := range [][]string{{"8080", "work:3000", "--name=web"}, {"8080", "work:3000", "--retries=1", "--retries=2"}, {"8080", "work:3000", "--foreground=true"}} {
		if _, _, err := parseForwardCommand(args); err == nil {
			t.Fatalf("accepted forward %v", args)
		}
	}
	if _, err := parseReview([]string{"--stay", "--stay"}); err == nil {
		t.Fatal("accepted duplicate --stay")
	}
	for _, command := range []string{"list", "cancel", "rename"} {
		if _, err := parseOptions([]string{command, "work", "--", "-L8080:localhost:80"}); err == nil {
			t.Fatalf("%s accepts unrelated forwarding option", command)
		}
	}
}

func TestPublicHelpBeforeExecution(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	t.Setenv("SSHORE_ASKPASS", "")
	for command := range commandHelp {
		if code, err := run([]string{command, "example", "--help"}); code != 0 || err != nil {
			t.Fatalf("%s help: %d %v", command, code, err)
		}
	}
	if code, err := run([]string{"shell", "--help"}); code == 0 || err == nil {
		t.Fatal("removed shell alias accepted")
	}
}

func TestShellStartupOptions(t *testing.T) {
	for _, command := range []string{"launch", "spawn"} {
		for _, flag := range []bool{false, true} {
			args := []string{command, "host"}
			if flag {
				args = append(args, "--no-login")
			}
			o, err := parseOptions(args)
			if err != nil || o.noLogin != flag {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
		}
	}
	for _, args := range [][]string{{"attach", "host", "--no-login"}, {"launch", "host", "--login"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := validateAttach(message{Type: "attach", Term: "xterm", Action: "existing", NoLogin: true}); err == nil {
		t.Fatal("accepted startup option on reattach")
	}
}

func TestNoProfileOptions(t *testing.T) {
	for _, command := range []string{"launch", "spawn"} {
		o, err := parseOptions([]string{command, "host", "--no-profile", "--no-login"})
		if err != nil || !o.noProfile || !o.noLogin {
			t.Fatalf("%+v %v", o, err)
		}
	}
	if _, err := parseOptions([]string{"attach", "host", "--no-profile"}); err == nil {
		t.Fatal("accepted creation flag on attach")
	}
	if err := validateAttach(message{Type: "attach", Term: "xterm", Action: "existing", NoProfile: true}); err == nil {
		t.Fatal("accepted profile flag on reattach")
	}
}

func TestShellVerboseAndSSHBoundary(t *testing.T) {
	o, err := parseOptions([]string{"list", "host", "--verbose", "--", "-v"})
	if err != nil || !o.listDetails || len(o.ssh) != 1 || o.ssh[0] != "-v" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, flag := range []string{"-v", "--details", "--version", "--verbosal"} {
		if _, err := parseOptions([]string{"list", flag}); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}

func TestLocalCommandRouting(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		local bool
		name  string
	}{
		{[]string{"launch"}, true, ""},
		{[]string{"launch", "--quiet", "--no-escape"}, true, ""},
		{[]string{"launch", "--local", "work"}, true, "work"},
		{[]string{"launch", "work"}, false, ""},
		{[]string{"launch", "host", "work"}, false, "work"},
		{[]string{"attach", "--local", "work"}, true, "work"},
		{[]string{"attach"}, false, ""},
		{[]string{"list", "--local"}, true, ""},
	} {
		o, err := parseOptions(tc.args)
		local := o.local
		if err != nil || local != tc.local || o.session != tc.name {
			t.Fatalf("%v: %+v %v %v", tc.args, o, local, err)
		}
	}
}

func TestLocalCommandRejectsAmbiguousOptions(t *testing.T) {
	for _, args := range [][]string{
		{"launch", "host", "--name", "work"},
		{"launch", "--name="},
		{"launch", "--local", "--server", "other"},
		{"attach", "--local", "../work"},
		{"cancel", "--local"},
		{"rename", "--local", "work"},
		{"list", "--local", "--json", "--verbose"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRemovedShellForms(t *testing.T) {
	for _, args := range [][]string{{"shell"}, {"launch", "--name", "dev"}, {"spawn", "--name=dev"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"shell"}, &out); err == nil {
		t.Fatal("accepted removed alias help")
	}
}

func TestRenameOptions(t *testing.T) {
	for _, args := range [][]string{{"rename", "host", "old", "new"}, {"rename", "host", "old", "new", "--", "-p22"}} {
		o, err := parseOptions(args)
		if err != nil || o.session != "old" || o.newName != "new" {
			t.Fatal(o, err)
		}
	}
	for _, args := range [][]string{{"rename", "host", "old"}, {"rename", "host", "old", "new", "extra"}, {"rename", "host", "old", "bad/name"}, {"rename", "host", "old", "new", "--quiet"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}

	o, err := parseOptions([]string{"rename", "12", "old", "new"})
	if err != nil || o.host != "12" || o.session != "old" || o.newName != "new" {
		t.Fatalf("numeric host: %+v %v", o, err)
	}

}

func TestCommandAliases(t *testing.T) {
	for alias, command := range map[string]string{"copy": "cp", "a": "attach", "d": "detach", "ps": "pstree", "ls": "list", "mnt": "mount", "fwd": "forward"} {
		t.Run(alias, func(t *testing.T) {
			var got, want bytes.Buffer
			if err := showHelp([]string{alias}, &got); err != nil {
				t.Fatal(err)
			}
			if err := showHelp([]string{command}, &want); err != nil {
				t.Fatal(err)
			}
			if got.String() != want.String() {
				t.Fatal("different help")
			}
			// Invalid arguments exercise dispatch without starting an operation.
			args := []string{alias, "--invalid-option"}
			code, err := run(args)
			wantCode, wantErr := run([]string{command, "--invalid-option"})
			if err == nil || wantErr == nil || code != wantCode || err.Error() != wantErr.Error() {
				t.Fatalf("dispatch: %d %v / %d %v", code, err, wantCode, wantErr)
			}
			if args[0] != alias {
				t.Fatal("dispatch modified caller arguments")
			}
		})
	}
	parsed, err := parseCLI([]string{"a", "d", "--", "-J", "ps"}, "copy")
	if err != nil || !reflect.DeepEqual(parsed.operands, []string{"a", "d"}) || !reflect.DeepEqual(parsed.ssh, []string{"-J", "ps"}) {
		t.Fatalf("operands rewritten: %+v %v", parsed, err)
	}
	for _, alias := range []string{"a", "ls"} {
		o, err := parseOptions([]string{alias, "host"})
		if err != nil || o.command != canonicalCommand(alias) {
			t.Fatalf("shell policy: %+v %v", o, err)
		}
	}
}

func TestEndpointRequests(t *testing.T) {
	for _, local := range []string{"origin:", ":"} {
		kind, paths, _, err := remoteCopyRequest([]string{local, "/tmp"})
		if err != nil || kind != "upload" || len(paths) != 1 || paths[0] != "/tmp" {
			t.Fatal(kind, paths, err)
		}
		kind, paths, _, err = remoteCopyRequest([]string{"a", "b", local})
		if err != nil || kind != "download" || len(paths) != 2 {
			t.Fatal(kind, paths, err)
		}
		for _, reverse := range []bool{false, true} {
			args := []string{local, "3000"}
			if reverse {
				args = []string{"3000", local}
			}
			op, err := remoteForwardRequest(args)
			if err != nil || !op.LocalPortPending() || op.ServerPort() != 3000 || op.Reverse != reverse {
				t.Fatal(op, err)
			}
		}
	}
	for _, args := range [][]string{{"origin:", "origin:"}, {"a", "local"}, {"a"}, {":", "a", "b"}} {
		if _, _, _, err := remoteCopyRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, args := range [][]string{{"local", "3000"}, {"3000", "8080"}, {"origin:", ":"}, {"origin:", "0"}, {"65536", ":"}} {
		if _, err := remoteForwardRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestForwardInvocation(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		reverse      bool
		port, target int
	}{
		{[]string{"origin:8080", "work:3000"}, false, 8080, 3000},
		{[]string{"work:3000", "origin:8080"}, true, 3000, 8080},
		{[]string{"8080", "work:3000"}, false, 8080, 3000},
	} {
		f, _, err := parseForwardCommand(tc.args)
		if err != nil || f.Host != "work" || f.Port != tc.port || f.Target != tc.target || f.Reverse != tc.reverse {
			t.Fatal(f, err)
		}
	}
}

func TestForwardRejectsInvalidLocalEndpoint(t *testing.T) {
	for _, args := range [][]string{{"origin:work:3000", "8080"}, {"origin:0", "work:3000"}, {"work:3000", "origin:65536"}, {"origin:3000", "origin:8080"}, {"3000", "8080"}} {
		if _, _, err := parseForwardCommand(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestEndpointHostValidation(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"work", "work"}, {"user@work", "user@work"}, {"[::1]", "::1"}, {"user@[::1]", "user@::1"},
	} {
		got, err := endpointHost(tc.value)
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	}
	for _, host := range []string{"", "-host", "host name", "host\tname", "host\x7f", "[broken]", "user@[]", "user@", "::1", "[::1"} {
		if _, err := endpointHost(host); err == nil {
			t.Fatal("accepted", host)
		}
	}
}

func TestSSHSeparatorBoundaries(t *testing.T) {
	parsers := map[string]func([]string) error{
		"session": func(a []string) error { _, e := parseOptions(append([]string{"launch"}, a...)); return e },
		"mount": func(a []string) error {
			args := append([]string{a[0] + ":"}, a[1:]...)
			_, e := parseFTPOptions(args)
			return e
		},
		"forward": func(a []string) error {
			args := append([]string{"8080", a[0] + ":3000"}, a[1:]...)
			_, _, e := parseForwardCommand(args)
			return e
		},
	}
	for name, parse := range parsers {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{{"work"}, {"work", "--", "-Jjump", "-p", "2222", "-o", "IdentityFile=key with spaces"}} {
				if e := parse(args); e != nil {
					t.Fatal(args, e)
				}
			}
			for _, args := range [][]string{
				{"work", "-J", "jump"}, {"work", "-p2222"}, {"work", "--"},
				{"work", "--", "-i"}, {"work", "--", "-i", "--"},
				{"work", "--", "--name", "web"}, {"work", "--", "other-host"},
				{"work", "--", "-J", "jump", "extra"}, {"work", "--", "--", "-v"},
				{"work", "--", "-f"}, {"work", "--", "-N"},
			} {
				if e := parse(args); e == nil {
					t.Fatal("accepted", args)
				}
			}
		})
	}
	for _, parse := range []func([]string) error{parsers["mount"], parsers["forward"]} {
		if e := parse([]string{"work", "--", "-D1080"}); e == nil {
			t.Fatal("resource accepted native forwarding")
		}
	}
}

func TestReconnectCommandSSHSeparator(t *testing.T) {
	o := options{host: "work", session: "dev", server: "/opt/sshore", ssh: []string{"-J", "jump"}}
	if got := reconnectCommand(o); got != "sshore attach work dev --server /opt/sshore -- -J jump" {
		t.Fatal(got)
	}
	o.ssh = nil
	if got := reconnectCommand(o); got != "sshore attach work dev --server /opt/sshore" {
		t.Fatal(got)
	}
}

func TestSharedHostValidation(t *testing.T) {
	for _, host := range []string{"host name", "host\tname", "host\x7f", "host\nname"} {
		if _, err := parseOptions([]string{"launch", host}); err == nil {
			t.Fatal("shell accepted", host)
		}
		if _, err := parseFTPOptions([]string{host + ":"}); err == nil {
			t.Fatal("mount accepted", host)
		}
		if _, err := parseCopyCommand([]string{"file", host + ":/tmp"}); err == nil {
			t.Fatal("copy accepted", host)
		}
		if _, _, err := parseForwardCommand([]string{"8080", host + ":3000"}); err == nil {
			t.Fatal("forward accepted", host)
		}
	}
}

func TestTargetDispatchRejectsInvalidArgumentsBeforeExecution(t *testing.T) {
	for _, args := range [][]string{
		{"cancel", "mount/demo", "--local"},
		{"cancel", "forward/demo", "--", "-p22"},
		{"cancel", "--all", "--local", "host"},
		{"attach", "host", "name", "extra"},
		{"rename", "host", "old"},
		{"review", "one", "two"},
	} {
		code, err := runTargetCommand(args)
		if code != 2 || err == nil {
			t.Fatalf("%v: code=%d err=%v", args, code, err)
		}
	}
}

func TestCancelAllScope(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		host     string
		all, bad bool
	}{
		{[]string{"--all"}, "", true, false},
		{[]string{"fedora", "--all"}, "fedora", true, false},
		{[]string{"--all", "fedora", "--", "-p", "2222"}, "fedora", true, false},
		{[]string{"--local", "--all"}, "", true, false},
		{[]string{"fedora", "shell", "--all"}, "", true, true},
		{[]string{"--all", "--all"}, "", true, true},
		{[]string{"--all", "--json"}, "", true, true},
		{[]string{"fedora", "shell", "--", "--all"}, "", false, false},
	} {
		o, all, err := parseCancelAll(tc.args)
		if all != tc.all || (err != nil) != tc.bad || (!tc.bad && all && o.host != tc.host) {
			t.Errorf("%v: host=%q all=%t err=%v", tc.args, o.host, all, err)
		}
	}
}

func TestListNumbersAreNotSelectors(t *testing.T) {
	for _, command := range []string{"attach", "cancel", "review"} {
		code, err := run([]string{command, "1"})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "display-only") {
			t.Fatalf("%s: %d %v", command, code, err)
		}
	}
}
