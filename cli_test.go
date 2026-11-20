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
	o, err := parseOptions([]string{"launch", "host:", "--quiet", "--no-escape", "--", "-v", "-l", "user"})
	if err != nil || o.session != "" || !o.quiet || !o.noEscape || len(o.ssh) != 3 {
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
func TestDefaultSessions(t *testing.T) {
	automatic, err := parseOptions([]string{"launch", "host:"})
	if err != nil || automatic.session != "" {
		t.Fatalf("automatic: %+v %v", automatic, err)
	}
	named, err := parseOptions([]string{"attach", "host:eevee"})
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
	if o, err := parseOptions([]string{"launch"}); err != nil || !o.local || o.session != "" {
		t.Fatalf("default launch: %+v, %v", o, err)
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
		if err != nil || o.command != tc.command || !strings.Contains(remoteCommand(o), `"mode":"`+tc.mode+`"`) {
			t.Fatalf("%+v %v", o, err)
		}
	}
	for _, args := range [][]string{{"new"}, {"new", "host"}, {"host"}, {"host", "list"}, {"connect", "host"}, {"launch", "host:", "-s", "build"}, {"attach", "host", "--session=build"}, {"launch", "host:", "a", "b"}, {"list", "host", "--quiet"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted obsolete/invalid syntax: %v", args)
		}
	}
	for command, spec := range cliCommands {
		if spec.help == "" {
			continue
		}
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
	for _, args := range [][]string{{"ls", "--json"}, {"list", "here:", "--json"}, {"pstree"}} {
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
		{"mount", []string{"work:", "here:"}, func(a []string) error { _, err := parseFTPOptions(a); return err }},
		{"forward", []string{".:8080", "work:3000"}, func(a []string) error { _, _, err := parseForwardCommand(a); return err }},
		{"cp", []string{"work:source", ".:target"}, func(a []string) error { _, err := parseCopyCommand(a); return err }},
		{"review", []string{"request"}, func(a []string) error { _, err := parseCLI(a, "review"); return err }},
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
	mount, err := parseFTPOptions([]string{"work:", "here:", "--io-timeout=5s", "--startup-timeout=1m"})
	if err != nil || mount.Name != "" || mount.IOTimeout.String() != "5s" || mount.StartupTimeout.String() != "1m0s" {
		t.Fatalf("mount equals values: %+v %v", mount, err)
	}
	forward, _, err := parseForwardCommand([]string{".:8080", "work:3000", "--retries=2"})
	if err != nil || forward.Name != "" || forward.Retries != 2 {
		t.Fatalf("forward equals values: %+v %v", forward, err)
	}
	for _, args := range [][]string{
		{"work:", "here:", "--name=a"},
		{"work:", "here:", "--read-only=false"},
		{"work:", "here:", "--server", "--read-only"},
		{"work:", "here:", "--server="},
		{"work:", "here:", "--server=/bin/sshore"},
		{"work:", "here:", "--no-watch"},
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

func TestRemovedScopeAndDownloadFlags(t *testing.T) {
	for command, spec := range cliCommands {
		for _, flag := range []string{"--local", "--download-dir"} {
			if _, err := parseCLI([]string{flag}, command); err == nil || !strings.Contains(err.Error(), flag) {
				t.Fatalf("%s accepted removed flag %s: %v", command, flag, err)
			}
			if strings.Contains(spec.help, flag) {
				t.Fatalf("%s still advertises %s", command, flag)
			}
		}
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
			args := append([]string{a[0] + ":", "here:"}, a[1:]...)
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
	if got := reconnectCommand(options{local: true, taskIdentity: "task:" + uid, session: "local-job"}); got != "sshore attach here:"+uid {
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
		if _, err := parseFTPOptions([]string{host + ":", "here:"}); err == nil {
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
		{"launch", []string{"host:"}}, {"spawn", []string{"host:"}}, {"attach", []string{"host:work"}},
		{"list", []string{"host"}}, {"kill", []string{"host:work"}},
		{"rename", []string{"host:old", "new"}}, {"cp", []string{".:local", "host:remote"}},
		{"mount", []string{"host:remote", "here:"}}, {"forward", []string{".:8080", "host:80"}},
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
	mounted, err := parseFTPOptions([]string{"host:remote", "here:", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(mounted.SSH, []string{"-p", "2222"}) {
		t.Fatal(mounted, err)
	}
}

func TestListHostEndpoint(t *testing.T) {
	for _, tc := range []struct {
		target, host string
		local        bool
	}{
		{"work:", "work", false},
		{"work", "work", false},
		{"user@[2001:db8::1]:", "user@2001:db8::1", false},
		{"2001:db8::1", "2001:db8::1", false},
		{"here:", "local", true},
	} {
		o, err := parseOptions([]string{"list", tc.target})
		if err != nil || o.host != tc.host || o.local != tc.local {
			t.Fatalf("%s: %+v, %v", tc.target, o, err)
		}
	}
	for _, args := range [][]string{{"work:path"}, {"origin:"}, {"here:", "--port", "2222"}} {
		if _, err := parseCLI(args, "list"); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestExecArguments(t *testing.T) {
	for _, command := range []string{"uname -a", "printf '%s' --help", "--help", "echo x -- -p 22"} {
		parsed, err := parseCLI([]string{"work:", command}, "exec")
		if err != nil || len(parsed.operands) != 2 || parsed.operands[0] != "work" || parsed.operands[1] != command {
			t.Fatalf("command %q: %+v, %v", command, parsed, err)
		}
	}
	for _, args := range [][]string{nil, {"work"}, {"work", "echo", "extra"}, {"-host", "echo"}, {"work:path", "echo"}, {"here:", "echo"}, {"origin:", "echo"}} {
		if _, err := parseCLI(args, "exec"); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestRemovedCommands(t *testing.T) {
	for _, args := range [][]string{{"cancel"}, {"cancel", "--all"}, {"cancel", "--help"}, {"mcp"}, {"mcp-agent"}} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("removed command accepted: %v: %d %v", args, code, err)
		}
	}
}

func TestInstallGrammar(t *testing.T) {
	for _, args := range [][]string{nil, {"work:"}, {".:/tmp/sshore"}, {"work:/tmp/sshore", "--", "-p", "2222"}} {
		if _, err := parseCLI(args, "install"); err != nil {
			t.Fatalf("rejected %q: %v", args, err)
		}
	}
	for _, args := range [][]string{{""}, {"work", "/tmp/sshore"}, {"--path"}, {"--path", ""}} {
		if _, err := parseCLI(args, "install"); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	for _, args := range [][]string{{"bootstrap"}, {"setup", "--apply", "--path", "/tmp/sshore"}, {"install", "work"}, {"install", "work:relative"}, {"install", "here:relative"}, {"install", "origin:"}, {"install", "work:/tmp/sshore", "--apply"}, {"install", "--", "-p", "2222"}} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("accepted %q: %d %v", args, code, err)
		}
	}
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("SSHORE_STATE_DIR", root)
	if code, err := run([]string{"install", "--help"}); code != 0 || err != nil {
		t.Fatalf("help: %d %v", code, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("help changed state: %v", err)
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
		i, err := chooseLocalItem(&stubSetupInput{answers: tc.answers}, ids, labels, "Request: ", "request")
		got := ""
		if err == nil {
			got = ids[i]
		}
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

func TestForwardPort(t *testing.T) {
	port, err := readForwardPort(&stubSetupInput{answers: []string{"", "0", "65536", "8080"}}, "Local port")
	if err != nil || port != 8080 {
		t.Fatal(port, err)
	}
	if _, err := readForwardPort(&stubSetupInput{}, "Local port"); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestCleanTerminalText(t *testing.T) {
	if got := cleanText("中文\x1b[31m\r\n"); got != "中文�[31m��" {
		t.Fatalf("unsafe terminal text: %q", got)
	}
}

func TestUnifiedEndpointSyntax(t *testing.T) {
	for _, tc := range []struct{ value, host, suffix string }{
		{"here:", "here", ""}, {".:x", "here", "x"}, {":x", "here", "x"},
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
		{"launch", ":work", "--port", "22"}, {"spawn", "origin:build"}, {"kill", "7afe22"},
		{"cp", "file", "dest"}, {"fwd", "3000", "80"}, {"mnt", "./src", "./mnt"},
	} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, target := range []string{"here:", ".:", ":"} {
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
	if o, err := parseFTPOptions([]string{"work:", "here:"}); err != nil || o.Remote != "." || o.Mountpoint != "" {
		t.Fatal(o, err)
	}
	if f, _, err := parseForwardCommand([]string{"work:3000", ".:8080"}); err != nil || !f.Reverse || f.Target != 8080 {
		t.Fatal(f, err)
	}
	if _, paths, hint, err := remoteCopyRequest([]string{".:/tmp/report", "::report"}); err != nil || !reflect.DeepEqual(paths, []string{"/tmp/report"}) || hint != "report" {
		t.Fatal(paths, hint, err)
	}
	if kind, paths, _, err := remoteCopyRequest([]string{"origin:", "here:"}); err != nil || kind != "upload" || !reflect.DeepEqual(paths, []string{"."}) {
		t.Fatal(kind, paths, err)
	}
	if op, err := remoteMountRequest([]string{"here:", "::"}); err != nil || op.Path != "." || op.LocalHint != "" {
		t.Fatal(op, err)
	}
	if op, err := remoteForwardRequest([]string{"origin:", ".:3000"}); err != nil || !op.LocalPortPending() || op.ServerPort() != 3000 {
		t.Fatal(op, err)
	}
}

func TestNamedShellCreationTargets(t *testing.T) {
	for _, tc := range []struct {
		command, target, host, name string
		local                       bool
	}{
		{"launch", "build", "build", "", false},
		{"spawn", "here:build", "local", "build", true},
		{"sh", ".:build", "local", "build", true},
		{"mux", "work:build", "work", "build", false},
		{"spawn", "user@[::1]:build", "user@::1", "build", false},
		{"launch", "work:", "work", "", false},
	} {
		o, err := parseOptions([]string{tc.command, tc.target})
		if err != nil || o.host != tc.host || o.session != tc.name || o.local != tc.local || o.command != canonicalCommand(tc.command) {
			t.Errorf("%s %s: %+v, %v", tc.command, tc.target, o, err)
		}
		if !tc.local && tc.name != "" && !strings.Contains(remoteCommand(o), `"name":"`+tc.name+`"`) {
			t.Errorf("%s %s did not carry the requested name to the server", tc.command, tc.target)
		}
	}
	for _, target := range []string{"origin:build", "work:abcdef", "here:bad/name"} {
		if _, err := parseOptions([]string{"spawn", target}); err == nil {
			t.Errorf("accepted invalid creation target %q", target)
		}
	}
}
