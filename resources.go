package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

// The account daemon owns resource identity and control. Mounts isolate native
// filesystem code in a child connected by pipes; forwards run in the daemon.
type resourceRecord struct {
	UID                                                   string `json:",omitempty"`
	Kind, Detail, Name, Host, Remote, Mountpoint, Version string
	ReadOnly                                              bool
	Direction                                             string `json:",omitempty"`
}

type resourceStartup struct {
	resourceRecord
	Error string `json:"error,omitempty"`
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

	statusMu sync.Mutex
	status   resourceStatus
	record   resourceRecord
	config   resourceConfig
	done     chan struct{}
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
func collectLocalResources(host string, withStatus bool) ([]resourceView, error) {
	snapshot, statuses, err := readTaskInventory(taskInventoryRequest{Resources: true, Status: withStatus, Host: host})
	if err != nil {
		return nil, err
	}
	return resourceViews(snapshot.Resources, statuses, withStatus), nil
}

func resourceViews(records []resourceRecord, statuses map[string]resourceStatus, withStatus bool) []resourceView {
	views := make([]resourceView, 0, len(records))
	for _, record := range records {
		view := viewRecord(record)
		if withStatus {
			switch status, ok := statuses[record.UID]; {
			case !ok:
				view.LastError = "local worker unavailable or status deadline exceeded"
			case status.Version != version:
				view.LastError = "incompatible or invalid worker status"
			default:
				view.State, view.resourceStatus = taskActive, status
			}
		}
		views = append(views, view)
	}
	slices.SortFunc(views, func(a, b resourceView) int { return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.ID, b.ID)) })
	return views
}

func printStoppedResource(kind, name, identity string) {
	switch kind {
	case "mount":
		termui.Log(os.Stdout, taskShortCode(identity), termui.Info, "Unmounted mount/%s.", name)
	case "forward":
		termui.Log(os.Stdout, taskShortCode(identity), termui.Info, "Closed forward/%s.", name)
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

// Kill-all uses administrative control, independent of normal release matching.
func runKillAll(o options) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snapshot daemonSnapshot
	var stop func() error
	var views []resourceView
	if o.host == "" {
		var err error
		snapshot, err = daemonAdmin(ctx, "snapshot", nil)
		if os.IsNotExist(err) {
			fmt.Println("No account daemon is running.")
			return 0, nil
		}
		if err != nil {
			return 1, err
		}
		stop = func() error { return requestDaemonShutdown(snapshot) }
	} else {
		var err error
		views, err = collectLocalResources(o.host, true)
		if err != nil {
			return 1, err
		}
		o.command = "daemon-admin"
		connection := withSSHDiagnostics(sshConnectionOptions(o), os.Stderr, nil)
		process, err := transportio.Start(ctx, connection, os.Stderr)
		if err != nil {
			return 1, err
		}
		defer process.CloseAndWait()
		decoder := json.NewDecoder(process)
		var reply daemonAdminReply
		if err := decoder.Decode(&reply); err != nil {
			return 1, fmt.Errorf("remote daemon administration unavailable: %w", err)
		}
		if reply.Error != "" {
			return 1, fmt.Errorf("%s", reply.Error)
		}
		snapshot = reply.Snapshot
		stop = func() error {
			if err := json.NewEncoder(process).Encode(snapshot); err != nil {
				return err
			}
			if err := decoder.Decode(&reply); err != nil {
				return err
			}
			if reply.Error != "" {
				return fmt.Errorf("%s", reply.Error)
			}
			err := process.Wait()
			return err
		}
	}
	if err := confirmDaemonShutdown(o.host, snapshot, views); err != nil {
		return 1, err
	}
	if err := stop(); err != nil {
		return 1, err
	}
	if err := stopConfirmedResources(ctx, views); err != nil {
		return 1, fmt.Errorf("remote daemon stopped; local resource cleanup: %w", err)
	}
	fmt.Println("Tasks ended; daemon stopped.")
	return 0, nil
}

func confirmDaemonShutdown(host string, snapshot daemonSnapshot, resources []resourceView) error {
	if len(snapshot.Tasks)+len(resources) == 0 {
		return nil
	}
	if host == "" {
		host = "local"
	}
	printDaemonSnapshot(os.Stdout, host, snapshot)
	if len(resources) > 0 {
		inv := inventory{Resources: slices.Clone(resources)}
		inv.setTaskReferences()
		writeTaskInventoryColor(os.Stdout, inv, "", termui.ColorEnabled(os.Stdout))
	}
	prompt := termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Warning, "End the listed tasks and stop the account daemon? [Y/n]")
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
	case "", "y", "yes", "kill":
		return true
	default:
		return false
	}
}

func printDaemonSnapshot(w io.Writer, host string, snapshot daemonSnapshot) {
	fmt.Fprintf(w, "HOST  %s · daemon %s\n", cleanText(host), cleanText(snapshot.Version))
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tNAME")
	tasks := slices.Clone(snapshot.Tasks)
	for i := range tasks {
		tasks[i].ID = strings.TrimPrefix(tasks[i].ID, "task:")
	}
	slices.SortStableFunc(tasks, func(a, b daemonTaskInfo) int { return cmp.Compare(a.ID, b.ID) })
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
		fmt.Fprintf(table, "%s\t(%s)\n", cleanText(task.ID[:width]), cleanText(task.Name))
	}
	table.Flush()
}

// Status separates local worker liveness from transport state and target health.
// Target services are never probed by listing. Exported views deliberately omit
// control capabilities, addresses, SSH options, credentials and terminal output.

type resourceStatus struct {
	Version       string    `json:"version"`
	Transport     string    `json:"transport"`
	Target        string    `json:"target"`
	Started       time.Time `json:"started,omitempty"`
	LastOperation time.Time `json:"last_operation,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

func (s *resourceService) statusSnapshot() resourceStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	snapshot := s.status
	snapshot.Version, snapshot.Target = version, "unchecked"
	return snapshot
}
func (s *resourceService) setTransport(state string, err error) {
	if s == nil {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status.Transport = state
	if err != nil {
		s.status.LastError = cleanText(err.Error())
	}
	if state == "connected" && s.status.Started.IsZero() {
		s.status.Started = time.Now().UTC()
	}
}
func (s *resourceService) operationCompleted() {
	if s == nil {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status.LastOperation = time.Now().UTC()
}

// Mount daemons receive only startup/control messages through the account daemon.
// Forwards run here and directly own their SSH child; payloads stay in SSH.
func runDaemonResource(c net.Conn, request localDaemonRequest, tasks *daemonTasks) (result error) {
	enc := json.NewEncoder(c)
	defer func() {
		if result != nil {
			termui.Log(tasks.log, canonicalTaskCode(identityFromUID(request.Resource.UID)), termui.Error, "%s", result)
			_ = enc.Encode(resourceStartup{Error: result.Error()})
		}
	}()
	config := *request.Resource
	kind := config.kind()
	if !validTaskUID(config.UID) {
		return fmt.Errorf("invalid resource task ID")
	}
	dec := json.NewDecoder(io.LimitReader(c, daemonBodyLimit))
	if kind == "forward" {
		return runDaemonForward(config, enc, dec, c, request, tasks)
	}
	log := &logStream{out: tasks.log, id: canonicalTaskCode(identityFromUID(config.UID)), level: termui.Error}
	defer log.flush()
	return runDaemonMount(config, enc, dec, c, log, request, tasks)
}

func acknowledgeResourceStartup(enc *json.Encoder, c net.Conn) {
	// The daemon owns the committed task even if the initiator loses its reply.
	_ = enc.Encode("started")
	_ = c.Close()
}

func newResourceService(config resourceConfig, record resourceRecord) *resourceService {
	record.UID, record.Version = config.UID, version
	s := &resourceService{record: record, config: config, done: make(chan struct{})}
	s.setTransport("starting", nil)
	return s
}

func awaitResourceCommit(read func(any) error, abandon func()) <-chan error {
	ack := make(chan error, 1)
	go func() {
		var answer string
		err := read(&answer)
		if err == nil && answer != "commit" {
			err = fmt.Errorf("invalid startup acknowledgement")
		}
		if err != nil {
			abandon()
		}
		ack <- err
	}()
	return ack
}

func offerResourceStartup(enc *json.Encoder, startup resourceRecord, ack <-chan error, finished <-chan struct{}) error {
	if err := enc.Encode(startup); err != nil {
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

func commitResourceStartup(enc *json.Encoder, dec *json.Decoder) error {
	if err := enc.Encode("commit"); err != nil {
		return err
	}
	var reply json.RawMessage
	if err := dec.Decode(&reply); err != nil {
		return fmt.Errorf("resource startup was not acknowledged: %w", err)
	}
	var answer string
	if json.Unmarshal(reply, &answer) == nil && answer == "started" {
		return nil
	}
	var failure resourceStartup
	if json.Unmarshal(reply, &failure) == nil && failure.Error != "" {
		return fmt.Errorf("resource startup: %s", failure.Error)
	}
	return fmt.Errorf("invalid resource startup reply")
}

func (s *resourceService) finish() {
	close(s.done)
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
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	var startup resourceStartup
	if e = dec.Decode(&startup); e != nil {
		return resourceRecord{}, fmt.Errorf("resource startup failed: %w", e)
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
	termui.Log(os.Stdout, canonicalTaskCode(resourceIdentity(record)), termui.Info, "Started %s/%s: %s", record.Kind, record.Name, cleanText(detail))
}

func (s *resourceService) peerTask(detail string) peerRecord {
	// Derive a stable mirror key without exposing the local control token.
	key := sha256.Sum256([]byte("peer:" + s.record.UID))
	return peerRecord{Direction: s.record.Direction, UID: s.record.UID, Version: version, Key: hex.EncodeToString(key[:16]), ID: s.record.Kind + "/" + s.record.Name, Manager: taskManagerLabel(), Detail: cleanText(detail)}
}
