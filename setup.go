package main

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
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
	"time"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

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
			termui.Log(os.Stderr, "", termui.Info, "%s\n", cleanText(strings.TrimSpace(m.Error)))
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
			recent, err := metadataRPC(metadataRequest{Action: "attach_remote_choice", Host: host, Targets: m.Targets})
			if err != nil {
				return "", err
			}
			selected := recent.Choice
			if selected == "" {
				input, err := openInput()
				if err != nil {
					return "", err
				}
				var selectErr error
				selected, selectErr = chooseSetupSession(input, names)
				if err := errors.Join(selectErr, input.Close()); err != nil {
					return "", err
				}
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

func runBootstrap(args []string) (int, error) {
	parsed, err := parseCLI(args, "bootstrap")
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
			return 0, bootstrapRelease(target)
		}
		if runtime.GOOS == "linux" {
			showInstallDestination(host, target)
		}
		return 0, runLocalInstaller(target)
	}
	if host == "origin" && !bare {
		return 2, usage("bootstrap requires here: or an SSH host")
	}
	if parsed.has("--apply") {
		return 2, usage("applying a prepared release requires here:PATH")
	}
	if target != "" && !strings.HasPrefix(target, "/") {
		return 2, usage("remote installation path must be absolute")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Keep SSH stdin available for task shutdown confirmation after authentication.
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
	connection = withSSHDiagnostics(connection, os.Stderr, nil)
	if err := sshio.Run(ctx, connection, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return 1, fmt.Errorf("remote installation on %s: %w", cleanText(host), err)
	}
	return 0, nil
}

func showInstallDestination(host, target string) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	destination := sshio.FormatEndpoint(host, target)
	fmt.Fprintln(os.Stderr, termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Info, "Resetting and reinstalling sshore · "+cleanText(destination)))
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
		target = shellQuote(sshio.FormatEndpoint(host, ""))
	}
	return "Remote sshore must match local version " + version + ". Run sshore bootstrap " + target +
		" to reset and reinstall the remote account, or sshore bootstrap if this machine is older. Bootstrap ends tasks and clears sshore state. For a custom remote path, use HOST:/absolute/path."
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

func remoteCommand(o options) string {
	req := remoteServiceRequest{Name: o.session, Instance: o.instance}
	switch {
	case o.command == "daemon-admin":
		req.Operation = "daemon-admin"
	case o.command == "kill" || o.command == "rename":
		req.Operation, req.Mode = "task-control", o.command
		req.NewName = o.newName
	default:
		switch o.command {
		case "cp-agent", "mount-agent":
			req.Operation = o.command
		default:
			req.Operation, req.Mode = "shell", "bridge"
			switch o.command {
			case "sh":
				req.Mode = "new"
			case "attach":
				req.Mode = o.command
			}
			if req.Mode == "attach" && validTaskUID(o.instance) {
				req.Name = o.instance
			}
		}
	}
	return renderRemoteCommand(o.server, req)
}

func renderRemoteCommand(server string, req remoteServiceRequest) string {
	command := "sshore"
	if server != "" && server != command {
		// Explicit executable paths are used for the Linux remote shell.
		command = shellQuote(server)
	}
	args := remoteServiceArgs(req)
	if req.Operation == "daemon-admin" {
		// Maintenance must be able to inspect older Linux releases.
		return command + " " + args[0] + " " + shellQuote(args[1])
	}
	return command + " " + args[0] + " base64:" + base64.RawURLEncoding.EncodeToString([]byte(args[1]))
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

func bootstrapRelease(target string) error {
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
	paths, err := prepareReset("bootstrap")
	if err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	for _, root := range []string{paths.state, paths.cache, paths.account} {
		if resetContains(root, filepath.Join(dir, filepath.Base(target))) {
			return fmt.Errorf("installation target is inside the sshore reset directory %s", root)
		}
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
	fmt.Fprintf(os.Stderr, "Bootstrap will end this account's sshore tasks, clear its identity, history and caches, and reinstall sshore %s at %s.\n", version, cleanText(target))
	var reply daemonAdminReply
	if running {
		reply, err = daemonAdministrationAt(context.Background(), record, "snapshot", nil)
		if err != nil {
			return fmt.Errorf("reading account daemon snapshot: %w", err)
		}
		hostname, _ := os.Hostname()
		if err := confirmDaemonShutdown(hostname, reply.Snapshot, nil); err != nil {
			return err
		}
		if err := requestDaemonShutdown(reply.Snapshot); err != nil {
			return fmt.Errorf("stopping the account daemon: %w", err)
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
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("replacing the executable before state cleanup: %w", err)
	}
	if err := paths.clear(reply.Runtime, gate, startup, owner); err != nil {
		return fmt.Errorf("installed sshore %s, but reset did not finish: %w; retry bootstrap", version, err)
	}
	return nil
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
