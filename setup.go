package main

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/template"
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

type setupProcess struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Start   string `json:"start"`
}

// Return the remote home directory from the readiness reply. Shell selection
// opens terminal input only after SSH authentication.
func negotiateSetup(r io.Reader, w io.Writer, host string, chooseShell bool) (string, error) {
	return negotiateSetupWithInput(r, w, host, chooseShell, func() (setupInput, error) { return termui.NewInput() })
}

type setupInput interface {
	Read(string, bool) (string, error)
	Close() error
}

func negotiateSetupWithInput(r io.Reader, w io.Writer, host string, chooseShell bool, openInput func() (setupInput, error)) (string, error) {
	for {
		m, err := readMessage(r)
		if err != nil {
			return "", fmt.Errorf("remote setup failed: %w", err)
		}
		if m.Type == "setup_diagnostic" {
			termui.Log(os.Stderr, termui.Info, "%s\n", cleanText(strings.TrimSpace(m.Error)))
			continue
		}
		if m.Type == "fatal" {
			return "", fmt.Errorf("%s", m.Error)
		}
		if m.SoftwareVersion != version {
			return "", fmt.Errorf("Unexpected remote version %q. %s", m.SoftwareVersion, remoteVersionHelp(host))
		}
		switch m.Type {
		case "setup_ready":
			if m.Action != "managed" {
				return "", fmt.Errorf("invalid setup mode")
			}
			return m.HomeDirectory, nil
		case "session_choice":
			if !chooseShell || len(m.Targets) < 2 {
				return "", fmt.Errorf("unexpected shell selection")
			}
			names, err := sessionTargetNames(m.Targets)
			if err != nil {
				return "", err
			}
			input, err := openInput()
			if err != nil {
				return "", err
			}
			selected, selectErr := chooseSetupSession(input, names)
			closeErr := input.Close()
			if err := errors.Join(selectErr, closeErr); err != nil {
				return "", err
			}

			if err := writeMessage(w, message{Type: "session_select", Name: selected}); err != nil {
				return "", err
			}
			chooseShell = false
		default:
			return "", fmt.Errorf("unexpected setup response: %s", m.Type)
		}
	}
}

//go:embed install.sh
var linuxInstaller string

func runInstall(args []string) (int, error) {
	parsed, err := parseCLI(args, "install")
	if err != nil {
		return 2, err
	}
	host, target := "here", ""
	local, bare := true, false
	if len(parsed.operands) != 0 {
		bare = isBareSSHHost(parsed.operands[0])
		host, target, err = installTarget(parsed.operands[0])
		if err != nil {
			return 2, err
		}
		local = !bare && host == "here"
	}
	if strings.ContainsAny(target, "\x00\r\n") {
		return 2, usage("invalid installation path")
	}
	if local {
		if len(parsed.ssh) != 0 {
			return 2, usage("SSH options require a remote installation target")
		}
		if target != "" && !filepath.IsAbs(target) {
			return 2, usage("installation path must be absolute")
		}
		if parsed.has("--apply") {
			if target == "" {
				return 2, usage("applying a prepared release requires here:PATH")
			}
			return 0, installRelease(target)
		}
		if runtime.GOOS == "linux" {
			showInstallDestination(host, target)
		}
		return 0, runLocalInstaller(target)
	}
	if host == "origin" && !bare {
		return 2, usage("install requires here: or an SSH host")
	}
	if parsed.has("--apply") {
		return 2, usage("applying a prepared release requires here:PATH")
	}
	if target != "" && !strings.HasPrefix(target, "/") {
		return 2, usage("remote installation path must be absolute")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Keep SSH stdin available for kill --all confirmation after authentication.
	command := "sh -c " + shellQuote(linuxInstaller) + " install.sh --version " + shellQuote(version)
	if target != "" {
		command += " --path " + shellQuote(target)
	} else {
		// Match the executable lookup used by managed remote connections.
		command = "if target=$(command -v sshore); then case \"$target\" in /*) ;; *) target=\"$PWD/$target\";; esac; set -- --path \"$target\"; else set --; fi; " +
			command + " \"$@\""
	}
	showInstallDestination(host, target)
	connection := sshio.Options{Host: host, Args: parsed.ssh, Command: command, Confirm: confirmSSHHost, Secret: promptSSHSecret}
	if err := sshio.Run(ctx, connection, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return 1, fmt.Errorf("remote installation on %s: %w", cleanText(host), err)
	}
	return 0, nil
}

func showInstallDestination(host, target string) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	destination := remotePathOperand(host, target)
	fmt.Fprintln(os.Stderr, termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Info, "Installing sshore · "+cleanText(destination)))
}

func installTarget(value string) (string, string, error) {
	if isBareSSHHost(value) {
		if err := validSSHDestination(value); err != nil {
			return "", "", err
		}
		return value, "", nil
	}
	return splitEndpoint(value)
}

func remoteVersionHelp(host string) string {
	target := "HOST:"
	if host != "" {
		target = shellQuote(remotePathOperand(host, ""))
	}
	return "Remote sshore must match local version " + version + ". Run sshore install " + target +
		" to update the remote, or sshore install if this machine is older. For a custom remote path, use HOST:/absolute/path."
}

// Return the canonical components for exact matching and upgrade ordering.
func releaseParts(v string) ([3]string, bool) {
	major, rest, hasMinor := strings.Cut(v, ".")
	minor, patch, hasPatch := strings.Cut(rest, ".")
	if !hasMinor || !hasPatch {
		return [3]string{}, false
	}
	parts := [3]string{major, minor, patch}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return [3]string{}, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return [3]string{}, false
			}
		}
	}
	return parts, true
}

func sameRelease(local, peer string) bool {
	if local != peer {
		return false
	}
	_, canonical := releaseParts(local)
	return canonical
}

// Compare numeric release components without integer overflow. Custom labels
// cannot be ordered and must be handled manually when they differ.
func compareReleaseVersions(a, b string) (int, bool) {
	aa, ok := releaseParts(a)
	if !ok {
		return 0, false
	}
	bb, ok := releaseParts(b)
	if !ok {
		return 0, false
	}
	for i := range aa {
		if order := cmp.Or(cmp.Compare(len(aa[i]), len(bb[i])), strings.Compare(aa[i], bb[i])); order != 0 {
			return order, true
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
	"q": shellQuote,
}).Parse(remoteStartupScript))

type remoteStartup struct {
	Server, Version, Request, Mismatch, Ready string
	Helper, KeepSocket                        bool
}

func remoteCommand(o options) string {
	req := remoteServiceRequest{Name: o.session, Instance: o.instance}
	data := remoteStartup{Server: o.server}
	switch {
	case o.list:
		req = remoteServiceRequest{Operation: "inventory", Diagnostics: o.listJSON}
		data.Mismatch = remoteStartupError("sshore: inventory requires matching remote release; no software changed")
	case o.command == "daemon-admin":
		req.Operation = "daemon-admin"
	case o.command == "kill" || o.command == "rename":
		req.Operation = o.command + "-session"
		req.NewName = o.newName
		data.Mismatch = remoteStartupError(fmt.Sprintf("sshore: %s requires the installed remote version to match %s; no software changed", o.command, version))
	default:
		switch o.command {
		case "cp-agent", "mount-agent":
			req.Operation = o.command
		default:
			req.Operation, req.Mode = "shell", "bridge"
			switch o.command {
			case "launch":
				req.Mode = "new"
			case "spawn", "attach":
				req.Mode = o.command
			}
			if req.Mode == "attach" && validTaskUID(o.instance) {
				req.Name = o.instance
			}
		}
		data.Mismatch = "printf 'Remote executable: %s\\n' \"$server\" >&2\n" + startupFailure(remoteVersionHelp(o.host))
	}
	return renderRemoteCommand(data, req)
}

func remoteStartupError(reason string) string {
	return "printf '%s\\n' " + shellQuote(reason) + " >&2; exit 1"
}

func remoteHelperCommand(host, server string, req remoteServiceRequest) string {
	data := remoteStartup{Server: server, Helper: true, Mismatch: remoteStartupError(remoteVersionHelp(host))}
	switch req.Operation {
	case "task-watch":
		data.Ready = forwardReadyMarker
	case "task-remove":
		data.KeepSocket = true
		data.Mismatch = "exit 0"
	}
	return renderRemoteCommand(data, req)
}

func renderRemoteCommand(data remoteStartup, req remoteServiceRequest) string {
	data.Version = version
	args := remoteServiceArgs(req)
	data.Request = shellQuote(args[0]) + " " + shellQuote(args[1])
	var b strings.Builder
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
	var installed string
	previous, err := os.Lstat(target)
	if err == nil {
		if !previous.Mode().IsRegular() {
			return fmt.Errorf("installation target must be a regular executable: %s", target)
		}
		installed, err = executableVersion(target)
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
	record, err := existingDaemonRecord()
	running := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if installed == version && (!running || record.Version == version) {
		return nil
	}
	if running {
		order, known := compareReleaseVersions(record.Version, version)
		if !known || order > 0 {
			return fmt.Errorf("cannot update account daemon release %q to %s", cleanText(record.Version), version)
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	if err := validateInstallTarget(target); err != nil {
		return err
	}
	staged, err := stageReplacement(target)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	stagedVersion, err := executableVersion(staged)
	if err != nil {
		return fmt.Errorf("prepared executable verification failed: %w", err)
	}
	if stagedVersion != version {
		return fmt.Errorf("prepared executable reports sshore %q, expected %q", stagedVersion, version)
	}
	if err := verifyInstallTarget(target, previous); err != nil {
		return err
	}
	if runtime.GOOS == "linux" && installed != "" && installed != version {
		if err := os.Link(target, target+"-"+installed); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("back up old executable: %w", err)
		}
	}
	if running {
		if installed == record.Version {
			// The installed release also speaks its daemon's administrative protocol.
			cmd := exec.Command(target, "kill", "--all")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("stopping the account daemon: %w", err)
			}
		} else {
			reply, err := daemonAdministrationAt(context.Background(), record, "snapshot", nil)
			if err != nil {
				return fmt.Errorf("reading account daemon snapshot: %w", err)
			}
			if err := confirmDaemonShutdown("", reply.Snapshot, nil); err != nil {
				return err
			}
			if err := requestDaemonShutdown(reply.Snapshot); err != nil {
				return fmt.Errorf("stopping the account daemon: %w", err)
			}
		}
	}
	// A direct daemon invocation can race even while we hold the startup lock.
	owner, err := daemonFileLock("owner")
	if err != nil {
		return fmt.Errorf("cannot take ownership for installation: %w", err)
	}
	defer owner.Close()
	if err := verifyInstallTarget(target, previous); err != nil {
		return err
	}
	return os.Rename(staged, target)
}

func verifyInstallTarget(path string, previous os.FileInfo) error {
	current, err := os.Lstat(path)
	if os.IsNotExist(err) && previous == nil {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if previous == nil || current == nil || !os.SameFile(previous, current) {
		return fmt.Errorf("installation target changed; restart installation")
	}
	return nil
}

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
	return stageSiblingFile(target, ".sshore-update-*.exe", func(file *os.File) error {
		if _, err := io.Copy(file, source); err != nil {
			return err
		}
		return file.Chmod(0755)
	})
}
