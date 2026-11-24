package sshconfig

import (
	"reflect"
	"testing"
)

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
		{"-*", []string{}}, {"ssh-rsa", []string{}},
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
