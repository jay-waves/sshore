package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jay-waves/sshore/internal/files"
)

func TestCLIOptionsAndHelp(t *testing.T) {
	o, err := parseOptions([]string{"launch", "host:", "--download-dir", "downloads", "--quiet", "--no-escape", "--", "-v", "-l", "user"})
	if err != nil || o.session != "" || o.downloadDir != "downloads" || !o.quiet || !o.noEscape || len(o.ssh) != 3 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{{"launch", "host:", "--help"}, {"attach", "--help"}} {
		if _, err := parseOptions(args); !errors.Is(err, errHelp) {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"list", "host", "--quiet"}, {"launch", "host:", "--session="}, {"launch", "--quiet=true"}, {"launch", "host:", "--unknown"}, {"launch", "bad host"}, {"launch", "host:", ""}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, topic := range []string{"launch", "list", "attach", "cp", "detach", "forward", "review", "kill", "mount", "daemon"} {
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
	automatic, err := parseOptions([]string{"launch", "host:"})
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
		for _, args := range [][]string{{"host:", "--", flag, value}, {"host:", "--", flag + value}} {
			o, err := parseOptions(append([]string{"launch"}, args...))
			if err != nil || !reflect.DeepEqual(o.ssh, []string{flag, value}) {
				t.Fatalf("%v: %+v %v", args, o, err)
			}
			connection := sshConnectionOptions(o)
			if !reflect.DeepEqual(connection.Args, o.ssh) {
				t.Fatal(connection.Args)
			}
		}
		if _, err := parseOptions([]string{"launch", "host:", "--", flag}); err == nil {
			t.Fatal("missing value accepted:", flag)
		}
	}
	flags := []string{"-C", "-vv", "-vvv", "-v", "-4", "-6"}
	o, err := parseOptions(append([]string{"launch", "host:", "--"}, flags...))
	if err != nil || !reflect.DeepEqual(o.ssh, flags) {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"launch", "host:", "--", "-R9000:localhost:3000", "-R", "9001:localhost:3001", "-D1080", "-D", "1081"})
	want := []string{"-R", "9000:localhost:3000", "-R", "9001:localhost:3001", "-D", "1080", "-D", "1081"}
	if err != nil || !reflect.DeepEqual(o.ssh, want) {
		t.Fatal(o, err)
	}
	for _, flag := range []string{"-N", "-n", "-f", "-t", "-tt", "-W", "-O", "-X"} {
		if _, err := parseOptions([]string{"launch", "host:", "--", flag}); err == nil {
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
	for _, tc := range []struct{ command, target, mode string }{{"launch", "host:", "new"}, {"attach", "host:build", "attach"}} {
		o, err := parseOptions([]string{tc.command, tc.target, "--", "-p2222"})
		if err != nil || o.command != tc.command || !strings.Contains(remoteCommand(o), shellQuote(tc.mode)) {
			t.Fatalf("%+v %v", o, err)
		}
	}
	for _, args := range [][]string{{"new"}, {"new", "host"}, {"host"}, {"host", "list"}, {"connect", "host"}, {"launch", "host:", "-s", "build"}, {"attach", "host", "--session=build"}, {"launch", "host:", "a", "b"}, {"list", "host", "--quiet"}} {
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
	if _, err := run([]string{"launch", "host:"}); err == nil {
		t.Fatal("recovery allowed shell")
	}
	if _, err := run([]string{"mount", "host:/remote", "mount"}); err == nil {
		t.Fatal("recovery allowed ftp")
	}
}

func TestRecoveryDiagnosticsStayLocal(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-run"))
	for _, args := range [][]string{{"ls", "--json"}, {"list", "--local", "--json"}, {"pstree"}} {
		if code, err := run(args); code != 0 || err != nil {
			t.Fatalf("local diagnostics %v: %d %v", args, code, err)
		}
	}
	if _, err := run([]string{"list", "remote", "--json"}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("recovery attempted remote diagnostics: %v", err)
	}
}

func TestSharedCLIGrammar(t *testing.T) {
	parsers := []struct {
		name  string
		args  []string
		parse func([]string) error
	}{
		{"shell", []string{"launch", "work:"}, func(a []string) error { _, err := parseOptions(a); return err }},
		{"mount", []string{"work:", "self:"}, func(a []string) error { _, err := parseFTPOptions(a); return err }},
		{"forward", []string{".:8080", "work:3000"}, func(a []string) error { _, _, err := parseForwardCommand(a); return err }},
		{"cp", []string{"work:source", ".:target"}, func(a []string) error { _, err := parseCopyCommand(a); return err }},
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
	mount, err := parseFTPOptions([]string{"work:", "self:", "--io-timeout=5s", "--startup-timeout=1m"})
	if err != nil || mount.name != "" || mount.ioTimeout.String() != "5s" || mount.startupTimeout.String() != "1m0s" {
		t.Fatalf("mount equals values: %+v %v", mount, err)
	}
	forward, _, err := parseForwardCommand([]string{".:8080", "work:3000", "--retries=2"})
	if err != nil || forward.Name != "" || forward.Retries != 2 {
		t.Fatalf("forward equals values: %+v %v", forward, err)
	}
	for _, args := range [][]string{
		{"work:", "self:", "--name=a"},
		{"work:", "self:", "--read-only=false"},
		{"work:", "self:", "--server", "--read-only"},
		{"work:", "self:", "--server="},
		{"work:", "self:", "--server=/bin/sshore"},
		{"work:", "self:", "--no-watch"},
	} {
		if _, err := parseFTPOptions(args); err == nil {
			t.Fatalf("accepted mount %v", args)
		}
	}
	for _, args := range [][]string{{".:8080", "work:3000", "--name=web"}, {".:8080", "work:3000", "--retries=1", "--retries=2"}, {".:8080", "work:3000", "--foreground=true"}} {
		if _, _, err := parseForwardCommand(args); err == nil {
			t.Fatalf("accepted forward %v", args)
		}
	}
	if _, err := parseReview([]string{"--stay", "--stay"}); err == nil {
		t.Fatal("accepted duplicate --stay")
	}
	for _, command := range []string{"list", "kill", "rename"} {
		if _, err := parseOptions([]string{command, "work", "--", "-L8080:localhost:80"}); err == nil {
			t.Fatalf("%s accepts unrelated forwarding option", command)
		}
	}
}

func TestShellListOptionsAndSSHBoundary(t *testing.T) {
	o, err := parseOptions([]string{"list", "host", "--json", "--", "-v"})
	if err != nil || !o.listJSON || len(o.ssh) != 1 || o.ssh[0] != "-v" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, flag := range []string{"-v", "--verbose", "--details", "--version", "--verbosal"} {
		if _, err := parseOptions([]string{"list", flag}); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}

func TestRemovedShellForms(t *testing.T) {
	for _, args := range [][]string{{"attach", "--local"}, {"attach", "--local", "dev"}, {"shell"}, {"launch", "--name", "dev"}, {"spawn", "--name=dev"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"shell"}, &out); err == nil {
		t.Fatal("accepted removed alias help")
	}
}

func TestCommandAliases(t *testing.T) {
	for alias, command := range map[string]string{"copy": "cp", "a": "attach", "d": "detach", "ps": "pstree", "ls": "list", "mnt": "mount", "fwd": "forward", "sh": "spawn", "mux": "launch"} {
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
	for _, alias := range []string{"a", "ls", "sh", "mux"} {
		target := "host"
		if alias == "sh" || alias == "mux" || alias == "a" {
			target += ":"
		}
		o, err := parseOptions([]string{alias, target})
		if err != nil || o.command != canonicalCommand(alias) {
			t.Fatalf("shell policy: %+v %v", o, err)
		}
	}
}

func TestSSHSeparatorBoundaries(t *testing.T) {
	parsers := map[string]func([]string) error{
		"session": func(a []string) error {
			_, e := parseOptions(append([]string{"launch", a[0] + ":"}, a[1:]...))
			return e
		},
		"mount": func(a []string) error {
			args := append([]string{a[0] + ":", "self:"}, a[1:]...)
			_, e := parseFTPOptions(args)
			return e
		},
		"forward": func(a []string) error {
			args := append([]string{".:8080", a[0] + ":3000"}, a[1:]...)
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
	uid := strings.Repeat("a", 32)
	if got := reconnectCommand(options{local: true, taskIdentity: "task:" + uid, session: "local-job"}); got != "sshore attach self:"+uid {
		t.Fatal(got)
	}

	o := options{host: "work", session: "dev", server: "/opt/sshore", ssh: []string{"-J", "jump"}}
	if got := reconnectCommand(o); got != "sshore attach work:dev --server /opt/sshore -- -J jump" {
		t.Fatal(got)
	}
	o.ssh = nil
	if got := reconnectCommand(o); got != "sshore attach work:dev --server /opt/sshore" {
		t.Fatal(got)
	}
}

func TestSharedHostValidation(t *testing.T) {
	for _, host := range []string{"host name", "host\tname", "host\x7f", "host\nname"} {
		if _, err := parseOptions([]string{"launch", host}); err == nil {
			t.Fatal("shell accepted", host)
		}
		if _, err := parseFTPOptions([]string{host + ":", "self:"}); err == nil {
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
		{"kill", "mount/demo", "--local"},
		{"kill", "forward/demo", "--", "-p22"},
		{"kill", "--all", "--local", "host"},
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

func TestNativeSSHPort(t *testing.T) {
	for _, tc := range []struct {
		command  string
		operands []string
	}{
		{"launch", []string{"host:"}}, {"spawn", []string{"host:"}}, {"attach", []string{"host", "work"}},
		{"list", []string{"host"}}, {"kill", []string{"host", "work"}},
		{"rename", []string{"host", "old", "new"}}, {"cp", []string{".:local", "host:remote"}},
		{"mount", []string{"host:remote", "self:"}}, {"forward", []string{".:8080", "host:80"}},
	} {
		args := append(append([]string(nil), tc.operands...), "--port=2222", "--", "-J", "jump")
		parsed, err := parseCLI(args, tc.command)
		if err != nil || !reflect.DeepEqual(parsed.ssh, []string{"-p", "2222", "-J", "jump"}) {
			t.Fatalf("%s: %+v %v", tc.command, parsed, err)
		}
	}
	for _, port := range []string{"1", "65535"} {
		if _, err := parseOptions([]string{"launch", "host:", "--port", port}); err != nil {
			t.Fatal(port, err)
		}
	}
	for _, args := range [][]string{
		{"host", "--port", "0"}, {"host", "--port", "65536"}, {"host", "--port", "-1"}, {"host", "--port", "+22"},
		{"host", "--port", "ssh"}, {"host", "--port", "22.0"}, {"host", "--port", "22\n"}, {"host", "--port"},
		{"host", "--port", "22", "--port", "23"}, {"--local", "--port", "22"}, {"--port", "22"},
		{"host", "--port", "22", "--", "-p", "23"}, {"host", "--port", "22", "--", "-p23"},
		{"host", "--port", "22", "--", "-o", "Port=23"}, {"host", "--port", "22", "--", "-o", " pOrT 23"},
	} {
		if _, err := parseOptions(append([]string{"launch"}, args...)); err == nil {
			t.Fatal("accepted", args)
		}
	}
	if _, err := parseOptions([]string{"launch", "host:", "--port", "22", "--", "-o", "ProxyCommand=ssh -p 23 jump"}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCLI([]string{"--port", "22"}, "review"); err == nil {
		t.Fatal("review accepted SSH port")
	}
	o, err := parseCopyCommand([]string{".:local", "host:remote", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(o.ssh, []string{"-p", "2222"}) {
		t.Fatal(o, err)
	}
	mounted, err := parseFTPOptions([]string{"host:remote", "self:", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(mounted.ssh, []string{"-p", "2222"}) {
		t.Fatal(mounted, err)
	}
}

func TestExecArguments(t *testing.T) {
	for _, command := range []string{"uname -a", "printf '%s' --help", "--help", "echo x -- -p 22"} {
		parsed, err := parseCLI([]string{"work", command}, "exec")
		if err != nil || len(parsed.operands) != 2 || parsed.operands[1] != command {
			t.Fatalf("command %q: %+v, %v", command, parsed, err)
		}
	}
	for _, args := range [][]string{nil, {"work"}, {"work", "echo", "extra"}, {"-host", "echo"}} {
		if _, err := parseCLI(args, "exec"); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestCancelCommandRemoved(t *testing.T) {
	for _, args := range [][]string{{"cancel"}, {"cancel", "--all"}, {"cancel", "--help"}} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("removed command accepted: %v: %d %v", args, code, err)
		}
	}
}

func TestSetupGrammar(t *testing.T) {
	for _, args := range [][]string{nil, {"work"}, {"--path", "/tmp/sshore"}, {"work", "--path", "/tmp/sshore", "--", "-p", "2222"}} {
		if _, err := parseCLI(args, "setup"); err != nil {
			t.Fatalf("rejected %q: %v", args, err)
		}
	}
	for _, args := range [][]string{{""}, {"work", "/tmp/sshore"}, {"--path"}, {"--path", ""}} {
		if _, err := parseCLI(args, "setup"); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	for _, args := range [][]string{{"bootstrap"}, {"setup", "--path", "relative"}, {"setup", "work", "--path", "relative"}, {"setup", "--", "-p", "2222"}} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("accepted %q: %d %v", args, code, err)
		}
	}
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	if code, err := run([]string{"setup", "--help"}); code != 0 || err != nil {
		t.Fatalf("help: %d %v", code, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("help changed state: %v", err)
	}
}

func TestSessionAge(t *testing.T) {
	if sessionAge(nil) != "" {
		t.Fatal("missing age displayed")
	}
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{-1, "0s"}, {0, "0s"}, {59, "59s"}, {60, "1m"}, {3599, "59m"},
		{3600, "1h"}, {86399, "23h"}, {86400, "1d"}, {172800, "2d"},
	} {
		if got := sessionAge(&tc.seconds); got != " · started "+tc.want+" ago" {
			t.Fatalf("%d: %q", tc.seconds, got)
		}
	}
}

func TestShortDirectory(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", "—"}, {"/", "/"}, {"/tmp", "/tmp"}, {"/home/me", "~"},
		{"/home/me/project", "~/project"}, {"/home/me/src/project", "~/src/project"},
		{"/home/me/work/src/project", "…/src/project"}, {"/var/lib/project", "…/lib/project"},
		{"/home/mean/project", "…/mean/project"}, {"/tmp/evil\x1b", "/tmp/evil�"},
	} {
		if got := shortDirectory(tc.path, "/home/me"); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := shortDirectory("/"+strings.Repeat("中", 100), ""); len([]rune(got)) != 40 {
		t.Fatal(got)
	}
}

func TestRequestPickerIdentityAndFilter(t *testing.T) {
	ids := []string{"first-id", "second-id"}
	labels := []string{"first-id · alpha · upload /tmp", "second-id · beta · download /tmp"}
	for _, tc := range []struct {
		answers []string
		want    string
	}{
		{[]string{"/beta", "#1", "#2"}, "second-id"},
		{[]string{"/beta", "/", "first-id"}, "first-id"},
		{[]string{""}, ""},
	} {
		got, err := chooseLocalItem(&stubSetupInput{answers: tc.answers}, ids, labels, "Request: ", "request")
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatal(got, err)
		}
	}
}

func TestApprovalPathRetry(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	validate := func(path string) (string, error) { return files.DownloadDestination(path, "new") }
	input := &stubSetupInput{answers: []string{existing, filepath.Join(dir, "missing", "new"), dir}}
	path, err := readApprovalPath(input, "Save as: ", "", validate)
	if err != nil || path != filepath.Join(dir, "new") {
		t.Fatal(path, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("validation created a file", err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "keep" {
		t.Fatal("validation overwrote a file")
	}
	// Defaults bypass input parsing, but still use native absolute-path normalization.
	defaultPath := filepath.Join(dir, " new with surrounding spaces ")
	want, err := filepath.Abs(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err = readApprovalPath(&stubSetupInput{answers: []string{""}}, "Save as: ", defaultPath, validate)
	if err != nil || path != want {
		t.Fatal(path, err)
	}
}

func TestApprovalPathCancellation(t *testing.T) {
	calls := 0
	_, err := readApprovalPath(&stubSetupInput{answers: []string{""}}, "File: ", "", func(path string) (string, error) { calls++; return path, nil })
	if err == nil || !strings.Contains(err.Error(), "remains pending") || !strings.Contains(err.Error(), io.EOF.Error()) || calls != 0 {
		t.Fatal(err, calls)
	}
}

func TestDisplayBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := displayBytes(n); got != want {
			t.Fatal(n, got, want)
		}
	}
}

func TestSelectedPort(t *testing.T) {
	for _, tc := range []struct {
		answers []string
		hint    string
		want    int
	}{
		{[]string{"", "0", "65536", "8080"}, "", 8080},
		{[]string{""}, "3000", 3000},
		{[]string{"8080"}, "3000", 8080},
	} {
		port, err := readSelectedPort(&stubSetupInput{answers: tc.answers}, "Local port", tc.hint, "cancelled")
		if err != nil || port != tc.want {
			t.Fatal(port, err)
		}
	}
	if _, err := readSelectedPort(&stubSetupInput{}, "Local port", "3000", "cancelled"); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatal("EOF accepted a suggestion", err)
	}
}

func TestCleanTerminalText(t *testing.T) {
	if got := cleanText("中文\x1b[31m\r\n"); got != "中文�[31m��" {
		t.Fatalf("unsafe terminal text: %q", got)
	}
}

func TestUnifiedEndpointSyntax(t *testing.T) {
	for _, tc := range []struct{ value, host, suffix string }{
		{"self:", "self", ""}, {".:x", "self", "x"}, {":x", "self", "x"},
		{"origin:", "origin", ""}, {"::8080", "origin", "8080"},
		{"work:/files/[name]", "work", "/files/[name]"},
		{"user@[::1]:3000", "user@::1", "3000"},
	} {
		host, suffix, err := splitEndpoint(tc.value)
		if err != nil || host != tc.host || suffix != tc.suffix {
			t.Fatalf("%q: %q %q %v", tc.value, host, suffix, err)
		}
	}
	for _, args := range [][]string{
		{"launch", "work"}, {"spawn", "work:build"}, {"attach", "7afe22"},
		{"cp", "file", "work:"}, {"fwd", "3000", "work:80"}, {"mnt", "work:", "./mnt"},
	} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, target := range []string{"self:", ".:", ":"} {
		o, err := parseOptions([]string{"launch", target})
		if err != nil || !o.local || o.session != "" {
			t.Fatalf("%q: %+v %v", target, o, err)
		}
	}
	if o, err := parseOptions([]string{"spawn", "work:"}); err != nil || o.host != "work" || o.session != "" {
		t.Fatal(o, err)
	}
	if o, err := parseOptions([]string{"attach", "work:7afe22"}); err != nil || o.host != "work" || o.session != "7afe22" {
		t.Fatal(o, err)
	}
	if o, err := parseOptions([]string{"rename", ".:7afe22", "build"}); err != nil || !o.local || o.newName != "build" {
		t.Fatal(o, err)
	}
	if o, err := parseCopyCommand([]string{".:report", "work:"}); err != nil || !o.upload || o.sources[0].path != "report" || o.target.path != "." {
		t.Fatal(o, err)
	}
	if o, err := parseFTPOptions([]string{"work:", "self:"}); err != nil || o.remote != "." || o.mountpoint != "" {
		t.Fatal(o, err)
	}
	if f, _, err := parseForwardCommand([]string{"work:3000", ".:8080"}); err != nil || !f.Reverse || f.Target != 8080 {
		t.Fatal(f, err)
	}
	if _, paths, hint, err := remoteCopyRequest([]string{".:/tmp/report", "::report"}); err != nil || !reflect.DeepEqual(paths, []string{"/tmp/report"}) || hint != "report" {
		t.Fatal(paths, hint, err)
	}
	if kind, paths, _, err := remoteCopyRequest([]string{"origin:", "self:"}); err != nil || kind != "upload" || !reflect.DeepEqual(paths, []string{"."}) {
		t.Fatal(kind, paths, err)
	}
	if op, err := remoteMountRequest([]string{"self:", "::"}); err != nil || op.Path != "." || op.LocalHint != "" {
		t.Fatal(op, err)
	}
	if op, err := remoteForwardRequest([]string{"origin:", ".:3000"}); err != nil || !op.LocalPortPending() || op.ServerPort() != 3000 {
		t.Fatal(op, err)
	}
}
