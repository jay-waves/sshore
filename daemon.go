package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/release"
	"github.com/jay-waves/sshore/internal/sys"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

func daemonDirectory() (string, error) {
	dir, err := daemonLocation()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := privateStatePath(dir, true); err != nil {
		return "", err
	}
	return dir, nil
}

func daemonStateRoot(root string, explicit bool) (string, error) {
	dir, err := daemonLocation()
	if err != nil {
		return "", err
	}
	return daemonStateRootAt(dir, root, explicit)
}

// Open the existing account lock without creating state or taking ownership.
func openDaemonOwner(dir string) (*sys.FileLock, error) {
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := privateStatePath(owner.Name(), false); err != nil {
		owner.Close()
		return nil, fmt.Errorf("validate account owner lock: %w", err)
	}
	return sys.NewFileLock(owner), nil
}

func daemonStateRootAt(dir, root string, explicit bool) (string, error) {
	record, err := ownedDaemonRecord(dir)
	if os.IsNotExist(err) {
		return root, nil
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(record.Directory) {
		return "", fmt.Errorf("invalid account daemon state root")
	}
	if !explicit {
		return record.Directory, nil
	}
	same := filepath.Clean(root) == filepath.Clean(record.Directory)
	if !same {
		a, aErr := os.Stat(root)
		b, bErr := os.Stat(record.Directory)
		same = aErr == nil && bErr == nil && os.SameFile(a, b)
	}
	if !same {
		return "", fmt.Errorf("account daemon uses %s; SSHORE_STATE_DIR cannot select another root while it is running", record.Directory)
	}
	return record.Directory, nil
}

// Daemon ownership follows the authenticated OS account, independently of runtime state.
var daemonLocation = func() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(account.HomeDir, ".sshore", "daemon"), nil
}

type localDaemonRequest struct {
	Shell    *shellConfig
	Resource *resourceConfig  `json:",omitempty"`
	Auth     *sshAuthEndpoint `json:",omitempty"`
	Parent   string
	Kind     string
	Record   localShellRecord
	Env      []string
}

func localEnvironmentValue(env []string, name string) string {
	for _, e := range slices.Backward(env) {
		key, value, ok := strings.Cut(e, "=")
		if ok && (key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
			return value
		}
	}
	return ""
}

func daemonPath() (string, error) {
	dir, err := daemonDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.json"), nil
}

// Reads never start a manager or create locator/lock files. The owner lock,
// rather than a leftover locator, establishes whether pending state exists.
func existingDaemonRecord() (localShellRecord, error) {
	dir, err := daemonLocation()
	if err != nil {
		return localShellRecord{}, err
	}
	return ownedDaemonRecord(dir)
}

func ownedDaemonRecord(dir string) (localShellRecord, error) {
	owner, err := openDaemonOwner(dir)
	if err != nil {
		return localShellRecord{}, err
	}
	defer owner.Close()
	path := filepath.Join(dir, "daemon.json")
	for until := time.Now().Add(time.Second); ; {
		if err := owner.TryLock(); err == nil {
			return localShellRecord{}, os.ErrNotExist
		} else if !errors.Is(err, sys.ErrFileLockBusy) {
			return localShellRecord{}, err
		}
		record, err := readLocalRecord(path, "daemon")
		if err == nil {
			return record, nil
		}
		if !os.IsNotExist(err) {
			return record, fmt.Errorf("read account daemon endpoint %s: %w", path, err)
		}
		// Startup publishes the endpoint after taking ownership. Exit removes it
		// before releasing ownership. Recheck both under the same account scope.
		if time.Now().After(until) {
			return record, errors.New("account daemon owns its lock but its endpoint is unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func daemonFileLock(name string) (*sys.FileLock, error) {
	dir, err := daemonDirectory()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+".lock")
	return tryPrivateFileLock(path)
}

func openLocalDaemonContext(ctx context.Context, request localDaemonRequest) (net.Conn, error) {
	record, err := ensureLocalDaemonContext(ctx)
	if err != nil {
		return nil, err
	}
	c, err := dialDaemonStream(ctx, record, time.Now().Add(5*time.Second), "/launch", request)
	if err == nil {
		_ = c.SetDeadline(time.Time{})
	}
	return c, err
}

func ensureLocalDaemonContext(ctx context.Context, executable ...string) (localShellRecord, error) {
	if err := ctx.Err(); err != nil {
		return localShellRecord{}, err
	}
	var empty localShellRecord
	deadline := time.Now().Add(10 * time.Second)
	var lock *sys.FileLock
	var err error
	for {
		lock, err = daemonFileLock("startup")
		if err == nil {
			break
		}
		if !errors.Is(err, sys.ErrFileLockBusy) || time.Now().After(deadline) {
			return empty, err
		}
		select {
		case <-ctx.Done():
			return empty, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer lock.Close()
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return launchLocalDaemonContext(ctx, executable...)
	}
	if err != nil {
		return empty, err
	}
	return record, checkPeerVersion(record.Version, "local daemon")
}

func newDaemonRecord() (localShellRecord, error) {
	root, err := statePath("")
	if err != nil {
		return localShellRecord{}, err
	}
	return localShellRecord{Name: "daemon", Version: version, Token: resourceToken(), Directory: root}, nil
}

func runDaemon() (result error) {
	stopHangups := sys.ProtectDaemonHangups()
	defer stopHangups()
	standalone := term.IsTerminal(int(os.Stdin.Fd()))
	enc := json.NewEncoder(os.Stdout)
	defer func() {
		if result != nil && !standalone {
			_ = enc.Encode(localStartup{Error: result.Error()})
		}
	}()
	dec := json.NewDecoder(io.LimitReader(os.Stdin, 16*1024))
	var record localShellRecord
	if !standalone {
		if err := dec.Decode(&record); err != nil {
			if err != io.EOF {
				return err
			}
			standalone = true
		}
	}
	if standalone {
		var err error
		record, err = newDaemonRecord()
		if err != nil {
			return err
		}
	}
	if record.Name != "daemon" || record.Version != version || len(record.Token) != 64 || !filepath.IsAbs(record.Directory) {
		return fmt.Errorf("invalid daemon startup")
	}
	group, cgroupErr := daemonTaskCgroup()
	owner, err := daemonFileLock("owner")
	if err != nil {
		return err
	}
	defer owner.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	record.Address = listener.Addr().String()
	record.ManagerPID = os.Getpid()
	path, err := daemonPath()
	if err != nil {
		return err
	}
	log := os.Stderr // Background launches inherit the account log as stderr.
	if standalone {
		log, err = openDaemonLog(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer log.Close()
	}
	if err = writeStateJSON(path, record); err != nil {
		return err
	}
	defer os.Remove(path)
	if !standalone {
		if err = enc.Encode(localStartup{Record: record}); err != nil {
			return err
		}
		var ack string
		if err = dec.Decode(&ack); err != nil || ack != "ready" {
			return fmt.Errorf("daemon startup not committed")
		}
		os.Stdin.Close()
		os.Stdout.Close()
	} else {
		termui.Log(os.Stderr, "daemon", termui.Info, "Daemon running.")
	}
	termui.Log(log, "daemon", termui.Info, "Daemon running (PID %d).", record.ManagerPID)
	defer func() {
		if result != nil {
			termui.Log(log, "daemon", termui.Error, "Daemon stopped: %s", cleanText(result.Error()))
		} else {
			termui.Log(log, "daemon", termui.Info, "Daemon stopped.")
		}
	}()
	tasks := &daemonTasks{log: log, cgroup: group, cgroupErr: cgroupErr}
	activity := &daemonActivity{tasks: tasks, root: record.Directory}
	accountActivity.Store(activity)
	defer accountActivity.Store(nil)
	return serveDaemon(listener, record, tasks)
}

func serveDaemon(listener net.Listener, record localShellRecord, tasks *daemonTasks) error {
	// The mutex protects bookkeeping only, never terminal I/O or worker startup.
	var mu sync.Mutex
	active := 0
	last := time.Now()
	stopping := false
	// Only sockets still reading their initial headers are pending.
	pending := make(map[net.Conn]struct{})
	busy := func() bool {
		if activity := accountActivity.Load(); activity != nil && activity.busy() {
			return true
		}
		return tasks.Busy()
	}
	stop := func(onlyIfIdle bool) bool {
		mu.Lock()
		if stopping || (onlyIfIdle && (active != 0 || busy() || time.Since(last) <= 30*time.Second)) {
			mu.Unlock()
			return false
		}
		stopping = true
		headers := pending
		pending = nil
		mu.Unlock()
		listener.Close()
		for c := range headers {
			c.Close()
		}
		return true
	}
	done := make(chan struct{})
	clockDone := make(chan struct{})
	defer func() { close(done); <-clockDone }()
	go func() {
		defer close(clockDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				if activity := accountActivity.Load(); activity != nil {
					activity.tick(now)
				}
				if stop(true) {
					return
				}
			}
		}
	}()
	mux := daemonRoutes(record, tasks, func() { stop(false) })
	server := http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		MaxHeaderBytes:    8192,
		ConnState: func(c net.Conn, state http.ConnState) {
			mu.Lock()
			reject := false
			switch state {
			case http.StateNew:
				reject = stopping || len(pending) >= 64
				if !reject {
					pending[c] = struct{}{}
				}
			case http.StateActive, http.StateClosed:
				delete(pending, c)
			}
			mu.Unlock()
			if reject {
				_ = c.Close()
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !daemonAuthorized(r, record) {
				http.Error(w, "unauthorized", 401)
				return
			}
			if r.URL.Path != daemonSnapshotRoute && r.URL.Path != daemonShutdownRoute {
				if err := release.Check(record.Version, r.Header.Get("Sshore-Version"), "daemon client"); err != nil {
					w.Header().Set("Sshore-Version", record.Version)
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
			}
			mu.Lock()
			if stopping {
				mu.Unlock()
				http.Error(w, "daemon stopping", 503)
				return
			}
			active++
			mu.Unlock()
			defer func() { mu.Lock(); active--; last = time.Now(); mu.Unlock() }()
			mux.ServeHTTP(w, r)
		}),
	}
	server.SetKeepAlivesEnabled(false)
	serveErr := server.Serve(listener)
	// A listener failure uses the same connection cleanup as an explicit
	// shutdown. If another path already stopped the listener, Serve's error
	// just reports that closure.
	if !stop(false) {
		serveErr = nil
	}
	// Drain admitted requests before releasing the daemon owner.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(serveErr, server.Shutdown(ctx))
}

func runDaemonLaunch(c net.Conn, req localDaemonRequest, record localShellRecord, tasks *daemonTasks) {
	// All children share this daemon's private state root, regardless of the
	// launcher's inherited environment casing or stale state-directory value.
	env := make([]string, 0, len(req.Env)+1)
	for _, item := range req.Env {
		key, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(key, "SSHORE_STATE_DIR") {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "SSHORE_STATE_DIR="+record.Directory)
	req.Env = env
	req.Record.ManagerPID = os.Getpid()
	_ = c.SetDeadline(time.Time{})

	if req.Kind == "remote-shell" {
		_ = runRemoteShell(c, req, tasks)
		return
	}
	if req.Kind == "shell" {
		req.Record.Address, req.Record.Token = record.Address, record.Token
		var parent *shellTask
		if req.Parent != "" {
			task, ok := tasks.lookup(req.Parent).(*nativeShellTask)
			if !ok {
				_ = json.NewEncoder(c).Encode(localStartup{Error: "parent shell is unavailable"})
				return
			}
			parent = &task.shell.shellTask
		}
		err := runLocalShell(tasks, req.Record, req.Env, parent, func(r localShellRecord) error {
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			if e := json.NewEncoder(c).Encode(localStartup{Record: r}); e != nil {
				return e
			}
			var ack string
			if e := json.NewDecoder(c).Decode(&ack); e != nil {
				return e
			}
			if ack != "ready" {
				return fmt.Errorf("shell startup not committed")
			}
			c.Close()
			return nil
		})
		if err != nil {
			_ = json.NewEncoder(c).Encode(localStartup{Error: err.Error()})
		}
	} else {
		_ = runDaemonResource(c, req, tasks)
	}
}

func launchLocalDaemonContext(ctx context.Context, executable ...string) (localShellRecord, error) {
	if err := ctx.Err(); err != nil {
		return localShellRecord{}, err
	}
	var empty localShellRecord
	request, err := newDaemonRecord()
	if err != nil {
		return empty, err
	}
	exe, err := os.Executable()
	if err != nil {
		return empty, err
	}
	if len(executable) > 0 && executable[0] != "" {
		exe = executable[0]
	}
	cmd := exec.Command(exe, "daemon")
	sys.DetachAccountDaemon(cmd)
	dir, err := daemonDirectory()
	if err != nil {
		return empty, err
	}
	log, err := openDaemonLog(dir)
	if err != nil {
		return empty, err
	}
	defer log.Close()
	cmd.Stderr = log
	input, err := cmd.StdinPipe()
	if err != nil {
		return empty, err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return empty, err
	}
	defer output.Close()
	if err = cmd.Start(); err != nil {
		return empty, err
	}
	committed := false
	defer func() {
		if committed {
			go cmd.Wait()
			return
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	startup, cancel := context.WithTimeoutCause(ctx, 10*time.Second, errors.New("local daemon startup timed out"))
	defer cancel()
	stopIO := context.AfterFunc(startup, func() { input.Close(); output.Close() })
	defer stopIO()
	pipeError := func(err error) error {
		if cause := context.Cause(startup); cause != nil {
			return cause
		}
		return err
	}
	enc := json.NewEncoder(input)
	if err := enc.Encode(request); err != nil {
		return empty, pipeError(err)
	}
	var reply localStartup
	if err := json.NewDecoder(output).Decode(&reply); err != nil {
		return empty, pipeError(fmt.Errorf("start local daemon: %w", err))
	}
	if reply.Error != "" {
		return empty, fmt.Errorf("start local daemon: %s", reply.Error)
	}
	r, err := readLocalRecord(filepath.Join(dir, "daemon.json"), "daemon")
	if err != nil {
		return empty, err
	}
	if r != reply.Record || r.Token != request.Token || r.Version != version {
		return empty, fmt.Errorf("local daemon changed during startup")
	}
	if cause := context.Cause(startup); cause != nil {
		return empty, cause
	}
	if err := enc.Encode("ready"); err != nil {
		return empty, pipeError(err)
	}
	committed = true
	return r, nil
}
