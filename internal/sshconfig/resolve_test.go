package sshconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, config string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveSources(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20-work.conf", "Host *\n User late\n IdentityFile \"second key\"\n")
	write(t, dir, "10-work.conf", "User first\nHost unrelated\n User never\n")
	userFile := write(t, dir, "config", `Host work* !work-blocked
 HostName REAL.example.test
 Include *-work.conf
 IdentityFile "third key"
 UserKnownHostsFile "known hosts" other
 Port 2222
Match host real.example.test originalhost work*
 ProxyJump first,admin@second:2333
Match command "deploy *"
 BatchMode yes
Match final all
 Port 2207
 RequiredRSASize 3072
Host real.example.test
 ConnectTimeout 1m30s
`)
	systemFile := write(t, dir, "system", "User system\nPort 2299\nIdentityFile fourth\n")
	req := Request{Host: "workstation", LocalUser: "local", Command: "deploy build",
		Sources:   []Source{{Path: userFile, Base: dir}, {Path: systemFile, Base: dir}},
		Overrides: []Option{{Key: "identityfile", Values: []string{"first key"}}},
	}
	result, err := Resolve(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"hostname": {"real.example.test"}, "user": {"first"}, "port": {"2222"},
		"requiredrsasize": {"3072"}, "connecttimeout": {"90"},
		"identityfile":       {"first key", "second key", "third key", "fourth"},
		"userknownhostsfile": {"known hosts", "other"},
		"proxyjump":          {"first,admin@second:2333"},
		"batchmode":          {"yes"},
	}
	for key, values := range want {
		if !reflect.DeepEqual(result.Values[key], values) {
			t.Errorf("%s = %q, want %q", key, result.Values[key], values)
		}
	}
	if result.Origins["user"] != filepath.Join(dir, "10-work.conf")+":1" {
		t.Errorf("user origin = %q", result.Origins["user"])
	}
}

func TestResolveFailures(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	for _, tc := range []struct{ input, want string }{
		{"Include config\n", "recursive SSH Include"},
		{"Match exec \"exit 0\"\n Port 2222\n", "Match exec is unsupported"},
		{"Port invalid\n", "invalid numeric"},
		{"UnknownOption yes\n", "unsupported SSH configuration option"},
		{"Host fixture\n User \"bad\n", "unterminated quoted argument"},
	} {
		write(t, dir, "config", tc.input)
		_, err := Resolve(t.Context(), Request{Host: "fixture", Sources: []Source{{Path: config, Base: dir}}})
		if err == nil || !strings.Contains(err.Error(), config+":") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want file and %q", tc.input, err, tc.want)
		}
	}
}

func TestCommandAndProxyPrecedence(t *testing.T) {
	dir := t.TempDir()
	config := write(t, dir, "config", "IgnoreUnknown vendor*\nVendorFlag yes\nProxyCommand helper\nPort 22\n")
	r, err := Resolve(t.Context(), Request{
		Host: "fixture", Sources: []Source{{Path: config}},
		Overrides: []Option{{Key: "proxyjump", Values: []string{"none"}}, {Key: "port", Values: []string{"2222"}}},
	})
	if err != nil || len(r.Values["proxycommand"]) > 0 || !reflect.DeepEqual(r.Values["port"], []string{"2222"}) {
		t.Fatalf("precedence: %+v, %v", r, err)
	}
}

func TestParseWindowsPathLists(t *testing.T) {
	opt, err := ParseOption(`UserKnownHostsFile="C:\Users\Example Person\known_hosts" "D:\ssh\known_hosts#2" # note`)
	want := []string{`C:\Users\Example Person\known_hosts`, `D:\ssh\known_hosts#2`}
	if err != nil || !reflect.DeepEqual(opt.Values, want) {
		t.Fatalf("paths %q, %v", opt.Values, err)
	}
}

func TestAlgorithms(t *testing.T) {
	supported := []string{"ssh-ed25519", "ssh-ed25519-cert", "rsa-sha2-256", "rsa-sha2-512"}
	for _, tc := range []struct {
		spec string
		want []string
	}{
		{"^rsa-sha2-512", []string{"rsa-sha2-512", "ssh-ed25519", "ssh-ed25519-cert", "rsa-sha2-256"}},
		{"-ssh-ed25519*", []string{"rsa-sha2-256", "rsa-sha2-512"}},
		{"+ssh-ed25519", supported}, {"+unsupported", supported}, {"^unsupported", supported},
		{"rsa-sha2-512,rsa-sha2-512,unsupported,ssh-ed25519", []string{"rsa-sha2-512", "ssh-ed25519"}},
		{"-*", nil}, {"ssh-rsa", nil},
	} {
		got, err := Algorithms(supported, tc.spec)
		if !reflect.DeepEqual(tc.want, got) || err != nil {
			t.Errorf("%s: got %v, %v; want %v", tc.spec, got, err, tc.want)
		}
	}
	for _, invalid := range []string{"+", "ssh-ed25519,,rsa-sha2-512"} {
		if _, err := Algorithms(supported, invalid); err == nil {
			t.Errorf("accepted invalid algorithm list %q", invalid)
		}
	}
}

// These expectations were checked against OpenSSH 10.2p1.
func TestFinalPassOpenSSHSemantics(t *testing.T) {
	dir := t.TempDir()
	extra := write(t, dir, "included.conf", "User manager\nIdentityFile /second\n")
	config := write(t, dir, "config", `Host Work
 HostName HOST.example.test
 IdentityFile /first
 Include "`+filepath.ToSlash(extra)+`"
 Port 2222
Match final all
 Port 2207
 RequiredRSASize 3072
Host host.example.test
 ProxyJump relay
`)
	req := Request{Host: "Work", Sources: []Source{{Path: config, Base: dir}}}
	result, err := Resolve(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"hostname": {"host.example.test"}, "user": {"manager"}, "port": {"2222"},
		"requiredrsasize": {"3072"}, "identityfile": {"/first", "/second"},
		"proxyjump": {"relay"},
	}
	for key, values := range want {
		if !reflect.DeepEqual(result.Values[key], values) {
			t.Errorf("%s: native %q, OpenSSH %q", key, result.Values[key], values)
		}
	}
}
