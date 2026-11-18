package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type stubSetupInput struct {
	answers  []string
	closeErr error
	closed   bool
}

func (s *stubSetupInput) Read(string, bool) (string, error) {
	if len(s.answers) == 0 {
		return "", io.EOF
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	return answer, nil
}

func (s *stubSetupInput) Close() error { s.closed = true; return s.closeErr }

func (s *stubSetupInput) ReadPrefilled(prompt string, paths bool, initial string) (string, error) {
	line, err := s.Read(prompt, paths)
	if line == "" && err == nil {
		return initial, nil
	}
	return line, err
}

func TestSetupRejectsRecoveryMode(t *testing.T) {
	var stream bytes.Buffer
	writeMessage(&stream, message{Type: "setup_ready", SoftwareVersion: version, Action: "plain"})
	if err := negotiateSetup(&stream, &bytes.Buffer{}, &options{}); err == nil {
		t.Fatal("obsolete recovery mode accepted")
	}
}

func TestAttachSessionSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  []string
		want     string
		closeErr error
	}{
		{name: "choose", answers: []string{"#0", "#3", "#2"}, want: "eevee"},
		{name: "name", answers: []string{"eevee"}, want: "eevee"},
		{name: "filter", answers: []string{"/EEV", "#1", "#2"}, want: "eevee"},
		{name: "filter-no-auto-attach", answers: []string{"/eev", ""}},
		{name: "clear-filter", answers: []string{"/missing", "/", "pikachu"}, want: "pikachu"},
		{name: "cancel", answers: []string{""}},
		{name: "eof"},
		{name: "eof-and-restore", closeErr: errors.New("restore failed")},
		{name: "cancel-and-restore", answers: []string{""}, closeErr: errors.New("restore failed")},
		{name: "restore", answers: []string{"#1"}, closeErr: errors.New("restore failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming, outgoing bytes.Buffer
			writeMessage(&incoming, message{Type: "session_choice", SoftwareVersion: version, Names: []string{"pikachu", "eevee"}})
			writeMessage(&incoming, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"})
			input := &stubSetupInput{answers: tc.answers, closeErr: tc.closeErr}
			err := negotiateSetupWithInput(&incoming, &outgoing, &options{command: "attach"}, func() (setupInput, error) { return input, nil })
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Fatalf("lost terminal restoration error: %v", err)
			}
			if !input.closed {
				t.Fatal("input left open")
			}
			if tc.want == "" {
				if err == nil || outgoing.Len() != 0 {
					t.Fatalf("cancelled selection sent: %v", err)
				}
				return
			}
			m, readErr := readMessage(&outgoing)
			if err != nil || readErr != nil || m.Type != "session_select" || m.Name != tc.want {
				t.Fatal(m, err, readErr)
			}
		})
	}
}

func TestSessionSelectionNumericNames(t *testing.T) {
	for _, tc := range []struct{ answer, want string }{{"2", "2"}, {"#2", "eevee"}} {
		selected, err := chooseSetupSession(&stubSetupInput{answers: []string{tc.answer}}, []string{"2", "eevee"})
		if err != nil || selected != tc.want {
			t.Fatal(selected, err)
		}
	}
}

func TestReleaseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.4.9", false}, {"0.4.9", "0.4.0", false}, {"0.4.0", "0.4.0", true},
		{"0.4.0", "0.5.0", false}, {"1.4.0", "0.4.0", false},
		{"0.4.0", "0.40.0", false}, {"0.4.0", "0.4.00", false},
		{"0.4.0", "0.4.1-rc1", false}, {"0.4.0", "", false},
		{"0.4.1-dev-a", "0.4.1-dev-a", false}, {"0.4.1-dev-a", "0.4.1-dev-b", false},
		{"unknown", "unknown", false}, {"0.4", "0.4", false}, {"0.4.00", "0.4.00", false},
	} {
		if got := sameRelease(tc.a, tc.b); got != tc.want {
			t.Fatalf("%s / %s: %v, want %v", tc.a, tc.b, got, tc.want)
		}
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
		{"unknown", "unknown", 0, false}, {"0.4.0-dev", "0.4.0-dev", 0, false}, {"0.4", "0.4", 0, false},
	} {
		got, known := compareReleaseVersions(tc.a, tc.b)
		if got != tc.order || known != tc.known {
			t.Fatal(tc, got, known)
		}
	}
}
