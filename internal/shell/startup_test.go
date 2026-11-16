package shell

import (
	"reflect"
	"testing"
)

func TestStartupModes(t *testing.T) {
	for _, tc := range []struct {
		platform, path string
		noLogin        bool
		want           []string
		fail           bool
	}{
		{"linux", "/bin/bash", false, []string{"-l", "-i"}, false},
		{"linux", "/bin/zsh", false, []string{"-l", "-i"}, false},
		{"linux", "/bin/fish", true, []string{"-i"}, false},
		{"linux", "/bin/sh", false, []string{"-l", "-i"}, false},
		{"linux", "/custom/shell", false, nil, true},
		{"windows", `C:\Program Files\PowerShell\7\pwsh.exe`, true, nil, true},
		{"windows", `C:\Windows\PowerShell.EXE`, true, nil, true},
		{"windows", "pwsh.exe", false, nil, false},
		{"windows", "cmd.exe", false, nil, false},
		{"windows", "cmd.exe", true, nil, true},
	} {
		got, err := startupArgs(tc.platform, tc.path, tc.noLogin, false)
		if (err != nil) != tc.fail || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: got %q, %v", tc, got, err)
		}
	}
}

func TestNoProfileModes(t *testing.T) {
	for _, tc := range []struct {
		platform, path string
		want           []string
	}{
		{"linux", "bash", []string{"--noprofile", "--norc", "-i"}},
		{"linux", "sh", []string{"-i"}},
		{"linux", "zsh", []string{"-f", "-i"}},
		{"linux", "fish", []string{"--no-config", "-i"}},
		{"windows", "pwsh.exe", []string{"-NoProfile"}},
		{"windows", "cmd.exe", []string{"/D"}},
	} {
		got, err := startupArgs(tc.platform, tc.path, false, true)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
	if _, err := startupArgs("linux", "unknown", false, true); err == nil {
		t.Fatal("unsupported shell accepted")
	}
}
