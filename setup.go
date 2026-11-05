package main

import (
	"fmt"
	"io"
	"os"
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
	for {
		m, err := readMessage(r)
		if err != nil {
			return fmt.Errorf("remote setup failed: %w", err)
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
					logf(os.Stderr, logWarning, "[sshore] Approved operations were not executed; request them again after reconnecting.\n")
				}
				o.plain, o.noEscape, o.stay = true, true, false
				o.approved, o.resumePID = nil, 0
			} else if m.Action == "updated" {
				if len(o.approved) > 0 {
					logf(os.Stderr, logWarning, "[sshore] Setup changed the remote installation; approved operations were skipped. Request them again.\n")
				}
				o.approved, o.resumePID, o.stay = nil, 0, false
			} else if m.Action != "managed" {
				return fmt.Errorf("invalid setup mode")
			}
			return nil
		case "setup_choice":
			logf(os.Stderr, logWarning, "[sshore] %s\n", cleanText(m.Error))
			for _, p := range m.SetupProcesses {
				logf(os.Stderr, logWarning, "  session %s, PID %d, version %s\n", cleanText(p.Name), p.PID, cleanText(p.Version))
			}
			prompt := "Type update to END listed old sessions/tasks and install; Enter for a recovery shell: "
			if m.Action == "manual-only" {
				prompt = "Automatic update is unavailable. Press Enter for a recovery shell: "
			}
			input, err := newApprovalInput()
			if err != nil {
				return err
			}
			var answer string
			for {
				answer, err = input.Read(prompt, false)
				if err != nil || answer == "" || answer == "manual" || answer == "update" && m.Action != "manual-only" {
					break
				}
				logf(os.Stderr, logWarning, "Enter update or press Enter for manual recovery.\n")
			}
			closeErr := input.Close()
			if err != nil {
				return fmt.Errorf("setup cancelled")
			}
			if closeErr != nil {
				return closeErr
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
