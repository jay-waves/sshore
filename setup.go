package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
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
			return fmt.Errorf("Unexpected remote version %q. %s", m.SoftwareVersion, remoteVersionHelp(o.host))
		}
		switch m.Type {
		case "setup_ready":
			if m.Action != "managed" {
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
		default:
			return fmt.Errorf("unexpected setup response: %s", m.Type)
		}
	}
}

//go:embed install.sh
var linuxInstaller string

func runSetup(args []string) (int, error) {
	parsed, err := parseCLI(args, "setup")
	if err != nil {
		return 2, err
	}
	host, target := parsed.operand(0, ""), parsed.value("--path", "")
	if host == "" {
		if len(parsed.ssh) != 0 {
			return 2, usage("SSH options require a setup host")
		}
		if target != "" && !filepath.IsAbs(target) {
			return 2, usage("--path must be an absolute installation path")
		}
		if parsed.has("--apply") {
			if target == "" {
				return 2, usage("applying a prepared release requires --path")
			}
			return 0, installRelease(target)
		}
		return 0, runLocalInstaller(target)
	}
	if parsed.has("--apply") {
		return 2, usage("applying a prepared release requires a local installation path")
	}
	if err := validSSHDestination(host); err != nil {
		return 2, err
	}
	if target != "" && !strings.HasPrefix(target, "/") {
		return 2, usage("--path must be an absolute remote installation path")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	command := "sh -s -- --version " + shellQuote(version)
	if target != "" {
		command += " --path " + shellQuote(target)
	} else {
		// Match the executable lookup used by managed remote connections.
		command = "if target=$(command -v sshore); then case \"$target\" in /*) ;; *) target=\"$PWD/$target\";; esac; set -- --path \"$target\"; else set --; fi; " +
			command + " \"$@\""
	}
	connection := sshio.Options{Host: host, Args: parsed.ssh, Command: command, Confirm: confirmSSHHost, Secret: promptSSHSecret}
	if err := sshio.Run(ctx, connection, strings.NewReader(linuxInstaller), os.Stdout, os.Stderr); err != nil {
		return 1, fmt.Errorf("remote setup on %s: %w", cleanText(host), err)
	}
	return 0, nil
}

func remoteVersionHelp(host string) string {
	if host == "" {
		host = "HOST"
	} else {
		host = shellQuote(host)
	}
	return "Remote sshore must match local version " + version + ". Run sshore setup " + host +
		" to update an older remote installation, or sshore setup if this machine is older. Use --path for a custom remote installation."
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
func startupFailure(reason string) string {
	return startupFrame(message{Type: "fatal", Error: reason}) + "; exit 1"
}

func startupFrame(m message) string {
	var frame bytes.Buffer
	_ = writeMessage(&frame, m)
	var escaped strings.Builder
	for _, b := range frame.Bytes() {
		fmt.Fprintf(&escaped, "\\%03o", b)
	}
	return "printf " + shellQuote(escaped.String())
}

//go:embed scripts/remote.sh.tmpl
var remoteStartupScript string

var remoteStartupTemplate = template.Must(template.New("remote.sh.tmpl").Funcs(template.FuncMap{
	"q": shellQuote, "fatal": startupFailure, "versionHelp": remoteVersionHelp,
}).Parse(remoteStartupScript))

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
		Version, Host, Server, Command, Mode, Session, Action, NewName, TaskIdentity, Instance string
		List, ListJSON                                                                         bool
	}{
		Version: version, Host: o.host, Server: o.server, Command: o.command, Mode: mode, Session: o.session,
		Action: o.command + "-session", NewName: o.newName, TaskIdentity: o.taskIdentity, Instance: o.instance,
		List: o.list, ListJSON: o.listJSON,
	}
	if err := remoteStartupTemplate.Execute(&b, data); err != nil {
		panic(err)
	}
	return "sh -c " + shellQuote(b.String())
}

func executableVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, "sshore ") || strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("unrecognized version response")
	}
	return strings.TrimPrefix(line, "sshore "), nil
}

// Serialize executable checks and replacement across the account.
func setupLock() (*os.File, error) {
	owner, err := daemonDirectory()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(owner)
	if err := privateStatePath(dir, true); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "setup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = privateStatePath(file.Name(), false); err == nil {
		err = lockResourceState(file)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func requireInstalledRelease(target string) error {
	installed, err := executableVersion(target)
	if err != nil || installed != version {
		return errors.New(remoteVersionHelp(""))
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Version != version {
		return fmt.Errorf("account daemon runs sshore %s. Use that release to run sshore kill --all and wait for it to exit before using %s", record.Version, version)
	}
	return nil
}

func installRelease(target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("installation target must be absolute")
	}
	gate, err := setupLock()
	if err != nil {
		return err
	}
	defer gate.Close()
	startup, err := daemonFileLock("startup")
	if err != nil {
		return err
	}
	defer startup.Close()
	// Hold the owner lock through publication, including against a direct daemon invocation.
	owner, err := daemonFileLock("owner")
	if resourceLockBusy(err) {
		return fmt.Errorf("account daemon is still running. Use its installed release to run sshore kill --all, wait for it to exit, then retry")
	}
	if err != nil {
		return err
	}
	defer owner.Close()

	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("installation target must be a regular executable: %s", target)
		}
		installed, err := executableVersion(target)
		if installed == version {
			return nil
		}
		order, known := compareReleaseVersions(installed, version)
		if err != nil || !known {
			return fmt.Errorf("unknown installed release; update manually")
		}
		if order > 0 {
			return fmt.Errorf("installed sshore %s is newer than %s", installed, version)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	staged, err := prepareReplacement(target)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	return os.Rename(staged, target)
}

// Copy and verify in the destination filesystem before any task is stopped.
func stageReplacement(target string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer source.Close()
	file, err := os.CreateTemp(filepath.Dir(target), ".sshore-update-*.exe")
	if err != nil {
		return "", err
	}
	path := file.Name()
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if _, err = io.Copy(file, source); err != nil {
		return "", err
	}
	if err = file.Chmod(0755); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if v, err := executableVersion(path); err != nil || v != version {
		return "", fmt.Errorf("prepared executable verification failed: %v", err)
	}
	success = true
	return path, nil
}
