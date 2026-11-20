package main

import "testing"

func TestInstallTarget(t *testing.T) {
	for _, tc := range []struct{ input, host, path string }{
		{"work", "work", ""},
		{"work:", "work", ""},
		{"work:/opt/sshore", "work", "/opt/sshore"},
		{"here:/opt/sshore", "here", "/opt/sshore"},
		{"user@2001:db8::1", "user@2001:db8::1", ""},
		{"[2001:db8::1]:/opt/sshore", "2001:db8::1", "/opt/sshore"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			host, path, err := installTarget(tc.input)
			if err != nil || host != tc.host || path != tc.path {
				t.Fatalf("installTarget(%q) = %q, %q, %v", tc.input, host, path, err)
			}
		})
	}
}

func TestCopyIDHostGrammar(t *testing.T) {
	for _, target := range []string{"work", "work:", "user@2001:db8::1", "[2001:db8::1]:"} {
		if _, err := parseCLI([]string{target}, "copy-id"); err != nil {
			t.Errorf("copy-id %s: %v", target, err)
		}
	}
	for _, target := range []string{"here:", "origin:", "work:path"} {
		if _, err := parseCLI([]string{target}, "copy-id"); err == nil {
			t.Errorf("copy-id accepted %s", target)
		}
	}
}
