package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	return local != "" && local == peer
}

// Compare numeric release components without integer overflow. Custom labels
// cannot be ordered and must be handled manually when they differ.
func compareReleaseVersions(a, b string) (int, bool) {
	if a == b && a != "" {
		return 0, true
	}
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

// Bootstrap runs in the original authenticated SSH connection. Resolve the
// configured server through PATH, then ~/.local/bin/sshore for the default name;
// explicit server paths are respected. Matching versions execute directly without
// querying GitHub. A mismatched program is never overwritten during bootstrap:
// prepare a checksum/version-verified temporary coordinator using install.sh.
// Its directory is staging, not a PATH installation. Suppress successful installer
// output but retain failure diagnostics. The coordinator negotiates confirmation
// or recovery before replacing the target. A failed remote download requests a
// verified executable from the client over the same authenticated connection.

const installerURL = "https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh"

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

// Match compareVersions before downloading a coordinator. Only canonical full
// releases are ordered; failed probes and custom labels proceed to manual recovery.
const newerReleaseAWK = `END {
    release = "(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)"
    if (NR != 1 || $0 !~ ("^sshore " release "$") || local !~ ("^" release "$")) exit 1
    split(local, l, "."); split($2, r, ".")
    for (i = 1; i <= 3; i++) {
        if (length(r[i]) > length(l[i])) exit 0
        if (length(r[i]) < length(l[i])) exit 1
        if (("x" r[i]) > ("x" l[i])) exit 0
        if (("x" r[i]) < ("x" l[i])) exit 1
    }
    exit 1
}`

// The bootstrap runs inside the original SSH connection. A mismatched executable
// is never overwritten here: first prepare a verified, temporary coordinator so
// even an old server can offer an interactive, nonpersistent recovery shell.
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
	} else if o.command == "mount-agent" || o.command == "cp-agent" {
		mode = o.command
	}
	if len(o.approved) > 0 {
		mode = "resume"
	}
	if o.list {
		mode = "sessions"
	}
	var b strings.Builder
	b.WriteString("server=" + shellQuote(o.server) + "; ")
	b.WriteString("if command -v \"$server\" >/dev/null 2>&1; then server=$(command -v \"$server\"); ")
	if o.server == "sshore" {
		b.WriteString("else server=\"$HOME/.local/bin/sshore\"; ")
	} else {
		b.WriteString("else :; ")
	}
	b.WriteString("fi; case \"$server\" in /*) ;; *) server=\"$PWD/$server\" ;; esac; ")
	b.WriteString("remote_version=; if [ -e \"$server\" ]; then remote_version=$(\"$server\" --version) || remote_version=; fi; ")
	if o.list {
		b.WriteString("if [ \"$remote_version\" != " + shellQuote("sshore "+version) + " ]; then printf '%s\\n' 'sshore: inventory requires matching remote release; no software changed' >&2; exit 1; fi; exec \"$server\" inventory " + shellQuote(version))
		if o.listJSON {
			b.WriteString(" --diagnostics")
		}
		return "sh -c " + shellQuote(b.String())
	}
	// Destructive management never enters setup: a kill must not upgrade or end
	// unrelated sessions. The remote command independently checks its version.
	if o.command == "cancel" || o.command == "cancel-resource" || o.command == "rename" {
		b.WriteString("if [ \"$remote_version\" != " + shellQuote("sshore "+version) + " ]; then printf '%s\\n' " + shellQuote("sshore: "+o.command+" requires the installed remote version to match "+version+"; no software changed") + " >&2; exit 1; fi; ")
		if o.command == "cancel-resource" {
			b.WriteString("exec \"$server\" cancel-resource " + shellQuote(version) + " " + shellQuote(o.session) + " " + shellQuote(o.taskIdentity))
			return "sh -c " + shellQuote(b.String())
		}
		actionArgs := shellQuote(o.session)
		if o.command == "rename" {
			actionArgs += " " + shellQuote(o.newName)
		}
		action := o.command
		if action == "cancel" {
			action = "kill"
		}
		b.WriteString("exec \"$server\" " + action + "-session " + actionArgs + " " + shellQuote(version))
		if o.instance != "" {
			b.WriteString(" " + shellQuote(o.instance))
		}
		return "sh -c " + shellQuote(b.String())
	}
	// Check direction before downloading anything. Unknown/custom labels are left
	// to the coordinator; they never authorize an automatic destructive upgrade.
	b.WriteString("if printf '%s\\n' \"$remote_version\" | awk -v local=" + shellQuote(version) + " " + shellQuote(newerReleaseAWK) + "; then printf '%s\\n' " + shellQuote("sshore: remote executable is newer than local "+version+"; update the local client. No downgrade performed.") + " >&2; exit 1; fi; ")
	args := " bootstrap \"$server\" " + shellQuote(mode) + " " + shellQuote(o.session)
	b.WriteString("if [ \"$remote_version\" = " + shellQuote("sshore "+version) + " ]; then exec \"$server\"" + args + "; fi; ")
	b.WriteString("if [ \"$(uname -s)\" != Linux ] || [ \"$(uname -m)\" != x86_64 ]; then " + bootstrapFailure("automatic setup supports Linux amd64 only") + "; fi; ")
	b.WriteString("printf '%s\\n' " + shellQuote("[sshore] Downloading sshore "+version+" and verifying checksum...") + " >&2; ")
	b.WriteString("umask 077; setup_dir=$(mktemp -d \"${TMPDIR:-/tmp}/sshore-setup.XXXXXXXX\") || { " + bootstrapFailure("remote setup: cannot create staging directory; no shells stopped") + "; }; trap 'rm -rf -- \"$setup_dir\"' EXIT; trap 'exit 130' INT; trap 'exit 143' TERM HUP; ")
	b.WriteString("if ! { curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 60 --retry 2 " + shellQuote(installerURL) + " -o \"$setup_dir/install.sh\" && sh \"$setup_dir/install.sh\" --version " + shellQuote(version) + " --bin-dir \"$setup_dir/bin\"; } >\"$setup_dir/install.log\" 2>&1; then ")
	b.WriteString("cat \"$setup_dir/install.log\" >&2; ")
	b.WriteString(bootstrapFrame(message{Type: "setup_download", SoftwareVersion: version, Action: "linux-amd64"}) + "; ")
	b.WriteString("IFS=' ' read -r setup_size setup_hash setup_extra || { " + bootstrapFailure("remote setup: client download unavailable; no shells stopped") + "; }; ")
	b.WriteString("case \"$setup_size\" in ''|*[!0-9]*|?????????*) " + bootstrapFailure("remote setup: invalid transfer size") + ";; esac; ")
	b.WriteString("if [ \"$setup_size\" -le 0 ] || [ \"$setup_size\" -gt 67108864 ] || [ -n \"$setup_extra\" ] || [ \"${#setup_hash}\" -ne 64 ]; then " + bootstrapFailure("remote setup: client download failed or transfer invalid; no shells stopped") + "; fi; ")
	b.WriteString("case \"$setup_hash\" in *[!0-9a-f]*) " + bootstrapFailure("remote setup: invalid transfer checksum") + ";; esac; ")
	b.WriteString("mkdir -p \"$setup_dir/bin\" && head -c \"$setup_size\" > \"$setup_dir/bin/sshore\" || { " + bootstrapFailure("remote setup: transfer failed; no shells stopped") + "; }; ")
	b.WriteString("setup_actual=$(sha256sum < \"$setup_dir/bin/sshore\") || { " + bootstrapFailure("remote setup: checksum tool failed; no shells stopped") + "; }; if [ \"$(wc -c < \"$setup_dir/bin/sshore\")\" -ne \"$setup_size\" ] || [ \"${setup_actual%% *}\" != \"$setup_hash\" ]; then " + bootstrapFailure("remote setup: transferred executable checksum mismatch; no shells stopped") + "; fi; ")
	b.WriteString("chmod 700 \"$setup_dir/bin/sshore\" || { " + bootstrapFailure("remote setup: cannot prepare executable; no shells stopped") + "; }; fi; ")
	b.WriteString("[ \"$(\"$setup_dir/bin/sshore\" --version)\" = " + shellQuote("sshore "+version) + " ] || { " + bootstrapFailure("remote setup: prepared binary version mismatch. No shells stopped.") + "; }; ")
	b.WriteString("\"$setup_dir/bin/sshore\"" + args)
	return "sh -c " + shellQuote(b.String())
}
