package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/jay-waves/sshore/internal/termui"
	"github.com/jay-waves/sshore/internal/wire"
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

type setupState struct {
	Installed string
	Daemon    *daemonSnapshot
	Manual    bool
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

// One lock per account, independent of custom runtime directories. Existing
// attached sessions remain usable while another client reviews the prompt.
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

func inspectSetup(target string) (setupState, error) {
	var state setupState
	if _, err := os.Lstat(target); err == nil {
		state.Installed, err = executableVersion(target)
		if err != nil {
			state.Installed = "unknown"
			state.Manual = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	if state.Installed != "" {
		order, known := compareReleaseVersions(state.Installed, version)
		if known && order > 0 {
			return state, fmt.Errorf("remote executable %s is newer than client %s; update the local client", state.Installed, version)
		}
		if !known {
			state.Manual = true
		}
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("account daemon unavailable; no update performed: %w", err)
	}
	if record.Version != version {
		return state, fmt.Errorf("account daemon runs sshore %s; use that release to end its tasks and wait for the daemon to exit before installing %s", record.Version, version)
	}
	// Use the checked locator so a replacement daemon cannot change which
	// version receives this request between discovery and connection.
	reply, err := daemonAdministrationAt(context.Background(), record, "snapshot", nil)
	if err != nil {
		return state, fmt.Errorf("account daemon unavailable; no update performed: %w", err)
	}
	snapshot := reply.Snapshot
	if err := checkPeerVersion(snapshot.Version, "daemon"); err != nil {
		return state, err
	}
	if state.Installed != version && len(snapshot.Tasks) > 0 {
		state.Daemon = &snapshot
	}
	return state, nil
}

// The review list is a view of the authenticated daemon snapshot.
func (s setupState) processes() []setupProcess {
	if s.Daemon == nil {
		return nil
	}
	processes := []setupProcess{{Name: "daemon", Start: s.Daemon.Instance, Version: s.Daemon.Version}}
	for _, task := range s.Daemon.Tasks {
		processes = append(processes, setupProcess{Name: task.Name, Version: s.Daemon.Version, Start: task.ID})
	}
	return processes
}

// The verified release coordinates both standalone installers and SSH setup.
// Preparation never ends work; only a confirmed, rechecked snapshot can do that.
func installRelease(target string) error {
	return installPreparedRelease(target, false, func(m message) (string, error) {
		return chooseSetupAction(m, installerInput)
	})
}

func installPreparedRelease(target string, reviewInstall bool, confirm func(message) (string, error)) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("installation target must be absolute")
	}
	lock, err := setupLock()
	if err != nil {
		return err
	}
	state, err := inspectSetup(target)
	if err != nil {
		lock.Close()
		return err
	}
	if state.Manual {
		lock.Close()
		return fmt.Errorf("unknown installed or daemon version; update manually; no tasks ended")
	}
	if state.Installed == version && state.Daemon == nil {
		return lock.Close()
	}
	staged, err := prepareReplacement(target)
	lock.Close()
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	for {
		if state.Daemon != nil && len(state.Daemon.Tasks) > 0 && (os.Getenv("SSHORE_LOCAL_SESSION") != "" || os.Getenv("SSHORE_SOCKET") != "") {
			return fmt.Errorf("run the installer outside managed shells; no tasks ended")
		}
		if reviewInstall || state.Installed != "" && state.Installed != version || state.Daemon != nil {
			processes := state.processes()
			action, err := confirm(message{Action: "update", Name: state.Installed, SetupTarget: target, Error: "Prepared sshore " + version + ". Updating ends the listed tasks; unsaved work may be lost.", SetupProcesses: processes})
			if err != nil {
				return err
			}
			if action != "update" {
				return fmt.Errorf("installation cancelled; no tasks ended")
			}
		}
		lock, err := setupLock()
		if err != nil {
			return err
		}
		startup, err := daemonFileLock("startup")
		if err != nil {
			lock.Close()
			return err
		}
		current, err := inspectSetup(target)
		if err != nil {
			startup.Close()
			lock.Close()
			return err
		}
		if !reflect.DeepEqual(state, current) {
			startup.Close()
			lock.Close()
			state = current
			if state.Manual {
				return fmt.Errorf("installation changed; update manually; no tasks ended")
			}
			if state.Installed == version && state.Daemon == nil {
				return nil
			}
			fmt.Fprintln(os.Stderr, "Tasks changed; review again.")
			continue
		}
		// Revalidate the prepared executable under the account setup lock.
		if v, err := executableVersion(staged); err != nil || v != version {
			startup.Close()
			lock.Close()
			return fmt.Errorf("prepared executable verification failed; no tasks ended")
		}
		if state.Daemon != nil {
			err = requestDaemonShutdown(*state.Daemon)
		}
		if err == nil {
			err = os.Rename(staged, target)
		}
		startup.Close()
		lock.Close()
		if err != nil {
			return fmt.Errorf("installation stopped: %w", err)
		}
		return nil
	}
}

// Shell-script downloads may own stdin. Read approval from the controlling
// terminal on Linux; Windows' installer already runs in the invoking console.
type installerPrompt struct {
	reader *bufio.Reader
	close  func() error
}

func (p *installerPrompt) Read(prompt string, _ bool) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := p.reader.ReadString('\n')
	return strings.TrimSpace(line), err
}
func (p *installerPrompt) Close() error { return p.close() }
func installerInput() (setupInput, error) {
	if runtime.GOOS == "linux" {
		file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("update confirmation requires a terminal; no tasks ended: %w", err)
		}
		return &installerPrompt{reader: bufio.NewReader(file), close: file.Close}, nil
	}
	return &installerPrompt{reader: bufio.NewReader(os.Stdin), close: func() error { return nil }}, nil
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

const maxSetupDownload = 64 * 1024 * 1024

func localSetupBinary(ctx context.Context) ([]byte, error) {
	if _, ok := releaseSeries(version); !ok {
		return nil, fmt.Errorf("setup requires a full published release")
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	cache := filepath.Join(root, "sshore", "releases", version)
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, err
	}
	if err := privateStatePath(cache, true); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(cache, "prepare-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := prepareLinuxRelease(ctx, stage); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(stage, "sshore"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxSetupDownload+1))
}

func sendSetupBinary(w io.Writer, binary []byte) error {
	if len(binary) == 0 || len(binary) > maxSetupDownload {
		return fmt.Errorf("invalid setup executable size")
	}
	if _, err := fmt.Fprintf(w, "%d %x\n", len(binary), sha256.Sum256(binary)); err != nil {
		return err
	}
	return wire.WriteFull(w, binary)
}

// Shell read and head consume exactly the header and file, leaving the same
// authenticated stream positioned at the coordinator's next framed message.
func relaySetupBinary(dst io.Writer, src io.Reader) error {
	var header []byte
	for len(header) < 100 {
		var b [1]byte
		if _, err := io.ReadFull(src, b[:]); err != nil {
			return err
		}
		header = append(header, b[0])
		if b[0] != '\n' {
			continue
		}
		if string(header) == "0 -\n" {
			return wire.WriteFull(dst, header)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 2 {
			break
		}
		size, err := strconv.ParseInt(fields[0], 10, 64)
		digest, hashErr := hex.DecodeString(fields[1])
		if err != nil || size <= 0 || size > maxSetupDownload || hashErr != nil || len(digest) != sha256.Size {
			break
		}
		if err := wire.WriteFull(dst, header); err != nil {
			return err
		}
		_, err = io.CopyN(dst, src, size)
		return err
	}
	return fmt.Errorf("invalid setup transfer header")
}
