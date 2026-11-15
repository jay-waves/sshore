package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
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
	for {
		m, err := readMessage(r)
		if err != nil {
			return fmt.Errorf("remote setup failed: %w", err)
		}
		if m.Type == "setup_diagnostic" {
			termui.Log(os.Stderr, termui.Info, "%s\n", cleanText(strings.TrimSpace(m.Error)))
			continue
		}
		if m.Type == "fatal" {
			return fmt.Errorf("%s", m.Error)
		}
		if m.SoftwareVersion != version {
			return fmt.Errorf("unexpected setup version: %s", m.SoftwareVersion)
		}
		switch m.Type {
		case "setup_ready":
			if m.Action == "plain" {
				if len(o.approved) > 0 {
					termui.Log(os.Stderr, termui.Warning, "[sshore] Operations skipped; reconnect and request again.\n")
				}
				o.plain, o.noEscape, o.stay = true, true, false
				o.approved, o.resumePID = nil, 0
			} else if m.Action == "updated" {
				termui.Log(os.Stderr, termui.Success, "[sshore] Remote updated to %s.\n", version)
				if len(o.approved) > 0 {
					termui.Log(os.Stderr, termui.Warning, "[sshore] Installation changed; operations skipped. Request again.\n")
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
			if m.Action != "update" && m.Action != "manual-only" {
				return fmt.Errorf("invalid setup choice")
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
			termui.Log(os.Stderr, termui.Info, "Recovery: temporary shell, sshore disabled.\n")
			prompt := "Type update to install, or Enter to recover: "
			if m.Action == "manual-only" {
				prompt = "Press Enter to recover: "
			}
			input, err := openInput()
			if err != nil {
				return err
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
					termui.Log(os.Stderr, termui.Warning, "Type update or Enter to recover.\n")
				}
			}
			closeErr := input.Close()
			if err := errors.Join(err, closeErr); err != nil {
				return fmt.Errorf("setup input or terminal restoration failed; no update requested: %w", err)
			}
			action := "plain"
			if answer == "update" {
				action = "update"
			}
			if err := writeMessage(w, message{Type: "setup_action", Action: action}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected setup response: %s", m.Type)
		}
	}
}
