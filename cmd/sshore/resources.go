package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/files/dirlist"
	deployment "github.com/jay-waves/sshore/internal/release/remote"
	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
)

// The account daemon owns resource identity and control. Mounts isolate native
// filesystem code in a child connected by pipes; forwards run in the daemon.
type resourceRecord struct {
	UID                                          string `json:",omitempty"`
	Kind, Detail, Name, Host, Remote, Mountpoint string
	ReadOnly                                     bool
	Direction                                    string `json:",omitempty"`
}

type resourceStartup struct {
	resourceRecord
	Error   string            `json:"error,omitempty"`
	Browse  bool              `json:"browse,omitempty"`
	Home    string            `json:"home,omitempty"`
	Listing *mountPathListing `json:"listing,omitempty"`
}

type resourceConfig struct {
	UID     string `json:",omitempty"`
	Mount   *ftpOptions
	Forward *forwardConfig
	Auth    *sshAuthEndpoint `json:",omitempty"`
}

func (c resourceConfig) kind() string {
	switch {
	case c.Mount != nil && c.Forward == nil:
		return "mount"
	case c.Forward != nil && c.Mount == nil:
		return "forward"
	default:
		return ""
	}
}

type resourceService struct {
	log         io.Writer
	environment []string
	directory   string

	record resourceRecord
	auth   *sshAuthEndpoint
}

func resourceToken() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
func (s *resourceService) baseEnvironment() []string {
	if s.environment != nil {
		return s.environment
	}
	return os.Environ()
}
func (s *resourceService) logWriter() io.Writer {
	if s.log != nil {
		return s.log
	}
	return os.Stderr
}
func printStoppedResource(kind, name, identity string) {
	switch kind {
	case "mount":
		termui.Log(os.Stdout, shortTaskObservation(identity), termui.Info, "Unmounted mount/%s.", name)
	case "forward":
		termui.Log(os.Stdout, shortTaskObservation(identity), termui.Info, "Closed forward/%s.", name)
	}
}

func stopConfirmedResources(ctx context.Context, views []resourceView) error {
	if len(views) == 0 {
		return nil
	}
	changes := make([]resourceMutation, len(views))
	for i, view := range views {
		kind, name, _ := strings.Cut(view.ID, "/")
		changes[i] = resourceMutation{Kind: kind, Ref: name, Identity: view.Identity}
	}
	reply, err := daemonResourceRequest(ctx, "/resources/stop-confirmed", changes)
	if err != nil {
		return err
	}
	if len(reply.Failures) != len(changes) {
		return fmt.Errorf("invalid resource cancellation response")
	}
	var failures []error
	for i, failure := range reply.Failures {
		change := changes[i]
		if failure != "" {
			failures = append(failures, fmt.Errorf("%s/%s: %s", change.Kind, change.Ref, failure))
		} else {
			printStoppedResource(change.Kind, change.Ref, change.Identity)
		}
	}
	return errors.Join(failures...)
}

func runKillAll(o options, reset bool) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	if o.host == "" {
		err = killLocalAccount(ctx, reset)
	} else {
		err = killRemoteAccount(ctx, o, reset)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func killLocalAccount(ctx context.Context, reset bool) error {
	var startup *sys.FileLock
	if reset {
		if err := requireLocalMaintenance("kill --all --reset"); err != nil {
			return err
		}
		var err error
		startup, err = daemonFileLock("startup")
		if err != nil {
			return err
		}
		defer startup.Close()
	}
	snapshot, err := readDaemonSnapshot(ctx)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := confirmDaemonShutdown("", snapshot, nil, reset); err != nil {
		return err
	}
	var paths resetPaths
	if reset {
		paths, err = prepareReset()
		if err != nil {
			return err
		}
	}
	if snapshot.Instance != "" {
		if err := requestDaemonShutdown(snapshot); err != nil {
			return err
		}
	}
	if reset {
		owner, err := daemonFileLock("owner")
		if err != nil {
			return err
		}
		defer owner.Close()
		if err := paths.clear(startup, owner); err != nil {
			return err
		}
		fmt.Println("Local sshore state and caches cleared.")
	} else if snapshot.Instance == "" {
		fmt.Println("No account daemon is running.")
	} else {
		fmt.Println("Tasks ended; daemon stopped.")
	}
	return nil
}

func killRemoteAccount(ctx context.Context, o options, reset bool) error {
	var local []resourceView
	record, err := existingDaemonRecord()
	if err == nil {
		err = daemonJSON(ctx, record, "/resources/for-host", o.host, &local)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return err
	}
	request := remoteServiceRequest{Operation: "daemon-admin"}
	if reset {
		request.Mode = "reset"
	}
	connection := remoteService(sshConnectionOptions(o), request)
	connection = withSSHDiagnostics(connection, os.Stderr, nil)
	client, err := transportio.Dial(ctx, connection)
	if err != nil {
		return err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	process, err := connection.OpenCommand(ctx, client, "", os.Stderr)
	if err != nil && !errors.Is(err, deployment.ErrMissing) {
		return err
	}
	var snapshot daemonSnapshot
	var decoder *jsontext.Decoder
	if process != nil {
		defer process.CloseAndWait()
		decoder = jsontext.NewDecoder(process)
		reply, err := readDaemonAdminReply(decoder)
		if err != nil {
			return err
		}
		snapshot = reply.Snapshot
	}
	if err := confirmDaemonShutdown(o.host, snapshot, local, reset); err != nil {
		return err
	}
	if process != nil && (snapshot.Instance != "" || reset) {
		if err := writeControlJSON(process, true); err != nil {
			return err
		}
		if _, err := readDaemonAdminReply(decoder); err != nil {
			return err
		}
		if err := process.Wait(); err != nil {
			return err
		}
	}
	if err := stopConfirmedResources(ctx, local); err != nil {
		return fmt.Errorf("local resource cleanup: %w", err)
	}
	if reset && process != nil {
		if err := removeRemoteServer(ctx, client); err != nil {
			return err
		}
		fmt.Println("Remote sshore server and data cleared.")
	} else if snapshot.Instance == "" {
		fmt.Println("No remote daemon is running.")
	} else {
		fmt.Println("Tasks ended; daemon stopped.")
	}
	return nil
}

func readDaemonAdminReply(decoder *jsontext.Decoder) (daemonAdminReply, error) {
	var reply daemonAdminReply
	if err := json.UnmarshalDecode(decoder, &reply); err != nil {
		return reply, fmt.Errorf("remote daemon administration unavailable: %w", err)
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("%s", reply.Error)
	}
	return reply, nil
}

func confirmDaemonShutdown(host string, snapshot daemonSnapshot, resources []resourceView, reset bool) error {
	if !reset && len(snapshot.Tasks)+len(resources) == 0 {
		return nil
	}
	local := host == ""
	if local {
		host = "local"
	}
	if len(snapshot.Tasks) > 0 {
		printDaemonSnapshot(os.Stdout, host, snapshot)
	}
	if len(resources) > 0 {
		writeList(os.Stdout, inventory{Resources: resources}, "", termui.ColorEnabled(os.Stdout))
	}
	question := "End the listed tasks and stop the daemon? [y/N]"
	if reset {
		question = "End tasks and clear local sshore state and caches? [y/N]"
		if !local {
			question = fmt.Sprintf("End tasks and delete this client's sshore %s server and state on %s? [y/N]", version, cleanText(host))
		}
	}
	prompt := termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Warning, question)
	fmt.Fprint(os.Stderr, prompt+" ")
	if !confirmKillAll(os.Stdin) {
		return fmt.Errorf("cancelled; no tasks ended")
	}
	return nil
}

func confirmKillAll(input io.Reader) bool {
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "kill":
		return true
	default:
		return false
	}
}

func printDaemonSnapshot(w *os.File, host string, snapshot daemonSnapshot) {
	fmt.Fprintf(w, "HOST  %s · daemon %s\n", cleanText(host), cleanText(snapshot.Version))
	table := [][]termui.TableCell{{{Text: "ID"}, {Text: "TYPE"}}}
	tasks := slices.Clone(snapshot.Tasks)
	for i := range tasks {
		tasks[i].ID = strings.TrimPrefix(tasks[i].ID, "task:")
	}
	for i, task := range tasks {
		width := min(6, len(task.ID))
		for _, neighbor := range []int{i - 1, i + 1} {
			if neighbor < 0 || neighbor >= len(tasks) {
				continue
			}
			for width < len(task.ID) && strings.HasPrefix(tasks[neighbor].ID, task.ID[:width]) {
				width++
			}
		}
		table = append(table, []termui.TableCell{{Text: taskDisplayID(task.ID[:width], task.Name, false)}, {Text: task.Kind}})
	}
	termui.WriteTable(w, table, 0, -1, termui.ColorEnabled(w))
}

// Mount daemons receive only startup/control messages through the account daemon.
// Forwards run here and directly own their SSH child; payloads stay in SSH.
func runDaemonResource(c net.Conn, request localDaemonRequest, tasks *daemonTasks) (result error) {
	enc := jsontext.NewEncoder(c)
	defer func() {
		if result != nil {
			termui.Log(tasks.log, canonicalTaskCode(taskKey(request.Resource.UID)), termui.Error, "%s", result)
			_ = json.MarshalEncode(enc, resourceStartup{Error: result.Error()})
		}
	}()
	config := *request.Resource
	kind := config.kind()
	if !validTaskUID(config.UID) {
		return fmt.Errorf("invalid resource task ID")
	}
	dec := jsontext.NewDecoder(io.LimitReader(c, daemonBodyLimit))
	if kind == "forward" {
		return runDaemonForward(config, enc, dec, c, request, tasks)
	}
	log := termui.NewLogWriter(tasks.log, canonicalTaskCode(taskKey(config.UID)), termui.Error)
	defer log.Flush()
	return runDaemonMount(config, enc, dec, c, log, request, tasks)
}

func acknowledgeResourceStartup(enc *jsontext.Encoder, c net.Conn) {
	// The daemon owns the committed task even if the initiator loses its reply.
	_ = json.MarshalEncode(enc, resourceCommitReply{Started: true})
	_ = c.Close()
}

func awaitResourceCommit(read func(any) error, abandon func()) <-chan error {
	ack := make(chan error, 1)
	go func() {
		ack <- readResourceCommit(read, abandon)
	}()
	return ack
}

func readResourceCommit(read func(any) error, abandon func()) error {
	var answer string
	err := read(&answer)
	if err == nil && answer != "commit" {
		err = fmt.Errorf("invalid startup acknowledgement")
	}
	if err != nil {
		abandon()
	}
	return err
}

func offerResourceStartup(enc *jsontext.Encoder, startup resourceRecord, ack <-chan error, finished <-chan struct{}) error {
	if err := json.MarshalEncode(enc, startup); err != nil {
		return err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err := <-ack:
		return err
	case <-finished:
		return fmt.Errorf("resource worker exited before startup acknowledgement")
	case <-timer.C:
		return fmt.Errorf("resource startup acknowledgement timed out")
	}
}

type resourceCommitReply struct {
	Started bool   `json:"started,omitzero"`
	Error   string `json:"error,omitempty"`
}

func commitResourceStartup(enc *jsontext.Encoder, dec *jsontext.Decoder) error {
	if err := json.MarshalEncode(enc, "commit"); err != nil {
		return err
	}
	var reply resourceCommitReply
	if err := json.UnmarshalDecode(dec, &reply); err != nil {
		return fmt.Errorf("resource startup was not acknowledged: %w", err)
	}
	if reply.Started {
		return nil
	}
	if reply.Error != "" {
		return fmt.Errorf("resource startup: %s", reply.Error)
	}
	return fmt.Errorf("invalid resource startup reply")
}

func startResourceBackground(config resourceConfig) (int, error) {
	record, err := launchResourceBackground(config, "")
	if err != nil {
		return 1, err
	}
	reportResourceStartup(record)
	return 0, nil
}

func launchResourceBackground(config resourceConfig, directory string) (resourceRecord, error) {
	config.UID = newTaskUID()
	auth, stopAuth, e := startSSHAuthBroker()
	if e != nil {
		return resourceRecord{}, e
	}
	defer stopAuth()
	if directory == "" {
		directory, e = os.Getwd()
		if e != nil {
			return resourceRecord{}, e
		}
	}
	config.Auth = auth
	c, e := openLocalDaemonContext(context.Background(), localDaemonRequest{
		Kind: "resource", Record: localShellRecord{Directory: directory},
		Env: os.Environ(), Resource: &config,
	})
	if e != nil {
		return resourceRecord{}, e
	}
	defer c.Close()
	enc, dec := jsontext.NewEncoder(c), jsontext.NewDecoder(c)
	var startup resourceStartup
	if e = json.UnmarshalDecode(dec, &startup); e != nil {
		return resourceRecord{}, fmt.Errorf("resource startup failed: %w", e)
	}
	if startup.Browse {
		if config.Mount == nil {
			return resourceRecord{}, fmt.Errorf("unexpected mount selection")
		}
		browseCtx, stopBrowse := context.WithCancelCause(context.Background())
		defer stopBrowse(nil)
		replies := make(chan resourceStartup, 1)
		go func() {
			for {
				var reply resourceStartup
				if err := json.UnmarshalDecode(dec, &reply); err != nil {
					stopBrowse(err)
					return
				}
				if reply.Error != "" {
					stopBrowse(errors.New(reply.Error))
					return
				}
				select {
				case replies <- reply:
				case <-browseCtx.Done():
					return
				}
				if reply.Listing == nil {
					return
				}
			}
		}()
		var requestMu sync.Mutex
		read := func(ctx context.Context, name, parent string) ([]dirlist.Entry, error, []dirlist.Entry, error) {
			requestMu.Lock()
			defer requestMu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err, nil, err
			}
			if err := json.MarshalEncode(enc, mountPathRequest{Directory: &name, Parent: parent}); err != nil {
				return nil, err, nil, err
			}
			select {
			case reply := <-replies:
				if reply.Listing == nil {
					err := errors.New("mount startup ended before replying to the directory request")
					stopBrowse(err)
					return nil, err, nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, err, nil, err
				}
				listing := reply.Listing
				return listing.Entries, listingError(listing.Error), listing.ParentEntries, listingError(listing.ParentError)
			case <-browseCtx.Done():
				err := context.Cause(browseCtx)
				return nil, err, nil, err
			}
		}
		var remote, local string
		err := termui.RunLocalConsole(func(console *termui.LocalConsole) error {
			var err error
			remote, local, err = config.Mount.selectMountPaths(browseCtx, console, startup.Home, read)
			return err
		})
		if err != nil {
			if cause := context.Cause(browseCtx); cause != nil {
				return resourceRecord{}, cause
			}
			return resourceRecord{}, err
		}
		requestMu.Lock()
		err = json.MarshalEncode(enc, mountPathRequest{Remote: remote, Local: local})
		requestMu.Unlock()
		if err != nil {
			return resourceRecord{}, err
		}
		select {
		case startup = <-replies:
			if startup.Listing != nil {
				return resourceRecord{}, fmt.Errorf("unexpected directory listing after mount selection")
			}
		case <-browseCtx.Done():
			return resourceRecord{}, context.Cause(browseCtx)
		}
	}
	if startup.Error != "" {
		return resourceRecord{}, fmt.Errorf("%s", startup.Error)
	}
	if e = commitResourceStartup(enc, dec); e != nil {
		return resourceRecord{}, e
	}
	return startup.resourceRecord, nil
}

func reportResourceStartup(record resourceRecord) {
	detail := record.Detail
	if record.Kind == "mount" {
		mode := "read-write"
		if record.ReadOnly {
			mode = "read-only"
		}
		detail = fmt.Sprintf("%s:%s -> %s · %s", record.Host, record.Remote, record.Mountpoint, mode)
	}
	termui.Log(os.Stdout, canonicalTaskCode(taskKey(record.UID)), termui.Info, "Started %s/%s: %s", record.Kind, record.Name, cleanText(detail))
}

func (s *resourceService) peerTask(detail string) peerRecord {
	// Derive a stable mirror key without exposing the local control token.
	key := sha256.Sum256([]byte("peer:" + s.record.UID))
	return peerRecord{Direction: s.record.Direction, UID: s.record.UID, Version: version, Key: hex.EncodeToString(key[:16]), ID: s.record.Kind + "/" + s.record.Name, Manager: taskManagerLabel(), Detail: cleanText(detail)}
}
