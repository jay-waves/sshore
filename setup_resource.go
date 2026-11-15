package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type resourceSetupRequest struct {
	Host   string
	Choice message
}

func (s *resourceService) setupUI(ctx context.Context) setupClientUI {
	return setupClientUI{
		Log: &resourceSetupLog{ctx: ctx, address: s.config.AuthAddress, token: s.config.AuthToken},
		Choose: func(m message) (string, error) {
			body, err := json.Marshal(resourceSetupRequest{Host: s.record.Host, Choice: m})
			if err != nil {
				return "", err
			}
			resp, err := resourceHTTPContext(ctx, s.config.AuthAddress, s.config.AuthToken, "POST", "/setup", bytes.NewReader(body))
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			answer, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
			if err != nil {
				return "", err
			}
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("setup confirmation failed: %s", cleanText(strings.TrimSpace(string(answer))))
			}
			action := strings.TrimSpace(string(answer))
			if action != "update" && action != "cancel" {
				return "", fmt.Errorf("invalid setup confirmation")
			}
			return action, nil
		},
	}
}

type resourceSetupLog struct {
	ctx            context.Context
	address, token string
}

func (l *resourceSetupLog) Write(p []byte) (int, error) {
	resp, err := resourceHTTPContext(l.ctx, l.address, l.token, "POST", "/setup-log", bytes.NewReader(p))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("setup log unavailable")
	}
	return len(p), nil
}

// Runs in the foreground launcher's authenticated, short-lived prompt broker.
func serveResourceSetup(w http.ResponseWriter, r *http.Request) {
	serveResourceSetupWithInput(w, r, func() (setupInput, error) { return newApprovalInput() })
}

func serveResourceSetupWithInput(w http.ResponseWriter, r *http.Request, openInput func() (setupInput, error)) {
	if r.URL.Path == "/setup-log" {
		b, err := readSetupLimited(r.Body, 8192)
		if err != nil {
			http.Error(w, "invalid setup log", 400)
			return
		}
		fmt.Fprintln(os.Stderr, cleanText(strings.TrimSpace(string(b))))
		return
	}
	data, err := readSetupLimited(r.Body, 256*1024)
	var request resourceSetupRequest
	if err != nil || json.Unmarshal(data, &request) != nil || validSSHDestination(request.Host) != nil || request.Choice.Type != "setup_choice" || request.Choice.SoftwareVersion != version {
		http.Error(w, "invalid setup request", 400)
		return
	}
	action, err := chooseSetupAction(request.Choice, &options{command: "mount-agent", host: request.Host}, openInput)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	fmt.Fprint(w, action)
}
