package main

import (
	"context"
	"net/http"
	"testing"
)

func TestBackgroundMountSetupPrompt(t *testing.T) {
	for _, answer := range []string{"update", ""} {
		t.Run("answer="+answer, func(t *testing.T) {
			input := &stubSetupInput{answers: []string{answer}}
			handled := make(chan struct{})
			token := resourceToken()
			listener, broker, err := resourceListen(token, func(w http.ResponseWriter, r *http.Request) {
				defer close(handled)
				serveResourceSetupWithInput(w, r, func() (setupInput, error) { return input, nil })
			})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			service := &resourceService{record: resourceRecord{Host: "fixture"}, config: resourceConfig{AuthAddress: listener.Addr().String(), AuthToken: token}}
			action, err := service.setupUI(context.Background()).Choose(message{Type: "setup_choice", Action: "update", SoftwareVersion: version, SetupProcesses: []setupProcess{{Name: "work", PID: 123, Version: "0.0.1"}}})
			<-handled
			want := answer
			if want == "" {
				want = "cancel"
			}
			if err != nil || action != want || !input.closed {
				t.Fatalf("action=%q want=%q closed=%v err=%v", action, want, input.closed, err)
			}
		})
	}
}
