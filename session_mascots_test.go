package main

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSessionMascots(t *testing.T) {
	// Assets may change foreground color or reset styles, but cannot move the
	// cursor, set a title, access the clipboard or inject arbitrary controls.
	channel := `(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])`
	sgr := regexp.MustCompile(`\x1b\[(?:0|38;2;` + channel + `;` + channel + `;` + channel + `)m`)
	seen := map[string]bool{}
	for _, name := range automaticSessionNames() {
		if err := validSession(name); err != nil || seen[name] {
			t.Fatalf("invalid or duplicate name %q: %v", name, err)
		}
		seen[name] = true
		art := sessionArt(name)
		if !strings.HasSuffix(art, "\x1b[0m\n") || !strings.Contains(art, "\x1b[38;2;") {
			t.Fatalf("missing color art or final reset for %s", name)
		}
		plain := sgr.ReplaceAllString(art, "")
		lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
		if len(lines) > 28 {
			t.Fatalf("art too tall for %s", name)
		}
		for _, line := range lines {
			if utf8.RuneCountInString(line) > 60 {
				t.Fatalf("art too wide for %s", name)
			}
			for _, c := range line {
				if c != ' ' && c != '█' {
					t.Fatalf("unexpected character/control %U in %s", c, name)
				}
			}
		}
	}
	files, err := mascotFiles.ReadDir("assets/pokemon-colorscripts/colorscripts")
	if err != nil || len(files) != len(seen) || len(seen) != 24 {
		t.Fatalf("expected exactly 24 bundled mascots: %d, %v", len(files), err)
	}
	if sessionArt("custom") != "" || sessionArt("../LICENSE") != "" {
		t.Fatal("non-roster names must not load assets")
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
		{"recovery has no session", options{plain: true}, ".", "", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := acceptSessionName(&tc.opts, tc.name)
			if (err != nil) != tc.wantError || tc.opts.session != tc.want {
				t.Fatalf("session %q, error %v", tc.opts.session, err)
			}
		})
	}
}
