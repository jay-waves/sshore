package main

// Local mount workers own one transport and one OS mount. Private records contain
// a loopback control capability, never SSH credentials. An OS lock serializes
// each name for the worker lifetime; control never signals a recorded PID.
// Startup retains a terminal askpass broker until the worker acknowledges its
// mount. EOF before the commit tears down the mount instead of orphaning it.
import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/term"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ftpRecord struct{ Name, Host, Remote, Mountpoint, Address, Token, Version string }
type ftpWorkerConfig struct {
	Args                   []string
	AuthAddress, AuthToken string
}
type ftpStopRequest struct{ reply chan error }
type ftpService struct {
	record     ftpRecord
	config     ftpWorkerConfig
	stop       chan ftpStopRequest
	ack        <-chan error
	abandoned  <-chan struct{}
	recordPath string
}

func ftpToken() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func ftpStateDir() (string, error) {
	d, e := approvalDir()
	if e != nil {
		return "", e
	}
	d = filepath.Join(filepath.Dir(d), "mounts")
	if e = os.MkdirAll(d, 0700); e != nil {
		return "", e
	}
	return d, privateState(d, true)
}
func ftpHTTP(address, token, method, route string, body io.Reader) (*http.Response, error) {
	host, _, e := net.SplitHostPort(address)
	if e != nil || host != "127.0.0.1" {
		return nil, fmt.Errorf("invalid mount control address")
	}
	req, e := http.NewRequest(method, "http://"+address+route, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	timeout := 20 * time.Second
	if route == "/status" {
		timeout = time.Second
	}
	if route == "/ask" {
		timeout = 0
	}
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer c.CloseIdleConnections()
	return c.Do(req)
}
func ftpListen(token string, handler http.HandlerFunc) (net.Listener, *http.Server, error) {
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
func (s *ftpService) sshEnvironment() []string {
	exe, _ := os.Executable()
	env := os.Environ()
	for k, v := range map[string]string{"SSH_ASKPASS": exe, "SSH_ASKPASS_REQUIRE": "force", "DISPLAY": "sshore", "SSHORE_FTP_ASKPASS": "1", "SSHORE_FTP_ASKPASS_ADDR": s.config.AuthAddress, "SSHORE_FTP_ASKPASS_TOKEN": s.config.AuthToken} {
		prefix := k + "="
		filtered := env[:0]
		for _, x := range env {
			if !strings.HasPrefix(x, prefix) {
				filtered = append(filtered, x)
			}
		}
		env = append(filtered, prefix+v)
	}
	return env
}
func (s *ftpService) ready(root, mountpoint string) error {
	s.record.Remote = root
	s.record.Mountpoint = mountpoint
	data, e := json.Marshal(s.record)
	if e != nil {
		return e
	}
	if e = os.WriteFile(s.recordPath, data, 0600); e != nil {
		return e
	}
	if e = privateState(s.recordPath, false); e != nil {
		return e
	}
	if e = json.NewEncoder(os.Stdout).Encode(s.record); e != nil {
		return e
	}

	select {
	case e = <-s.ack:
	case <-time.After(10 * time.Second):
		e = fmt.Errorf("mount startup acknowledgement timed out")
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode("started")
}
func runFTPWorker() (int, error) {
	dec := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	var config ftpWorkerConfig
	if e := dec.Decode(&config); e != nil {
		return 1, e
	}
	o, e := parseFTPOptions(config.Args)
	if e != nil {
		return 1, e
	}
	dir, e := ftpStateDir()
	if e != nil {
		return 1, e
	}
	lockPath := filepath.Join(dir, o.name+".lock")
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 1, e
	}
	defer lock.Close()
	if e = privateState(lockPath, false); e != nil {
		return 1, e
	}
	if e = lockFTPState(lock); e != nil {
		return 1, fmt.Errorf("mount session %s already active (use --list or --name): %w", o.name, e)
	}
	cleanup, e := prepareFTPPaths(&o)
	if e != nil {
		return 1, e
	}
	defer cleanup()
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
	s := &ftpService{record: ftpRecord{Name: o.name, Host: o.host, Version: version, Token: ftpToken()}, config: config, stop: make(chan ftpStopRequest), ack: ack, abandoned: abandoned, recordPath: filepath.Join(dir, o.name+".json")}
	// The lock excludes a live owner. Remove only its stale metadata, never a mount.
	if e = os.Remove(s.recordPath); e != nil && !os.IsNotExist(e) {
		return 1, e
	}
	defer os.Remove(s.recordPath)
	l, server, e := ftpListen(s.record.Token, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/status":
			fmt.Fprint(w, version)
		case r.Method == "POST" && r.URL.Path == "/stop":
			request := ftpStopRequest{reply: make(chan error, 1)}
			select {
			case s.stop <- request:
			case <-r.Context().Done():
				return
			}
			select {
			case e := <-request.reply:
				if e != nil {
					http.Error(w, cleanText(e.Error()), 409)
				} else {
					fmt.Fprint(w, "unmounted")
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
	defer func() { ctx, cancel := contextWithFTPTimeout(); defer cancel(); server.Shutdown(ctx) }()
	s.record.Address = l.Addr().String()
	return runFTPMount(o, s)
}
func startFTPBackground(args []string, o ftpOptions) (int, error) {
	dir, e := ftpStateDir()
	if e != nil {
		return 1, e
	}
	token := ftpToken()
	var prompts sync.Mutex
	l, broker, e := ftpListen(token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/ask" {
			http.NotFound(w, r)
			return
		}
		prompt, e := io.ReadAll(io.LimitReader(r.Body, 8193))
		if e != nil || len(prompt) > 8192 {
			http.Error(w, "invalid prompt", 400)
			return
		}
		prompts.Lock()
		defer prompts.Unlock()
		fmt.Fprint(os.Stderr, cleanText(string(prompt))+" ")
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			http.Error(w, "SSH authentication requires a terminal", 409)
			return
		}
		answer, e := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if e != nil {
			http.Error(w, "terminal input failed", 409)
			return
		}
		w.Write(answer)
	})
	if e != nil {
		return 1, e
	}
	defer broker.Close()
	logPath := filepath.Join(dir, o.name+".log")
	log, e := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return 1, e
	}
	defer log.Close()
	if e = privateState(logPath, false); e != nil {
		return 1, e
	}
	exe, e := os.Executable()
	if e != nil {
		return 1, e
	}
	cmd := exec.Command(exe, "ftp-worker")
	detachFTPWorker(cmd)
	cmd.Stderr = log
	input, e := cmd.StdinPipe()
	if e != nil {
		return 1, e
	}
	defer input.Close()
	output, e := cmd.StdoutPipe()
	if e != nil {
		return 1, e
	}
	defer output.Close()
	if e = cmd.Start(); e != nil {
		return 1, e
	}
	committed := false
	defer func() {
		if !committed {
			input.Close()
			output.Close()
			done := make(chan struct{})
			go func() { cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(12 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
	}()
	if e = json.NewEncoder(input).Encode(ftpWorkerConfig{Args: args, AuthAddress: l.Addr().String(), AuthToken: token}); e != nil {
		return 1, e
	}
	dec := json.NewDecoder(output)
	var record ftpRecord
	if e = dec.Decode(&record); e != nil {
		return 1, fmt.Errorf("mount startup failed; see %s: %w", logPath, e)
	}
	if e = json.NewEncoder(input).Encode("commit"); e != nil {
		return 1, e
	}
	var ack string
	if e = dec.Decode(&ack); e != nil || ack != "started" {
		return 1, fmt.Errorf("mount startup was not acknowledged: %v", e)
	}
	committed = true
	cmd.Process.Release()
	fmt.Fprintf(os.Stdout, "Mounted %s:%s at %s (session %s).\nStop: sshore ftp --stop %s\n", cleanText(record.Host), cleanText(record.Remote), cleanText(record.Mountpoint), record.Name, record.Name)
	return 0, nil
}
func runFTPAskpass(args []string) (int, error) {
	r, e := ftpHTTP(os.Getenv("SSHORE_FTP_ASKPASS_ADDR"), os.Getenv("SSHORE_FTP_ASKPASS_TOKEN"), "POST", "/ask", strings.NewReader(strings.Join(args, " ")))
	if e != nil {
		return 1, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return 1, fmt.Errorf("SSH prompt failed")
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 8192))
	if e != nil {
		return 1, e
	}
	if os.Getenv("SSH_ASKPASS_PROMPT") == "confirm" && !strings.EqualFold(strings.TrimSpace(string(b)), "yes") {
		return 1, fmt.Errorf("SSH confirmation declined")
	}
	fmt.Println(string(b))
	return 0, nil
}
func readFTPRecord(name string) (ftpRecord, error) {
	var r ftpRecord
	if e := validSession(name); e != nil {
		return r, e
	}
	d, e := ftpStateDir()
	if e != nil {
		return r, e
	}
	p := filepath.Join(d, name+".json")
	if e = privateState(p, false); e != nil {
		return r, e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	if e == nil && r.Name != name {
		e = fmt.Errorf("invalid mount record")
	}
	return r, e
}
func listFTPSessions() error {
	d, e := ftpStateDir()
	if e != nil {
		return e
	}
	files, e := os.ReadDir(d)
	if e != nil {
		return e
	}
	count := 0
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		r, e := readFTPRecord(strings.TrimSuffix(f.Name(), ".json"))
		if e != nil {
			return e
		}
		status := "offline"
		resp, e := ftpHTTP(r.Address, r.Token, "GET", "/status", nil)
		if e == nil {
			if resp.StatusCode == 200 {
				status = "mounted"
			}
			resp.Body.Close()
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s:%s\t%s\n", r.Name, status, cleanText(r.Host), cleanText(r.Remote), cleanText(r.Mountpoint))
		count++
	}
	if count == 0 {
		fmt.Fprintln(os.Stdout, "No local mount sessions.")
	}
	return nil
}
func stopFTPSession(name string) error {
	r, e := readFTPRecord(name)
	if e != nil {
		return e
	}
	if r.Version != version {
		return fmt.Errorf("mount uses sshore %s; stop it with that exact version", cleanText(r.Version))
	}
	resp, e := ftpHTTP(r.Address, r.Token, "POST", "/stop", nil)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return fmt.Errorf("unmount: %s", cleanText(string(b)))
	}
	dir, e := ftpStateDir()
	if e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if e = lockFTPState(lock); e == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("unmounted, but worker cleanup is still pending")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Fprintf(os.Stdout, "Unmounted %s.\n", name)
	return nil
}

func contextWithFTPTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}
