package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jay-waves/sshore/internal/termui"
	"io"
	"net/http"
	"os"
	"strings"
)

type setupProcess struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Start   string `json:"start"`
}

// No keyboard reader runs before SSH authentication. The setup exchange and
// shell/mux exchange use the same pipes and authenticated SSH connection.
func negotiateSetup(r io.Reader, w io.Writer, o *options) error {
	return negotiateSetupWithInput(r, w, o, func() (setupInput, error) { return newApprovalInput() })
}

type setupInput interface {
	Read(string, bool) (string, error)
	Close() error
}

func negotiateSetupWithInput(r io.Reader, w io.Writer, o *options, openInput func() (setupInput, error)) error {
	return negotiateSetupUsing(context.Background(), r, w, o, setupClientUI{OpenInput: openInput})
}

type setupClientUI struct {
	OpenInput func() (setupInput, error)
	Choose    func(message) (string, error)
	Log       io.Writer
	Download  func(context.Context) ([]byte, error)
}

func negotiateSetupUsing(ctx context.Context, r io.Reader, w io.Writer, o *options, ui setupClientUI) error {
	openInput := ui.OpenInput
	if openInput == nil {
		openInput = func() (setupInput, error) { return newApprovalInput() }
	}
	if ui.Log == nil {
		ui.Log = os.Stderr
	}
	if ui.Download == nil {
		ui.Download = localSetupBinary
	}
	log := func(level termui.Color, format string, args ...any) {
		fmt.Fprint(ui.Log, termui.Format(ui.Log == os.Stderr && termui.ColorEnabled(os.Stderr), level, format, args...))
	}
	downloaded := false
	for {
		m, err := readMessage(r)
		if err != nil {
			return fmt.Errorf("remote setup failed: %w", err)
		}
		if m.Type == "setup_diagnostic" {
			log(termui.Info, "%s\n", cleanText(strings.TrimSpace(m.Error)))
			continue
		}
		if m.Type == "fatal" {
			return fmt.Errorf("%s", m.Error)
		}
		if m.SoftwareVersion != version {
			return fmt.Errorf("unexpected setup version: %s", m.SoftwareVersion)
		}
		switch m.Type {
		case "setup_download":
			if downloaded {
				return fmt.Errorf("repeated setup download request")
			}
			if m.Action != "linux-amd64" {
				return fmt.Errorf("unsupported setup download platform")
			}
			downloaded = true
			log(termui.Info, "[sshore] Remote download unavailable; preparing sshore %s locally.\n", version)
			binary, err := ui.Download(ctx)
			if err != nil {
				_, _ = io.WriteString(w, "0 -\n")
				return fmt.Errorf("local release preparation failed; no shells stopped: %w", err)
			}
			if err := sendSetupBinary(w, binary); err != nil {
				return fmt.Errorf("transfer remote executable: %w", err)
			}
		case "setup_ready":
			if m.Action == "plain" {
				if o.command == "mount-agent" || o.command == "cp-agent" {
					return fmt.Errorf("helper setup requires manual recovery; use sshore launch %s", cleanText(o.host))
				}
				if len(o.approved) > 0 {
					log(termui.Warning, "[sshore] Operations skipped; reconnect and request again.\n")
				}
				o.plain, o.noEscape, o.stay = true, true, false
				o.approved, o.resumePID = nil, 0
			} else if m.Action == "updated" {
				log(termui.Success, "[sshore] Remote updated to %s.\n", version)
				if len(o.approved) > 0 {
					log(termui.Warning, "[sshore] Installation changed; operations skipped. Request again.\n")
				}
				o.approved, o.resumePID, o.stay = nil, 0, false
			} else if m.Action != "managed" {
				return fmt.Errorf("invalid setup mode")
			}
			return nil
		case "session_choice":
			if o.command != "attach" || o.session != "" || len(m.Names) < 2 || len(m.Names) > 128 {
				return fmt.Errorf("unexpected shell selection")
			}
			for _, name := range m.Names {
				if err := validSession(name); err != nil {
					return err
				}
			}
			input, err := openInput()
			if err != nil {
				return err
			}
			selected, selectErr := chooseSetupSession(input, m.Names)
			closeErr := input.Close()
			if err := errors.Join(selectErr, closeErr); err != nil {
				return err
			}

			if err := writeMessage(w, message{Type: "session_select", Name: selected}); err != nil {
				return err
			}
		case "setup_choice":
			var action string
			if ui.Choose != nil {
				action, err = ui.Choose(m)
			} else {
				action, err = chooseSetupAction(m, o, openInput)
			}
			if err != nil {
				return err
			}
			if err := writeMessage(w, message{Type: "setup_action", Action: action}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected setup response: %s", m.Type)
		}
	}
}

func chooseSetupAction(m message, o *options, openInput func() (setupInput, error)) (string, error) {
	if m.Action != "update" && m.Action != "manual-only" {
		return "", fmt.Errorf("invalid setup choice")
	}
	termui.Log(os.Stderr, termui.Warning, "[sshore] %s\n", cleanText(m.Error))
	if m.Action == "update" {
		if len(m.SetupProcesses) == 0 {
			termui.Log(os.Stderr, termui.Info, "No shells to stop.\n")
		} else {
			termui.Log(os.Stderr, termui.Warning, "Update ends these shells and tasks:\n")
		}
	} else if len(m.SetupProcesses) > 0 {
		termui.Log(os.Stderr, termui.Warning, "Shells requiring manual recovery:\n")
	}
	for _, p := range m.SetupProcesses {
		termui.Log(os.Stderr, termui.Warning, "  %s  PID %d  %s\n", cleanText(p.Name), p.PID, cleanText(p.Version))
	}
	helper := o.command == "mount-agent" || o.command == "cp-agent"
	if helper && m.Action == "manual-only" {
		return "", fmt.Errorf("automatic setup unavailable; use sshore launch %s for manual recovery", cleanText(o.host))
	}
	if !helper {
		termui.Log(os.Stderr, termui.Info, "Recovery: temporary shell, sshore disabled.\n")
	}
	prompt := "Type update to install, or Enter to recover: "
	if helper {
		prompt = "Type update to install, or Enter to cancel: "
	}
	if m.Action == "manual-only" {
		prompt = "Press Enter to recover: "
	}
	input, err := openInput()
	if err != nil {
		return "", err
	}
	var answer string
	for {
		answer, err = input.Read(prompt, false)
		if err != nil || answer == "" || answer == "manual" || answer == "update" && m.Action != "manual-only" {
			break
		}
		if m.Action == "manual-only" {
			termui.Log(os.Stderr, termui.Warning, "Update unavailable. Enter to recover.\n")
		} else {
			termui.Log(os.Stderr, termui.Warning, "%s\n", strings.TrimSpace(prompt))
		}
	}
	closeErr := input.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return "", fmt.Errorf("setup input or terminal restoration failed; no update requested: %w", err)
	}
	action := "plain"
	if helper {
		action = "cancel"
	}
	if answer == "update" {
		action = "update"
	}
	return action, nil
}

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
