package main

// Local resource workers own one SSH transport and one mount or forward.
// Private records contain
// a loopback control capability, never SSH credentials. An OS lock serializes
// each name for the worker lifetime; control never signals a recorded PID.
// Startup retains a terminal askpass broker until the worker acknowledges its
// resource. EOF before the commit tears it down instead of orphaning it.
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

	statusMu   sync.Mutex
	status     resourceStatus
	record     resourceRecord
	config     resourceConfig
	stop       chan chan error
	ack        <-chan error
	abandoned  <-chan struct{}
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
func resourceHTTP(address, token, method, route string, body io.Reader) (*http.Response, error) {
	return resourceHTTPContext(context.Background(), address, token, method, route, body)
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
	if e := writeStateJSON(s.recordPath, s.record); e != nil {
		return e
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
func runResourceWorker() (code int, resultErr error) {
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
	return runConfiguredResource(config, dec, os.Stdout, os.Stderr, os.Environ(), "", nil)
}

func runConfiguredResource(config resourceConfig, dec *json.Decoder, startup, log io.Writer, env []string, directory string, committed func()) (code int, resultErr error) {
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
			if o.approval == nil || config.MountConnection.Parent == nil || config.MountConnection.Session != o.approval.Session || config.MountConnection.Host != o.host {
				return 1, fmt.Errorf("invalid routed mount approval")
			}
		}
		if o.approval != nil && (o.approval.PID <= 0 || validSession(o.approval.Session) != nil || !validApprovalID(o.approval.ID)) {
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
	nameLock, e := lockObjectName(o.name)
	if e != nil {
		return 1, e
	}
	defer nameLock.Close()
	slot := o.name
	if validTaskUID(config.UID) {
		slot = config.UID
	}
	lockPath := filepath.Join(dir, slot+".lock")
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 1, e
	}
	defer lock.Close()
	if e = privateStatePath(lockPath, false); e != nil {
		return 1, e
	}
	if e = lockResourceState(lock); e != nil {
		return 1, fmt.Errorf("resource %s already active (see sshore list): %w", o.name, e)
	}
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
	s := &resourceService{startup: startup, log: log, environment: env, directory: directory, committed: committed, record: resourceRecord{UID: config.UID, Kind: kind, Name: o.name, Host: o.host, Version: version, Token: resourceToken(), ReadOnly: o.readOnly, Direction: direction}, config: config, stop: make(chan chan error), ack: ack, abandoned: abandoned, recordPath: filepath.Join(dir, slot+".json")}
	// The lock excludes a live owner. Remove only its stale metadata, never a files.
	if e = os.Remove(s.recordPath); e != nil && !os.IsNotExist(e) {
		return 1, e
	}
	displayPath, err := resourceNamePath(kind, slot)
	if err != nil {
		return 1, err
	}
	defer os.Remove(displayPath)
	s.setTransport("starting", nil)
	defer func() { s.saveResult(code, resultErr) }()
	defer os.Remove(s.recordPath)
	l, server, e := resourceListen(s.record.Token, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.statusSnapshot())
		case r.Method == "POST" && r.URL.Path == "/stop":
			request := make(chan error, 1)
			select {
			case s.stop <- request:
			case <-r.Context().Done():
				return
			}
			select {
			case e := <-request:
				if e != nil {
					http.Error(w, cleanText(e.Error()), 409)
				} else {
					fmt.Fprint(w, "stopped")
				}
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	})
	if e != nil {
		return 1, e
	}
	defer func() { ctx, cancel := resourceShutdownContext(); defer cancel(); server.Shutdown(ctx) }()
	s.record.Address = l.Addr().String()
	if config.Forward != nil {
		return runForward(*config.Forward, s)
	}
	return runFTPMount(o, s)
}
func startResourceBackground(config resourceConfig, kind string) (int, error) {
	reservation, e := reserveResourceName(kind, "")
	if e != nil {
		return 1, e
	}
	defer reservation.Close()
	return startReservedResource(config, kind, reservation)
}
func startReservedResource(config resourceConfig, kind string, reservation *resourceNameReservation) (int, error) {
	name := reservation.name
	config.UID = reservation.uid
	if config.Forward != nil {
		config.Forward.Name = name
	} else {
		config.MountName = name
	}
	// Keep the startup reservation until the worker owns the lifetime lock.
	reservation.owner.Close()
	reservation.owner = nil
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
	logPath := filepath.Join(dir, name+".log")
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

func readResourceRecord(kind, name string) (resourceRecord, error) {
	var r resourceRecord
	if e := validSession(name); e != nil {
		return r, e
	}
	d, e := resourceStateDir(kind)
	if e != nil {
		return r, e
	}
	p := filepath.Join(d, name+".json")
	if e = privateStatePath(p, false); e != nil {
		return r, e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	if r.Kind == "" {
		r.Kind = "mount"
	}
	if e == nil && r.UID != "" && !validTaskUID(r.UID) {
		e = fmt.Errorf("invalid resource identity")
	}
	if e == nil && r.Kind != kind {
		e = fmt.Errorf("resource type mismatch")
	}
	if e == nil && r.Name != name && r.UID != name {
		e = fmt.Errorf("invalid mount record")
	}
	return r, e
}
func stopResource(kind, name string, expected ...string) error {
	r, e := findRunningResource(kind, name)
	if e != nil {
		return e
	}
	if len(expected) > 0 && resourceIdentity(r) != expected[0] {
		return fmt.Errorf("task changed; refresh with sshore list")
	}
	dir, e := resourceStateDir(kind)
	if e != nil {
		return e
	}
	lockPath := filepath.Join(dir, r.storageName()+".lock")
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = privateStatePath(lockPath, false); e != nil {
		return e
	}
	// The worker holds this lease throughout its lifetime. A failed HTTP probe
	// alone cannot distinguish an exited worker from an unresponsive one.
	if e = lockResourceState(lock); e == nil {
		current, err := readResourceRecord(kind, r.storageName())
		if err != nil {
			return err
		}
		if resourceIdentity(current) != resourceIdentity(r) {
			return fmt.Errorf("task changed; refresh with sshore list")
		}
		for _, suffix := range []string{".json", ".name", ".json.result"} {
			if err := os.Remove(filepath.Join(dir, r.storageName()+suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		fmt.Fprintf(os.Stdout, "Removed exited %s/%s.\n", kind, name)
		return nil
	}
	if r.Version != version {
		return fmt.Errorf("resource uses sshore %s; stop it with that exact version", cleanText(r.Version))
	}
	resp, e := resourceHTTP(r.Address, r.Token, "POST", "/stop", nil)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return fmt.Errorf("stop: %s", cleanText(string(b)))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if e = lockResourceState(lock); e == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("resource stopped, but worker cleanup is still pending")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if kind == "mount" {
		fmt.Fprintf(os.Stdout, "Unmounted mount/%s.\n", name)
	} else {
		fmt.Fprintf(os.Stdout, "Closed forward/%s.\n", name)
	}
	return nil
}

func resourceShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

func (s *resourceService) peerTask() peerRecord {
	detail := strings.ReplaceAll(s.record.Detail, "your computer", "manager")
	if s.record.Kind == "mount" {
		detail = "remote directory: " + s.record.Remote
	}
	// Derive a stable mirror key without exposing the local control token.
	key := sha256.Sum256([]byte("peer:" + s.record.Token))
	return peerRecord{Direction: s.record.Direction, UID: s.record.UID, Version: version, Key: hex.EncodeToString(key[:16]), ID: s.record.Kind + "/" + displayedResource(s.record).Name, Manager: taskManagerLabel(), State: "active", Detail: cleanText(detail)}
}
