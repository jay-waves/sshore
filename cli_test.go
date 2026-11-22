package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/rogpeppe/go-internal/testscript"
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
	for _, topic := range []string{"launch", "list", "attach", "cp", "detach", "forward", "kill", "mount", "daemon"} {
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
	for _, args := range [][]string{{"ls", "--json"}, {"pstree"}} {
		if code, err := run(args); code != 0 || err != nil {
			t.Fatalf("local diagnostics %v: %d %v", args, code, err)
		}
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
	for _, command := range []string{"kill", "rename"} {
		if _, err := parseOptions([]string{command, "work", "--", "-L8080:localhost:80"}); err == nil {
			t.Fatalf("%s accepts unrelated forwarding option", command)
		}
	}
}

func TestInventoryOptions(t *testing.T) {
	parsed, err := parseCLI([]string{"--json"}, "list")
	if err != nil || !parsed.has("--json") {
		t.Fatalf("%+v %v", parsed, err)
	}
	for _, args := range [][]string{
		{"list", "-v"}, {"list", "--verbose"}, {"list", "--version"},
		{"ls", "--", "-L8080:localhost:80"}, {"ps", "--json"}, {"ps", "work:"},
	} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
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
	for _, args := range [][]string{{"a", "host:"}, {"sh", "host:"}, {"mux", "host:"}} {
		o, err := parseOptions(args)
		if err != nil || o.command != canonicalCommand(args[0]) {
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
		{"kill", []string{"host:work"}},
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
	o, err := parseCopyCommand([]string{".:local", "host:remote", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(o.ssh, []string{"-p", "2222"}) {
		t.Fatal(o, err)
	}
	mounted, err := parseFTPOptions([]string{"host:remote", "here:", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(mounted.SSH, []string{"-p", "2222"}) {
		t.Fatal(mounted, err)
	}
}

func TestListRejectsHostEndpoint(t *testing.T) {
	for _, command := range []string{"list", "ls"} {
		for _, host := range []string{"work", "work:", "here:", ".:", "user@[2001:db8::1]:"} {
			if code, err := run([]string{command, host, "--json"}); code != 2 || err == nil {
				t.Fatalf("%s %s: %d %v", command, host, code, err)
			}
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
	for _, args := range [][]string{nil, {"work"}, {"work:"}, {".:/tmp/sshore"}, {"work:/tmp/sshore", "--", "-p", "2222"}} {
		if _, err := parseCLI(args, "install"); err != nil {
			t.Fatalf("rejected %q: %v", args, err)
		}
	}
	for _, args := range [][]string{{""}, {"work", "/tmp/sshore"}, {"--path"}, {"--path", ""}} {
		if _, err := parseCLI(args, "install"); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	for _, args := range [][]string{{"bootstrap"}, {"setup", "--apply", "--path", "/tmp/sshore"}, {"install", "work:relative"}, {"install", "here:relative"}, {"install", "origin:"}, {"install", "work:/tmp/sshore", "--apply"}, {"install", "--", "-p", "2222"}} {
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
	if err == nil || !strings.Contains(err.Error(), "approval cancelled") || !errors.Is(err, io.EOF) || calls != 0 {
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

func TestTaskSelectorsResolveOnTheirOwnHost(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	local := registerTestShell(t, tasks, "work", strings.Repeat("a", 32))
	// Leftover observations from older releases cannot affect either scope.
	legacy, err := stateDir("shells")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "stale.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"attach", "kill", "rename"} {
		for _, ref := range []string{"aaaaaa", ":aaaaaa", "here:aaaaaa", "remote:aaaaaa"} {
			args := []string{ref}
			if command == "rename" {
				args = append(args, "new")
			}
			parsed, err := parseCLI(args, command)
			if err != nil {
				t.Fatal(err)
			}
			target, selected, err := resolveShortTask(command, &parsed)
			if err != nil {
				t.Fatal(err)
			}
			o, err := shellOptions(command, parsed)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(ref, "remote:") {
				if selected || o.local || o.host != "remote" || o.session != "aaaaaa" {
					t.Fatalf("%s %s should pass the ID to remote: %+v", command, ref, o)
				}
			} else if !selected || !o.local || o.session != "work" || target.Identity != local.identity() {
				t.Fatalf("%s %s resolved to %+v", command, ref, target)
			}
		}
	}
	close(tasks.records[local.UID].(*nativeShellTask).shell.done)
	registerTestShell(t, tasks, "work", strings.Repeat("b", 32))
	if _, err := run([]string{"kill", "aaaaaa"}); err == nil {
		t.Fatal("old local ID selected a replacement")
	}
}

func TestShortTaskResourceRenameAndReplacement(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	for i, kind := range []string{"mount", "forward"} {
		task, done := registerTestResource(t, tasks, kind, kind+"job", strings.Repeat(string(rune('a'+i)), 32))
		r := task.record
		key := resourceIdentity(r)
		code := taskShortCode(key)
		previousAlias := kind + "alias"
		if _, err := run([]string{"rename", kind + "/" + r.Name, previousAlias}); err != nil {
			t.Fatal(err)
		}
		if _, err := run([]string{"rename", ":" + code, kind + "new"}); err != nil {
			t.Fatal(err)
		}
		inv, err := collectInventory()
		if err != nil || !slices.ContainsFunc(inv.Resources, func(row resourceView) bool {
			return row.Identity == key && row.ID == kind+"/"+kind+"new" && row.ShortID == code && row.State == taskActive
		}) {
			t.Fatalf("renamed resource inventory: %+v %v", inv.Resources, err)
		}
		if _, err := run([]string{"kill", kind + "/" + previousAlias}); err == nil {
			t.Fatal("old alias still selects the resource")
		}
		parsed, err := parseCLI([]string{kind + "/" + code}, "kill")
		if err != nil {
			t.Fatal(err)
		}
		target, ok, err := resolveShortTask("kill", &parsed)
		if err != nil || !ok || target.Name != kind+"new" {
			t.Fatalf("%+v %v", target, err)
		}
		close(done)
		registerTestResource(t, tasks, kind, r.Name, strings.Repeat(string(rune('c'+i)), 32))
		if err = stopResource(kind, r.Name, key); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatal("accepted replacement", err)
		}
		if _, err = run([]string{"rename", ":" + code, "other"}); err == nil {
			t.Fatal("renamed replacement by old ID")
		}
	}
}

func TestShortTaskUnknownAndScopedNames(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-connect"))
	for _, args := range [][]string{{"kill", "123456"}, {"rename", "ABCDEF", "new"}, {"review", "abcdef"}} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, ref := range []string{"here:abcdef", "host:abcdef", "mount/abcdef"} {
		parsed, err := parseCLI([]string{ref}, "kill")
		if err != nil {
			t.Fatal(err)
		}
		_, selected, err := resolveShortTask("kill", &parsed)
		if selected || (err == nil) != (ref == "host:abcdef") {
			t.Fatalf("unexpected scope for %s: selected=%t, %v", ref, selected, err)
		}
	}

}

func TestShortTaskLocalCommandRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	r := registerTestShell(t, tasks, "work", "123456"+strings.Repeat("a", 26))
	requests := make(chan message, 2)
	tasks.records[r.UID].(*nativeShellTask).serve = func(c net.Conn, m message) error {
		requests <- m
		reply := "killed"
		if m.Type == "rename" {
			reply = "renamed"
		}
		return writeMessage(c, message{Type: reply})
	}
	for _, command := range []string{"kill", "rename"} {
		args := []string{command, ":123456"}
		if command == "rename" {
			args = []string{command, "here:123456", "new"}
		}
		code, err := run(args)
		if code != 0 || err != nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
		m := <-requests
		if m.Type != command || command == "rename" && m.Name != "new" {
			t.Fatalf("wrong route: %+v", m)
		}
	}
}

func TestTaskPrefixCollisionResolution(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	first := "abcdef" + strings.Repeat("1", 26)
	second := "abcdef" + strings.Repeat("2", 26)
	registerTestShell(t, tasks, "one", first)
	registerTestShell(t, tasks, "two", second)
	for _, tc := range []struct{ ref, uid string }{
		{"ab", ""}, {"abcdef", ""}, {"abcdef1", first}, {first, first}, {second, second}, {"ABCDEF2", second},
	} {
		parsed, err := parseCLI([]string{":" + tc.ref}, "attach")
		if err != nil {
			t.Fatal(err)
		}
		target, selected, err := resolveShortTask("attach", &parsed)
		if tc.uid == "" {
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("accepted ambiguous prefix: %v", err)
			}
		} else if err != nil || !selected || target.Identity != "task:"+tc.uid {
			t.Fatalf("%s: %+v %v", tc.ref, target, err)
		}
	}

}

func TestKillOptions(t *testing.T) {
	o, err := parseOptions([]string{"kill", "host:build", "--server", "/a b/sshore", "--", "-p", "2222"})
	if err != nil || o.command != "kill" || o.session != "build" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, args := range [][]string{
		{"kill"}, {"kill", "host"}, {"kill", "host", "../other"},
		{"kill", "host", "one", "two"}, {"kill", "host:one", "--quiet"},
		{"kill", "host:one", "--download-dir", "/tmp"}, {"kill", "host:one", "--no-escape"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	var out bytes.Buffer
	if err := showHelp([]string{"kill"}, &out); err != nil || !strings.Contains(out.String(), "without confirmation") {
		t.Fatal(out.String(), err)
	}
}

func TestRenameOptions(t *testing.T) {
	for _, args := range [][]string{{"rename", "host:old", "new"}, {"rename", "host:old", "new", "--", "-p22"}} {
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

	o, err := parseOptions([]string{"rename", "12:old", "new"})
	if err != nil || o.host != "12" || o.session != "old" || o.newName != "new" {
		t.Fatalf("numeric host: %+v %v", o, err)
	}

}

func TestKillAllScope(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		host     string
		all, bad bool
	}{
		{[]string{"--all"}, "", true, false},
		{[]string{"fedora:", "--all"}, "fedora", true, false},
		{[]string{"here:", "--all"}, "", true, false},
		{[]string{"fedora", "--all"}, "fedora", true, false},
		{[]string{"--all", "fedora", "--", "-p", "2222"}, "fedora", true, false},
		{[]string{"here:", "--all"}, "", true, false},
		{[]string{"fedora", "shell", "--all"}, "", true, true},
		{[]string{"--all", "--all"}, "", true, true},
		{[]string{"--all", "--json"}, "", true, true},
		{[]string{"fedora", "shell", "--", "--all"}, "", false, true},
	} {
		parsed, err := parseCLI(tc.args, "kill")
		all := parsed.has("--all")
		var o options
		if err == nil && all {
			o, err = killAllOptions(parsed)
		}
		if all != tc.all || (err != nil) != tc.bad || (!tc.bad && all && o.host != tc.host) {
			t.Errorf("%v: host=%q all=%t err=%v", tc.args, o.host, all, err)
		}
	}
}

func TestListNumbersAreNotSelectors(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	code, err := run([]string{"kill", "1"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "requires HOST:ID") {
		t.Fatalf("kill: %d %v", code, err)
	}
	o, err := parseOptions([]string{"attach", "1"})
	if err != nil || o.local || o.host != "1" {
		t.Fatalf("numeric host: %+v %v", o, err)
	}
}

func TestAttachUnknownTargetNeverConnects(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	marker := filepath.Join(t.TempDir(), "ssh-invoked")
	ssh := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHORE_SSH", ssh)
	for _, ref := range []string{"xxxyyy", "3a"} {
		code, err := run([]string{"attach", ":" + ref})
		if err == nil {
			t.Fatalf("unknown %s: %d %v", ref, code, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unknown target started SSH: %v", err)
	}
}

func TestTwoCharacterTaskPrefix(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	tasks := startRequestDaemon(t)
	first := "3aaaaa" + strings.Repeat("1", 26)
	registerTestShell(t, tasks, "short-prefix", first)
	registerTestShell(t, tasks, "db", strings.Repeat("c", 32))
	parsed, err := parseCLI([]string{":3a"}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, selected, err := resolveShortTask("attach", &parsed)
	if err != nil || !selected || target.Identity != "task:"+first {
		t.Fatalf("unique short prefix: %+v %v", target, err)
	}
	alias, _ := parseCLI([]string{":db"}, "attach")
	if candidate, ok, err := resolveShortTask("attach", &alias); err != nil || !ok || candidate.Name != "db" {
		t.Fatalf("short hex alias lost: %+v %v", candidate, err)
	}
	registerTestShell(t, tasks, "collision", "3abbbb"+strings.Repeat("2", 26))
	parsed, _ = parseCLI([]string{":3a"}, "attach")
	if _, selected, err := resolveShortTask("attach", &parsed); err == nil || selected || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous short prefix selected: %v", err)
	}
}

type cliFixture struct {
	bin, dir string
	env      []string
}

func fixtureDaemon(t *testing.T, state string) localShellRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "daemon", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record localShellRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// These fields are the documented JSON command output, not persisted records.
type cliShell struct {
	ShortID string `json:"short_id"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	PID     int    `json:"pid"`
}

func (e *cliFixture) shells(t *testing.T) []cliShell {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "list", "--json")
	cmd.Env = e.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var inv struct {
		LocalShells []cliShell `json:"local_shells"`
	}
	if err := json.Unmarshal(out, &inv); err != nil {
		t.Fatalf("list output: %v: %s", err, out)
	}
	return inv.LocalShells
}

func newCLI(t *testing.T) *cliFixture {
	t.Helper()
	e := &cliFixture{bin: testBinary(t), dir: t.TempDir()}
	e.env = append(os.Environ(), "HOME="+e.dir, "SSHORE_STATE_DIR="+filepath.Join(e.dir, "state"), "SSHORE_RUNTIME_DIR="+filepath.Join(e.dir, "runtime"), "SSHORE_SOCKET=", "SSHORE_LOCAL_SESSION=", "SSHORE_DISABLED=", "SSH_CONNECTION=", "SSHORE_TREE_ID=", "GORACE=atexit_sleep_ms=0", "SHELL=/bin/sh", "PS1=sshore-test> ")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, e.bin, "kill", "--all")
		cmd.Env, cmd.Stdin = e.env, strings.NewReader("kill\n")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("CLI cleanup: %v\n%s", err, output)
		}
	})
	return e
}

func TestScripts(t *testing.T) {
	bin := testBinary(t)
	repo, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	testscript.Run(t, testscript.Params{
		Dir: "testdata/cli",
		Setup: func(env *testscript.Env) error {
			env.Setenv("REPO", repo)
			env.Setenv("SSHORE_BIN", bin)
			env.Setenv("SSHORE_TEST_CLI", "1")
			return nil
		},
	})
}
