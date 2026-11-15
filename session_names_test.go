package main

import "testing"

func TestSessionNames(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range automaticSessionNames() {
		if err := validSession(name); err != nil || seen[name] {
			t.Fatalf("invalid or duplicate name %q", name)
		}
		seen[name] = true
	}
	if len(seen) != 154 {
		t.Fatal("expected 154 automatic names")
	}
}

func TestAcceptSessionName(t *testing.T) {
	for _, tc := range []struct {
		label      string
		opts       options
		name, want string
		wantError  bool
	}{
		{"automatic", options{}, "pikachu", "pikachu", false},
		{"invalid remote name", options{}, "../bad", "", true},
		{"missing remote name", options{}, "", "", true},
		{"explicit", options{session: "build"}, "", "build", false},
		{"recovery has no shell", options{plain: true}, ".", "", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := acceptSessionName(&tc.opts, tc.name)
			if (err != nil) != tc.wantError || tc.opts.session != tc.want {
				t.Fatalf("shell %q, error %v", tc.opts.session, err)
			}
		})
	}
}
