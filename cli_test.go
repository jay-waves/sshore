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
	for _, topic := range []string{"launch", "list", "attach", "cp", "detach", "forward", "review", "cancel", "mount", "daemon"} {
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
			connection := sshConnectionOptions(o)
			if !reflect.DeepEqual(connection.Args, o.ssh) {
				t.Fatal(connection.Args)
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

func TestRecoveryDiagnosticsStayLocal(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_RUNTIME_DIR", t.TempDir())
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

func TestNativeSSHPort(t *testing.T) {
	for _, tc := range []struct {
		command  string
		operands []string
	}{
		{"launch", []string{"host"}}, {"spawn", []string{"host"}}, {"attach", []string{"host", "work"}},
		{"list", []string{"host"}}, {"cancel", []string{"host", "work"}},
		{"rename", []string{"host", "old", "new"}}, {"cp", []string{"local", "host:remote"}},
		{"mount", []string{"host:remote"}}, {"forward", []string{"origin:8080", "host:80"}},
	} {
		args := append(append([]string(nil), tc.operands...), "--port=2222", "--", "-J", "jump")
		parsed, err := parseCLI(args, tc.command)
		if err != nil || !reflect.DeepEqual(parsed.ssh, []string{"-p", "2222", "-J", "jump"}) {
			t.Fatalf("%s: %+v %v", tc.command, parsed, err)
		}
	}
	for _, port := range []string{"1", "65535"} {
		if _, err := parseOptions([]string{"launch", "host", "--port", port}); err != nil {
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
	if _, err := parseOptions([]string{"launch", "host", "--port", "22", "--", "-o", "ProxyCommand=ssh -p 23 jump"}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCLI([]string{"--port", "22"}, "review"); err == nil {
		t.Fatal("review accepted SSH port")
	}
	o, err := parseCopyCommand([]string{"local", "host:remote", "--port", "2222"})
	if err != nil || !reflect.DeepEqual(o.ssh, []string{"-p", "2222"}) {
		t.Fatal(o, err)
	}
	mounted, err := parseFTPOptions([]string{"host:remote", "--port", "2222"})
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
