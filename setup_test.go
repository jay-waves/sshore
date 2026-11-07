package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
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

func TestSetupConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, choice, want string
		answers            []string
		closeErr           error
	}{
		{name: "update", choice: "update", answers: []string{"update"}, want: "update"},
		{name: "enter", choice: "update", answers: []string{""}, want: "plain"},
		{name: "retry", choice: "update", answers: []string{"yes", "update"}, want: "update"},
		{name: "manual-only", choice: "manual-only", answers: []string{"update", ""}, want: "plain"},
		{name: "cancel", choice: "update"},
		{name: "restore-failure", choice: "update", answers: []string{"update"}, closeErr: errors.New("restore failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming, outgoing bytes.Buffer
			for _, m := range []message{
				{Type: "setup_choice", SoftwareVersion: version, Action: tc.choice},
				{Type: "setup_ready", SoftwareVersion: version, Action: "managed"},
			} {
				if err := writeMessage(&incoming, m); err != nil {
					t.Fatal(err)
				}
			}
			input := &stubSetupInput{answers: tc.answers, closeErr: tc.closeErr}
			err := negotiateSetupWithInput(&incoming, &outgoing, &options{}, func() (setupInput, error) { return input, nil })
			if !input.closed {
				t.Fatal("input not closed before handoff")
			}
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "no update requested") || outgoing.Len() != 0 {
					t.Fatalf("failed input sent action: %v, %q", err, outgoing.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m, err := readMessage(&outgoing)
			if err != nil || m.Type != "setup_action" || m.Action != tc.want {
				t.Fatalf("action: %+v, %v", m, err)
			}
		})
	}
}

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

func TestAttachSessionSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  []string
		want     string
		closeErr error
	}{
		{name: "choose", answers: []string{"0", "3", "2"}, want: "eevee"},
		{name: "cancel", answers: []string{""}},
		{name: "eof"},
		{name: "restore", answers: []string{"1"}, closeErr: errors.New("restore failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming, outgoing bytes.Buffer
			writeMessage(&incoming, message{Type: "session_choice", SoftwareVersion: version, Names: []string{"pikachu", "eevee"}})
			writeMessage(&incoming, message{Type: "setup_ready", SoftwareVersion: version, Action: "managed"})
			input := &stubSetupInput{answers: tc.answers, closeErr: tc.closeErr}
			err := negotiateSetupWithInput(&incoming, &outgoing, &options{command: "attach"}, func() (setupInput, error) { return input, nil })
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
