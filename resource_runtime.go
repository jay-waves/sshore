package main

// The account daemon owns resource identity and control. Mounts isolate native
// filesystem code in a child connected by pipes; forwards run in the daemon.
import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

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
func resourceStateDir(kind string) (string, error) {
	if kind != "mount" && kind != "forward" {
		return "", fmt.Errorf("unknown resource type")
	}
	return stateDir(kind + "s")
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
func runMountDaemon() (code int, resultErr error) {
	enc := json.NewEncoder(os.Stdout)
	committed := false
	defer func() {
		if !committed && resultErr != nil {
			_ = enc.Encode(resourceStartup{Error: cleanText(resultErr.Error())})
		}
	}()

	// The daemon encodes one JSON record per line. Bound each record so a
	// long-lived control stream never exhausts a cumulative input budget.
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(nil, 1<<20)
	read := func(value any) error {
		if input.Scan() {
			return json.Unmarshal(input.Bytes(), value)
		}
		if err := input.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	var config resourceConfig
	if e := read(&config); e != nil {
		return 1, e
	}
	if config.kind() != "mount" {
		return 1, fmt.Errorf("mount worker requires mount configuration")
	}
	o := *config.Mount
	if err := validateMountConfig(o); err != nil {
		return 1, err
	}
	record := resourceRecord{Kind: "mount", Name: o.Name, Host: o.Host, ReadOnly: o.ReadOnly, Direction: "<-"}
	s, err := newResourceService(config, record)
	if err != nil {
		return 1, err
	}
	cleanup, err := prepareFTPPaths(&o)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	abandoned := make(chan struct{})
	ack := awaitResourceCommit(read, func() { close(abandoned) })
	unmount := make(chan chan error)
	var replyMu sync.Mutex
	ready := func(root, mountpoint string) error {
		s.setTransport("connected", nil)
		s.record.Remote, s.record.Mountpoint = root, mountpoint
		if err := offerResourceStartup(enc, s.record, ack, nil); err != nil {
			return err
		}
		if err := enc.Encode("started"); err != nil {
			return err
		}
		committed = true
		go s.serveWorkerControl(read, enc, unmount, &replyMu)
		return nil
	}
	defer func() {
		s.finish()
		// Let an in-flight reply finish before returning to the worker entry point.
		replyMu.Lock()
		replyMu.Unlock()
	}()
	lifecycle := &files.MountLifecycle{Abandoned: abandoned, OperationCompleted: s.operationCompleted, Ready: ready, Stop: unmount}
	return runFTPMount(o, s, lifecycle)
}

func newResourceService(config resourceConfig, record resourceRecord) (*resourceService, error) {
	if err := validSession(record.Name); err != nil {
		return nil, err
	}
	if !validTaskUID(config.UID) {
		return nil, fmt.Errorf("invalid resource task ID")
	}
	_, err := resourceStateDir(record.Kind)
	if err != nil {
		return nil, err
	}
	record.UID, record.Version = config.UID, version
	s := &resourceService{record: record, config: config, done: make(chan struct{})}
	s.setTransport("starting", nil)
	return s, nil
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

// The account daemon retains the worker pipes after commit. Losing that owner
// stops the filesystem worker, including when no task-control RPC is in flight.
func (s *resourceService) serveWorkerControl(read func(any) error, enc *json.Encoder, unmount chan<- chan error, replyMu *sync.Mutex) {
	for {
		var action string
		if err := read(&action); err != nil {
			break
		}
		replyMu.Lock()
		select {
		case <-s.done:
			replyMu.Unlock()
			return
		default:
		}
		err := enc.Encode(s.mountControl(action, unmount))
		replyMu.Unlock()
		if err != nil {
			break
		}
	}
	s.mountControl("kill", unmount)
}

func startResourceBackground(config resourceConfig) (int, error) {
	kind := config.kind()
	if kind == "" {
		return 1, fmt.Errorf("invalid resource configuration")
	}
	reservation, e := chooseTaskIdentity(kind, "")
	if e != nil {
		return 1, e
	}

	return startSelectedResource(config, reservation, "")
}
func startSelectedResource(config resourceConfig, reservation *taskIdentitySelection, directory string) (int, error) {
	kind := config.kind()
	name := reservation.name
	config.UID = reservation.uid
	switch kind {
	case "forward":
		config.Forward.Name = name
	case "mount":
		config.Mount.Name = name
	default:
		return 1, fmt.Errorf("invalid resource configuration")
	}
	if e := validSession(name); e != nil {
		return 1, e
	}
	dir, e := resourceStateDir(kind)
	if e != nil {
		return 1, e
	}
	auth, stopAuth, e := startSSHAuthBroker()
	if e != nil {
		return 1, e
	}
	defer stopAuth()
	logPath := filepath.Join(dir, config.UID+".log")
	if directory == "" {
		directory, e = os.Getwd()
		if e != nil {
			return 1, e
		}
	}
	config.Auth = auth
	c, e := openLocalDaemonContext(context.Background(), localDaemonRequest{
		Kind: "resource", Record: localShellRecord{Directory: directory},
		Env: os.Environ(), Resource: &config,
	})
	if e != nil {
		return 1, e
	}
	defer c.Close()
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	var startup resourceStartup
	if e = dec.Decode(&startup); e != nil {
		return 1, fmt.Errorf("resource startup failed; see %s: %w", logPath, e)
	}
	if startup.Error != "" {
		return 1, fmt.Errorf("%s (log: %s)", startup.Error, logPath)
	}
	record := startup.resourceRecord
	if e = commitResourceStartup(enc, dec); e != nil {
		return 1, e
	}
	detail := record.Detail
	switch kind {
	case "mount":
		mode := "read-write"
		if record.ReadOnly {
			mode = "read-only"
		}
		detail = fmt.Sprintf("%s:%s -> %s · %s", record.Host, record.Remote, record.Mountpoint, mode)
	case "forward":
		if f := config.Forward; f != nil {
			detail = f.description("local", f.Host)
		}
	}
	fmt.Fprintf(os.Stdout, "Started %s/%s: %s\n", kind, record.Name, cleanText(detail))

	return 0, nil
}

func (s *resourceService) peerTask(detail string) peerRecord {
	// Derive a stable mirror key without exposing the local control token.
	key := sha256.Sum256([]byte("peer:" + s.record.UID))
	return peerRecord{Direction: s.record.Direction, UID: s.record.UID, Version: version, Key: hex.EncodeToString(key[:16]), ID: s.record.Kind + "/" + s.record.Name, Manager: taskManagerLabel(), Detail: cleanText(detail)}
}
