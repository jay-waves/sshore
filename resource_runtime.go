package main

// The account daemon owns resource identity and control. Mounts isolate native
// filesystem code in a child connected by pipes; forwards run in the daemon.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/files"
)

type resourceRecord struct {
	UID                                                                   string `json:",omitempty"`
	Kind, Detail, Name, Host, Remote, Mountpoint, Address, Token, Version string
	ReadOnly                                                              bool
	Direction                                                             string `json:",omitempty"`
	slot                                                                  string
}

func (r resourceRecord) storageName() string {
	if validTaskUID(r.UID) {
		return r.UID
	}
	if r.slot != "" {
		return r.slot
	}
	return r.Name
}

type resourceStartup struct {
	resourceRecord
	Error string `json:"error,omitempty"`
}

type resourceConfig struct {
	UID                    string `json:",omitempty"`
	MountName              string
	MountApproval          *files.MountApproval
	MountConnection        *recentConnection
	Forward                *forwardConfig
	Args                   []string
	AuthAddress, AuthToken string
}
type resourceService struct {
	startup     io.Writer
	log         io.Writer
	environment []string
	directory   string
	committed   func()

	controlMu  sync.Mutex
	statusMu   sync.Mutex
	status     resourceStatus
	record     resourceRecord
	config     resourceConfig
	stop       chan chan error
	ack        <-chan error
	abandoned  <-chan struct{}
	done       chan struct{}
	publish    func(resourceRecord)
	recordPath string
}

func resourceToken() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func resourceStateDir(kind string) (string, error) {
	if kind != "mount" && kind != "forward" {
		return "", fmt.Errorf("unknown resource type")
	}
	return stateDir(kind + "s")
}

func resourceHTTPContext(ctx context.Context, address, token, method, route string, body io.Reader) (*http.Response, error) {
	host, _, e := net.SplitHostPort(address)
	if e != nil || host != "127.0.0.1" {
		return nil, fmt.Errorf("invalid resource control address")
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://"+address+route, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	timeout := 20 * time.Second
	if route == "/status" {
		timeout = time.Second
	}
	if route == "/ask" || route == "/setup" || route == "/unlock" {
		timeout = 0
	}
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer c.CloseIdleConnections()
	return c.Do(req)
}
func resourceListen(token string, handler http.HandlerFunc) (net.Listener, *http.Server, error) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return nil, nil, e
	}
	s := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		handler(w, r)
	})}
	go s.Serve(l)
	return l, s, nil
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
func (s *resourceService) startupWriter() io.Writer {
	if s.startup != nil {
		return s.startup
	}
	return os.Stdout
}
func (s *resourceService) sshEnvironment() []string {
	return append(append([]string(nil), s.baseEnvironment()...), "SSHORE_CONFIRM_ADDR="+s.config.AuthAddress, "SSHORE_CONFIRM_TOKEN="+s.config.AuthToken)
}
func (s *resourceService) ready(root, mountpoint string) error {
	s.setTransport("connected", nil)
	s.record.Remote = root
	s.record.Mountpoint = mountpoint
	if s.publish != nil {
		s.publish(s.record)
	}
	var e error
	if e = json.NewEncoder(s.startupWriter()).Encode(s.record); e != nil {
		return e
	}

	select {
	case e = <-s.ack:
	case <-time.After(10 * time.Second):
		e = fmt.Errorf("resource startup acknowledgement timed out")
	}
	if e != nil {
		return e
	}
	err := json.NewEncoder(s.startupWriter()).Encode("started")
	if err == nil && s.committed != nil {
		s.committed()
	}
	return err
}
func runMountDaemon() (code int, resultErr error) {
	defer func() {
		if resultErr != nil {
			_ = json.NewEncoder(os.Stdout).Encode(resourceStartup{Error: cleanText(resultErr.Error())})
		}
	}()

	dec := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	var config resourceConfig
	if e := dec.Decode(&config); e != nil {
		return 1, e
	}
	return runConfiguredResource(config, dec, os.Stdout, os.Stderr, os.Environ(), "", nil, nil)
}

func runConfiguredResource(config resourceConfig, dec *json.Decoder, startup, log io.Writer, env []string, directory string, committed func(), tasks *daemonTasks) (code int, resultErr error) {
	kind := "mount"
	var o ftpOptions
	var e error
	if config.Forward != nil {
		kind = "forward"
		if e = validateForwardConfig(*config.Forward); e != nil {
			return 1, e
		}
		o.host = config.Forward.Host
		o.name = config.Forward.Name
	} else {
		o, e = parseFTPOptions(config.Args)
		o.name = config.MountName
		o.approval = config.MountApproval
		if config.MountConnection != nil {
			if err := validateRecentConnection(*config.MountConnection); err != nil {
				return 1, err
			}
			if o.approval == nil || !config.MountConnection.viaShell() || config.MountConnection.Session != o.approval.Session || config.MountConnection.Host != o.host {
				return 1, fmt.Errorf("invalid routed mount approval")
			}
		}
		if o.approval != nil && (o.approval.PID <= 0 || validSession(o.approval.Session) != nil || !validRequestID(o.approval.ID)) {
			return 1, fmt.Errorf("invalid mount approval")
		}
		if e != nil {
			return 1, e
		}
	}
	if e = validSession(o.name); e != nil {
		return 1, e
	}
	dir, e := resourceStateDir(kind)
	if e != nil {
		return 1, e
	}
	if !validTaskUID(config.UID) {
		return 1, fmt.Errorf("invalid resource task ID")
	}
	slot := config.UID
	if kind == "mount" {
		cleanup, e := prepareFTPPaths(&o)
		if e != nil {
			return 1, e
		}
		defer cleanup()
	}
	ack := make(chan error, 1)
	abandoned := make(chan struct{})
	go func() {
		var v string
		e := dec.Decode(&v)
		if e == nil && v != "commit" {
			e = fmt.Errorf("invalid startup acknowledgement")
		}
		if e != nil {
			close(abandoned)
		}
		ack <- e
	}()
	direction := "<-"
	if kind == "forward" && config.Forward.Reverse {
		direction = "->"
	}
	s := &resourceService{startup: startup, log: log, environment: env, directory: directory, committed: committed, record: resourceRecord{UID: config.UID, Kind: kind, Name: o.name, Host: o.host, Version: version, ReadOnly: o.readOnly, Direction: direction}, config: config, stop: make(chan chan error), ack: ack, abandoned: abandoned, done: make(chan struct{}), recordPath: filepath.Join(dir, slot+".json")}
	defer func() { close(s.done); s.controlMu.Lock(); s.controlMu.Unlock() }()
	s.setTransport("starting", nil)
	defer func() { s.saveResult(code, resultErr) }()
	if tasks != nil {
		task := &daemonResourceTask{record: s.record, done: s.done, control: s.control}
		if err := tasks.register(config.UID, task); err != nil {
			return 1, err
		}
		s.publish = task.publish
	} else {
		// The manager retains both pipes after commit. Losing that owner asks
		// the isolated mount to stop; no endpoint or recovery record is needed.
		s.committed = func() {
			go func() {
				for {
					var action string
					if err := dec.Decode(&action); err != nil {
						s.control("cancel")
						return
					}
					s.controlMu.Lock()
					select {
					case <-s.done:
						s.controlMu.Unlock()
						return
					default:
					}
					err := json.NewEncoder(startup).Encode(s.control(action))
					s.controlMu.Unlock()
					if err != nil {
						s.control("cancel")
						return
					}
				}
			}()
		}
	}
	if config.Forward != nil {
		return runForward(*config.Forward, s)
	}
	return runFTPMount(o, s)
}
func startResourceBackground(config resourceConfig, kind string) (int, error) {
	reservation, e := chooseTaskIdentity(kind, "")
	if e != nil {
		return 1, e
	}

	return startSelectedResource(config, kind, reservation)
}
func startSelectedResource(config resourceConfig, kind string, reservation *taskIdentitySelection) (int, error) {
	name := reservation.name
	config.UID = reservation.uid
	if config.Forward != nil {
		config.Forward.Name = name
	} else {
		config.MountName = name
	}
	if e := validSession(name); e != nil {
		return 1, e
	}
	dir, e := resourceStateDir(kind)
	if e != nil {
		return 1, e
	}
	token := resourceToken()
	prompts := make(chan struct{}, 1)
	l, broker, e := resourceListen(token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && (r.URL.Path == "/setup" || r.URL.Path == "/setup-log") {
			select {
			case prompts <- struct{}{}:
				defer func() { <-prompts }()
			case <-r.Context().Done():
				return
			}
			serveResourceSetup(w, r)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/ask" && r.URL.Path != "/unlock" {
			http.NotFound(w, r)
			return
		}
		select {
		case prompts <- struct{}{}:
			defer func() { <-prompts }()
		case <-r.Context().Done():
			return
		}
		serveSSHConfirmation(w, r)
	})
	if e != nil {
		return 1, e
	}
	defer broker.Close()
	logPath := filepath.Join(dir, config.UID+".log")
	cwd, e := os.Getwd()
	if e != nil {
		return 1, e
	}
	c, e := openLocalDaemon(localDaemonRequest{Kind: kind, Record: localShellRecord{Name: name, Directory: cwd}, Env: os.Environ()})
	if e != nil {
		return 1, e
	}
	defer c.Close()
	input, output := c, c
	config.AuthAddress = l.Addr().String()
	config.AuthToken = token
	if e = json.NewEncoder(input).Encode(config); e != nil {
		return 1, e
	}
	dec := json.NewDecoder(output)
	var startup resourceStartup
	if e = dec.Decode(&startup); e != nil {
		return 1, fmt.Errorf("resource startup failed; see %s: %w", logPath, e)
	}
	if startup.Error != "" {
		return 1, fmt.Errorf("%s (log: %s)", startup.Error, logPath)
	}
	record := startup.resourceRecord
	if e = json.NewEncoder(input).Encode("commit"); e != nil {
		return 1, e
	}
	var ack string
	if e = dec.Decode(&ack); e != nil || ack != "started" {
		return 1, fmt.Errorf("resource startup was not acknowledged: %v", e)
	}
	detail := record.Detail
	if kind == "mount" {
		detail = fmt.Sprintf("%s:%s at %s using SFTP over SSH", record.Host, record.Remote, record.Mountpoint)
	}
	fmt.Fprintf(os.Stdout, "Started %s/%s: %s\nCancel: sshore cancel %s/%s\n", kind, record.Name, cleanText(detail), kind, record.Name)

	return 0, nil
}

func stopResource(kind, name string, expected ...string) error {
	r, err := findRunningResource(kind, name)
	if err != nil {
		return err
	}
	if len(expected) > 0 && resourceIdentity(r) != expected[0] {
		return fmt.Errorf("task changed; refresh with sshore list")
	}
	if _, err := daemonResourceRequest(message{Type: "cancel", Instance: r.UID}); err != nil {
		return err
	}
	if kind == "mount" {
		fmt.Fprintf(os.Stdout, "Unmounted mount/%s.\n", name)
	} else {
		fmt.Fprintf(os.Stdout, "Closed forward/%s.\n", name)
	}
	return nil
}

func (s *resourceService) peerTask() peerRecord {
	detail := strings.ReplaceAll(s.record.Detail, "your computer", "manager")
	if s.record.Kind == "mount" {
		detail = "remote directory: " + s.record.Remote
	}
	// Derive a stable mirror key without exposing the local control token.
	key := sha256.Sum256([]byte("peer:" + s.record.UID))
	return peerRecord{Direction: s.record.Direction, UID: s.record.UID, Version: version, Key: hex.EncodeToString(key[:16]), ID: s.record.Kind + "/" + s.record.Name, Manager: taskManagerLabel(), State: "active", Detail: cleanText(detail)}
}
