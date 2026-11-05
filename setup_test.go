package main

import (
	"bytes"
	"testing"
)

func TestSetupPlainClearsApprovedOperations(t *testing.T) {
	var stream bytes.Buffer
	if err := writeMessage(&stream, message{Type: "setup_ready", SoftwareVersion: version, Action: "plain"}); err != nil {
		t.Fatal(err)
	}
	o := options{approved: []operation{{Kind: "forward", Port: 1234}}, resumePID: 123, stay: true}
	if err := negotiateSetup(&stream, &bytes.Buffer{}, &o); err != nil {
		t.Fatal(err)
	}
	if !o.plain || !o.noEscape || o.stay || len(o.approved) > 0 || o.resumePID != 0 {
		t.Fatal(o)
	}
}
func TestCompareReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		order int
		known bool
	}{
		{"0.4.0", "0.4.0", 0, true}, {"0.4.1", "0.4.0", 1, true}, {"0.4.0", "0.4.1", -1, true},
		{"0.10.0", "0.9.9", 1, true}, {"2.0.0", "1.99.99", 1, true}, {"0.4.0-dev", "0.4.0", 0, false},
	} {
		got, known := compareVersions(tc.a, tc.b)
		if got != tc.order || known != tc.known {
			t.Fatal(tc, got, known)
		}
	}
}
