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
				logf(os.Stderr, logSuccess, "[sshore] Remote updated to %s.\n", version)
				if len(o.approved) > 0 {
					logf(os.Stderr, logWarning, "[sshore] Setup changed the remote installation; approved operations were skipped. Request them again.\n")
				}
				o.approved, o.resumePID, o.stay = nil, 0, false
			} else if m.Action != "managed" {
				return fmt.Errorf("invalid setup mode")
			}
			return nil
		case "setup_choice":
			if m.Action != "update" && m.Action != "manual-only" {
				return fmt.Errorf("invalid setup choice")
			}
			logf(os.Stderr, logWarning, "[sshore] %s\n", cleanText(m.Error))
			if m.Action == "update" {
				if len(m.SetupProcesses) == 0 {
					logf(os.Stderr, logInfo, "No sessions need to be stopped.\n")
				} else {
					logf(os.Stderr, logWarning, "Updating will end these sessions and their tasks:\n")
				}
			} else if len(m.SetupProcesses) > 0 {
				logf(os.Stderr, logWarning, "Sessions requiring manual recovery:\n")
			}
			for _, p := range m.SetupProcesses {
				logf(os.Stderr, logWarning, "  session %s, PID %d, version %s\n", cleanText(p.Name), p.PID, cleanText(p.Version))
			}
			logf(os.Stderr, logInfo, "Enter opens a temporary recovery shell; sshore features are disabled.\n")
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
					logf(os.Stderr, logWarning, "Automatic update is unavailable. Press Enter to recover.\n")
				} else {
					logf(os.Stderr, logWarning, "Type update, or press Enter to recover.\n")
				}
			}
			closeErr := input.Close()
			if err != nil {
				return fmt.Errorf("setup input failed; no update requested: %w", err)
			}
			if closeErr != nil {
				return fmt.Errorf("restore terminal; no update requested: %w", closeErr)
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
