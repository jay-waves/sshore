package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

type localDaemonRequest struct {
	Spawn  *spawnConfig
	Kind   string
	Record localShellRecord
	Env    []string
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
	owner, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_RDWR, 0600)
	if err != nil {
		return localShellRecord{}, err
	}
	defer owner.Close()
	if err := privateStatePath(owner.Name(), false); err != nil {
		return localShellRecord{}, err
	}
	if err := lockResourceState(owner); err == nil {
		return localShellRecord{}, os.ErrNotExist
	} else if !resourceLockBusy(err) {
		return localShellRecord{}, err
	}
	record, err := readLocalRecord(filepath.Join(dir, "daemon.json"), "daemon")
	if os.IsNotExist(err) {
		return record, fmt.Errorf("account daemon owns its lock but its endpoint is unavailable")
	}
	return record, err
}

func daemonFileLock(name string) (*os.File, error) {
	dir, err := daemonDirectory()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = privateStatePath(path, false); err == nil {
		err = lockResourceState(f)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openLocalDaemon(request localDaemonRequest) (net.Conn, error) {
	return openLocalDaemonContext(context.Background(), request)
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
	var lock *os.File
	var err error
	for {
		lock, err = daemonFileLock("startup")
		if err == nil {
			break
		}
		if !resourceLockBusy(err) || time.Now().After(deadline) {
			return empty, err
		}
		select {
		case <-ctx.Done():
			return empty, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer lock.Close()
	path, err := daemonPath()
	if err != nil {
		return empty, err
	}
	// The owner lock, not a persisted record, establishes whether a daemon exists.
	owner, err := daemonFileLock("owner")
	if err == nil {
		owner.Close()
		return launchLocalDaemonContext(ctx, executable...)
	}
	if !resourceLockBusy(err) {
		return empty, err
	}
	r, err := readLocalRecord(path, "daemon")
	if err != nil {
		return empty, err
	}
	if err = checkPeerVersion(r.Version, "local daemon"); err != nil {
		return empty, err
	}
	var ready bool
	if err := daemonJSON(ctx, r, "/ready", nil, &ready); err != nil {
		return empty, fmt.Errorf("local daemon is unavailable; its owner is still active: %w", err)
	}
	return r, nil
}

func newDaemonRecord() (localShellRecord, error) {
	root, err := statePath("")
	if err != nil {
		return localShellRecord{}, err
	}
	return localShellRecord{Name: "daemon", Version: version, Token: resourceToken(), Directory: root}, nil
}

func runDaemon() (result error) {
	stopHangups := protectDaemonHangups()
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
		fmt.Fprintln(os.Stderr, "Daemon running.")
	}
	tasks := &daemonTasks{}
	activity := &daemonActivity{tasks: tasks, root: record.Directory}
	accountActivity.Store(activity)
	defer accountActivity.Store(nil)
	services := newDaemonServices(tasks)
	if services != nil {
		defer services.Close()
	}
	return serveDaemon(listener, record, services, tasks)
}

// Platform services share the same authenticated listener and lifetime owner.
type daemonServices interface {
	Routes(*http.ServeMux, *daemonTasks)
	Close()
}

func serveDaemon(listener net.Listener, record localShellRecord, services daemonServices, tasks *daemonTasks) error {
	// The mutex protects bookkeeping only, never terminal I/O or worker startup.
	var mu sync.Mutex
	active := 0
	last := time.Now()
	stopping := false
	busy := func() bool {
		if activity := accountActivity.Load(); activity != nil && activity.busy() {
			return true
		}
		return tasks.Busy()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
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
				mu.Lock()
				idle := active == 0 && !busy() && time.Since(last) > 30*time.Second
				if idle {
					stopping = true
				}
				mu.Unlock()
				if idle {
					listener.Close()
					return
				}
			}
		}
	}()
	mux := daemonRoutes(record, tasks, services, func() { mu.Lock(); stopping = true; mu.Unlock(); listener.Close() })
	// Limit connections that are still reading their HTTP headers.
	pending := make(map[net.Conn]struct{})
	server := http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		MaxHeaderBytes:    8192,
		ConnState: func(c net.Conn, state http.ConnState) {
			mu.Lock()
			defer mu.Unlock()
			switch state {
			case http.StateNew:
				if stopping || len(pending) >= 64 {
					_ = c.Close()
					return
				}
				pending[c] = struct{}{}
			case http.StateActive, http.StateClosed:
				delete(pending, c)
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !daemonAuthorized(r, record) {
				http.Error(w, "unauthorized", 401)
				return
			}
			if !strings.HasPrefix(r.URL.Path, "/admin/") && r.Header.Get("Sshore-Version") != record.Version {
				http.Error(w, "daemon version mismatch", 409)
				return
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
	err := server.Serve(listener)
	mu.Lock()
	closedForShutdown := stopping
	mu.Unlock()
	if !closedForShutdown {
		return err
	}
	// Let the shutdown response finish before releasing the daemon owner.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
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
		_ = runRemoteShell(c, req)
		return
	}
	if req.Kind == "shell" {
		req.Record.Address, req.Record.Token = record.Address, record.Token
		err := runLocalShell(tasks, req.Record, req.Env, func(r localShellRecord) error {
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

// Mount daemons receive only startup/control messages through the account daemon.
// Forwards run here and directly own their SSH child; payloads stay in SSH.
func runDaemonResource(c net.Conn, request localDaemonRequest, tasks *daemonTasks) (result error) {
	enc := json.NewEncoder(c)
	defer func() {
		if result != nil {
			_ = enc.Encode(resourceStartup{Error: result.Error()})
		}
	}()
	dec := json.NewDecoder(io.LimitReader(c, 1<<20))
	var config resourceConfig
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := dec.Decode(&config); err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Time{})
	if request.Kind == "forward" {
		if config.Forward == nil || config.Forward.Name != request.Record.Name {
			return fmt.Errorf("invalid forward startup")
		}
	} else {
		_, err := parseFTPOptions(config.Args)
		if err != nil {
			return err
		}
		if config.Forward != nil || config.MountName != request.Record.Name {
			return fmt.Errorf("invalid mount startup")
		}
	}
	dir, err := resourceStateDir(request.Kind)
	if err != nil {
		return err
	}
	if !validTaskUID(config.UID) {
		return fmt.Errorf("invalid resource task ID")
	}
	logPath := filepath.Join(dir, config.UID+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	if err = privateStatePath(logPath, false); err != nil {
		return err
	}
	if config.Forward != nil {
		_, err := runConfiguredResource(config, dec, c, log, request.Env, request.Record.Directory, func() { c.Close() }, tasks)
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", "mount")
	detachDaemon(cmd)
	cmd.Dir = request.Record.Directory
	cmd.Env = request.Env
	cmd.Stderr = log
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer output.Close()
	if err = cmd.Start(); err != nil {
		return err
	}
	finished := make(chan struct{})
	defer close(finished)
	committed := false
	defer func() {
		if committed {
			return
		}
		input.Close()
		output.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	if err = json.NewEncoder(input).Encode(config); err != nil {
		return err
	}
	child := json.NewDecoder(output)
	var startup resourceStartup
	if err = child.Decode(&startup); err != nil {
		return err
	}
	var pipeMu sync.Mutex
	pipeMu.Lock()
	pipeLocked := true
	defer func() {
		if pipeLocked {
			pipeMu.Unlock()
		}
	}()
	if startup.Error == "" {
		task := &daemonResourceTask{record: startup.resourceRecord, done: finished, control: func(action string) resourceReply {
			pipeMu.Lock()
			defer pipeMu.Unlock()
			if err := json.NewEncoder(input).Encode(action); err != nil {
				return resourceReply{Error: err.Error()}
			}
			var reply resourceReply
			if err := child.Decode(&reply); err != nil {
				return resourceReply{Error: err.Error()}
			}
			return reply
		}}
		if err := tasks.register(config.UID, task); err != nil {
			return err
		}
	}
	if err = enc.Encode(startup); err != nil {
		return err
	}
	if startup.Error != "" {
		return nil
	}
	var ack string
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = dec.Decode(&ack); err != nil {
		return err
	}
	if ack != "commit" {
		return fmt.Errorf("invalid resource commit")
	}
	if err = json.NewEncoder(input).Encode(ack); err != nil {
		return err
	}
	if err = child.Decode(&ack); err != nil {
		return err
	}
	if ack != "started" {
		return fmt.Errorf("resource not started")
	}
	// From this point the worker owns the task even if the acknowledgement is lost.
	committed = true
	pipeMu.Unlock()
	pipeLocked = false
	_ = enc.Encode(ack)
	c.Close()
	_ = cmd.Wait()
	return nil
}

func launchLocalDaemonContext(ctx context.Context, executable ...string) (localShellRecord, error) {
	if err := ctx.Err(); err != nil {
		return localShellRecord{}, err
	}
	var empty localShellRecord
	exe, err := os.Executable()
	if err != nil {
		return empty, err
	}
	if len(executable) > 0 && executable[0] != "" {
		exe = executable[0]
	}
	cmd := exec.Command(exe, "daemon")
	detachDaemon(cmd)
	dir, err := daemonDirectory()
	if err != nil {
		return empty, err
	}
	logPath := filepath.Join(dir, "local.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return empty, err
	}
	defer log.Close()
	if err = privateStatePath(logPath, false); err != nil {
		return empty, err
	}
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
		if !committed {
			_ = cmd.Process.Kill()
		}
		go cmd.Wait()
	}()
	request, err := newDaemonRecord()
	if err != nil {
		return empty, err
	}
	if err = json.NewEncoder(input).Encode(request); err != nil {
		return empty, err
	}
	result := make(chan localStartup, 1)
	go func() {
		var reply localStartup
		if e := json.NewDecoder(output).Decode(&reply); e != nil {
			reply.Error = e.Error()
		}
		result <- reply
	}()
	select {
	case <-ctx.Done():
		return empty, ctx.Err()
	case reply := <-result:
		if reply.Error != "" {
			return empty, fmt.Errorf("start local daemon: %s", reply.Error)
		}
		path, err := daemonPath()
		if err != nil {
			return empty, err
		}
		r, err := readLocalRecord(path, "daemon")
		if err != nil {
			return empty, err
		}
		if r != reply.Record || r.Token != request.Token || r.Version != version {
			return empty, fmt.Errorf("local daemon changed during startup")
		}
		if err = json.NewEncoder(input).Encode("ready"); err != nil {
			return empty, err
		}
		committed = true
		return r, nil
	case <-time.After(10 * time.Second):
		return empty, fmt.Errorf("local daemon startup timed out")
	}
}
