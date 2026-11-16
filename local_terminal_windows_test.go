package main

import "testing"

func TestLocalShellPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"shell", []string{`SHELL=C:\Program Files\nu\nu.exe`, `COMSPEC=C:\Windows\System32\cmd.exe`}, `C:\Program Files\nu\nu.exe`},
		{"case and override", []string{`SHELL=old.exe`, `Shell=nu.exe`}, `nu.exe`},
		{"comspec", []string{`SHELL=`, `ComSpec=custom.exe`}, `custom.exe`},
		{"default", nil, `cmd.exe`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := localShellPath(tc.env)
			if path != tc.want {
				t.Fatalf("got %q, want %q", path, tc.want)
			}
		})
	}
}
