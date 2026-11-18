package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"
	"time"

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
	return negotiateSetupWithInput(r, w, o, func() (setupInput, error) { return termui.NewInput() })
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
		openInput = func() (setupInput, error) { return termui.NewInput() }
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
			log(termui.Info, "[sshore] Preparing sshore %s locally.\n", version)
			binary, err := ui.Download(ctx)
			if err != nil {
				_, _ = io.WriteString(w, "0 -\n")
				return fmt.Errorf("local release preparation failed; no shells stopped: %w", err)
			}
			if err := sendSetupBinary(w, binary); err != nil {
				return fmt.Errorf("transfer remote executable: %w", err)
			}
		case "setup_ready":
			o.daemonTask = nil
			if m.LocalRequest != nil {
				record := m.LocalRequest.Record
				o.daemonTask = &record
			}
			if m.Action == "updated" {
				log(termui.Success, "[sshore] Remote updated to %s.\n", version)
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
				action, err = waitSetupReview(ctx, *o, m, ui.Log)
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

func chooseSetupAction(m message, openInput func() (setupInput, error)) (string, error) {
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
		if p.PID > 0 {
			termui.Log(os.Stderr, termui.Warning, "  %s  PID %d  %s\n", cleanText(p.Name), p.PID, cleanText(p.Version))
		} else {
			termui.Log(os.Stderr, termui.Warning, "  %s  %s\n", cleanText(p.Name), cleanText(p.Version))
		}
	}
	if m.Action == "manual-only" {
		return "", fmt.Errorf("automatic setup unavailable; update manually; no tasks ended")
	}
	prompt := "Type update to install, or Enter to cancel: "
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
		termui.Log(os.Stderr, termui.Warning, "%s\n", strings.TrimSpace(prompt))
	}
	closeErr := input.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return "", fmt.Errorf("setup input or terminal restoration failed; no update requested: %w", err)
	}
	action := "cancel"
	if answer == "update" {
		action = "update"
	}
	return action, nil
}

// Parse stable release labels for upgrade direction; this is not a compatibility series.
func releaseSeries(v string) (string, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return "", false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", false
			}
		}
	}
	return parts[0] + "." + parts[1], true
}

func sameRelease(local, peer string) bool {
	_, canonical := releaseSeries(local)
	return canonical && local == peer
}

// Compare numeric release components without integer overflow. Custom labels
// cannot be ordered and must be handled manually when they differ.
func compareReleaseVersions(a, b string) (int, bool) {
	if _, ok := releaseSeries(a); !ok {
		return 0, false
	}
	if _, ok := releaseSeries(b); !ok {
		return 0, false
	}
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		if len(aa[i]) < len(bb[i]) {
			return -1, true
		}
		if len(aa[i]) > len(bb[i]) {
			return 1, true
		}
		if aa[i] < bb[i] {
			return -1, true
		}
		if aa[i] > bb[i] {
			return 1, true
		}
	}
	return 0, true
}

// Encode a normal fatal frame before a remote Go executable is available.
// Octal escapes keep binary framing and JSON out of shell interpolation.
func bootstrapFailure(reason string) string {
	return bootstrapFrame(message{Type: "fatal", Error: reason}) + "; exit 1"
}

func bootstrapFrame(m message) string {
	var frame bytes.Buffer
	_ = writeMessage(&frame, m)
	var escaped strings.Builder
	for _, b := range frame.Bytes() {
		fmt.Fprintf(&escaped, "\\%03o", b)
	}
	return "printf " + shellQuote(escaped.String())
}

//go:embed scripts/bootstrap.sh.tmpl
var remoteBootstrapScript string

var remoteBootstrapTemplate = template.Must(template.New("bootstrap.sh.tmpl").Funcs(template.FuncMap{
	"q":     shellQuote,
	"fatal": bootstrapFailure,
	"download": func() string {
		return bootstrapFrame(message{Type: "setup_download", SoftwareVersion: version, Action: "linux-amd64"})
	},
}).Parse(remoteBootstrapScript))

// The bootstrap runs inside the original SSH connection. A mismatched executable
// is never overwritten here: first prepare a verified, temporary coordinator so
// older installations can negotiate reviewed replacement without ending tasks first.
func remoteCommand(o options) string {
	if o.server == "" {
		o.server = "sshore"
	}
	mode := "bridge"
	if o.command == "spawn" {
		mode = "spawn"
	} else if o.command == "launch" {
		mode = "new"
	} else if o.command == "attach" {
		mode = "attach"
	} else if o.command == "mount-agent" || o.command == "cp-agent" || o.command == "mcp-agent" {
		mode = o.command
	}
	if o.list {
		mode = "sessions"
	}
	var b strings.Builder
	data := struct {
		Version, Server, Command, Mode, Session, Action, NewName, TaskIdentity, Instance string
		List, ListJSON                                                                   bool
	}{
		Version: version, Server: o.server, Command: o.command, Mode: mode, Session: o.session,
		Action: o.command + "-session", NewName: o.newName, TaskIdentity: o.taskIdentity, Instance: o.instance,
		List: o.list, ListJSON: o.listJSON,
	}
	if err := remoteBootstrapTemplate.Execute(&b, data); err != nil {
		panic(err)
	}
	return "sh -c " + shellQuote(b.String())
}

func waitSetupReview(ctx context.Context, o options, m message, log io.Writer) (string, error) {
	if m.Action != "update" {
		return "", fmt.Errorf("unsupported setup action")
	}
	installed := m.Name
	if installed == "" {
		installed = "missing"
	} else if n, ok := compareReleaseVersions(installed, version); !ok || n > 0 {
		return "", fmt.Errorf("incompatible release %s; manual repair required", installed)
	}
	connection, err := connectionFromOptions(o)
	if err != nil {
		return "", err
	}
	connection.Session, connection.Instance = "", ""
	id, err := newTaskUID()
	if err != nil {
		return "", err
	}
	r := requestRecord{ID: id, SoftwareVersion: version, Created: time.Now(), Connection: connection, Setup: &setupReview{Installed: installed, Tasks: m.SetupProcesses, Target: m.SetupTarget, Account: m.SourceMachineID}}
	_, err = requestRPCContext(ctx, requestTTL, "setup_wait", "", "", &r, func(id string) {
		fmt.Fprintf(log, "[sshore] %s needs setup. Run sshore review in another local terminal (request %s).\n", cleanText(o.host), id[:6])
	})
	if err != nil {
		return "", err
	}
	return "update", nil
}
