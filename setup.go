package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
)

func negotiateFileSetup(stream io.ReadWriter) (string, error) {
	reply, err := negotiateSetup(stream, nil)
	return reply.HomeDirectory, err
}

func negotiateShellSetup(stream io.ReadWriter, host string, chooseShell bool) (message, error) {
	var choose func([]sessionTarget) (string, error)
	if chooseShell {
		choose = func(targets []sessionTarget) (string, error) {
			return selectSetupShell(host, targets)
		}
	}
	return negotiateSetup(stream, choose)
}

func negotiateSetup(stream io.ReadWriter, choose func([]sessionTarget) (string, error)) (message, error) {
	for {
		m, err := readMessage(stream)
		if err != nil {
			return message{}, fmt.Errorf("remote setup failed: %w", err)
		}
		if m.Type == "setup_diagnostic" {
			termui.Log(os.Stderr, "", termui.Info, "%s\n", cleanText(strings.TrimSpace(m.Error)))
			continue
		}
		if err := m.peerError("remote"); err != nil {
			return message{}, err
		}
		switch m.Type {
		case "setup_ready":
			if m.Action != "managed" {
				return message{}, fmt.Errorf("invalid setup mode")
			}
			return m, nil
		case "session_choice":
			if choose == nil || len(m.Targets) < 2 {
				return message{}, fmt.Errorf("unexpected shell selection")
			}
			selected, err := choose(m.Targets)
			if err != nil {
				return message{}, err
			}
			if err := writeMessage(stream, message{Type: "session_select", Name: selected}); err != nil {
				return message{}, err
			}
			choose = nil
		default:
			return message{}, fmt.Errorf("unexpected setup response: %s", m.Type)
		}
	}
}

func selectSetupShell(host string, targets []sessionTarget) (string, error) {
	names, err := sessionTargetNames(targets)
	if err != nil {
		return "", err
	}
	recent, err := metadataRPC(metadataRequest{Action: "attach_remote_choice", Host: host, Targets: targets})
	if err != nil {
		return "", err
	}
	if recent.Choice != "" {
		return recent.Choice, nil
	}
	input, err := termui.NewInput()
	if err != nil {
		return "", err
	}
	i, selectErr := chooseLocalItem(input, names, names, "Shell (#number/name, /filter; Enter cancels): ", "shell")
	if err := errors.Join(selectErr, input.Close()); err != nil {
		return "", err
	}
	return names[i], nil
}
