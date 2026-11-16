package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	statefs "github.com/jay-waves/sshore/internal/state"
)

type localDaemonRequest struct {
	Spawn  *spawnConfig
	Kind   string
	Record localShellRecord
	Env    []string
}

func localEnvironmentValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(env[i], "=")
		if ok && (key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
			return value
		}
	}
	return ""
}

func daemonPath() (string, error) {
	dir, err := statefs.Dir("daemon")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local.json"), nil
}

func daemonFileLock(name string) (*os.File, error) {
	dir, err := statefs.Dir("daemon")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = statefs.Private(path, false); err == nil {
		err = lockResourceState(f)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openLocalDaemon(request localDaemonRequest) (net.Conn, error) {
	record, err := ensureLocalDaemon()
	if err != nil {
		return nil, err
	}
	c, err := dialLocalSession(record)
	if err != nil {
		return nil, err
	}
	err = writeMessage(c, message{Type: "daemon_request", LocalRequest: &request})
	if err == nil {
		var ack message
		ack, err = readMessage(c)
		if err == nil && ack.Type != "daemon_ready" {
			err = fmt.Errorf("local daemon: %s", ack.Error)
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func ensureLocalDaemon() (localShellRecord, error) {
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
		time.Sleep(10 * time.Millisecond)
	}
	defer lock.Close()
	path, err := daemonPath()
	if err != nil {
		return empty, err
	}
	r, err := readLocalRecord(path, "daemon")
	if err == nil {
		if err = checkPeerVersion(r.Version, "local daemon"); err != nil {
			return empty, err
		}
		c, e := dialLocalSession(r)
		if e == nil {
			c.Close()
			return r, nil
		}
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	// Never replace an unresponsive live owner or signal a PID from metadata.
	owner, err := daemonFileLock("owner")
	if err != nil {
		return empty, fmt.Errorf("local daemon is unavailable; its owner is still active: %w", err)
	}
	owner.Close()
	return launchLocalDaemon()
}

func runLocalDaemon() (result error) {
	enc := json.NewEncoder(os.Stdout)
	defer func() {
		if result != nil {
			_ = enc.Encode(localStartup{Error: result.Error()})
		}
	}()
	dec := json.NewDecoder(io.LimitReader(os.Stdin, 16*1024))
	var record localShellRecord
	if err := dec.Decode(&record); err != nil {
		return err
	}
	if record.Name != "daemon" || record.Version != version || len(record.Token) != 64 {
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
	if err = statefs.WriteJSON(path, record); err != nil {
		return err
	}
	defer os.Remove(path)
	if err = enc.Encode(localStartup{Record: record}); err != nil {
		return err
	}
	var ack string
	if err = dec.Decode(&ack); err != nil || ack != "ready" {
		return fmt.Errorf("daemon startup not committed")
	}
	os.Stdin.Close()
	os.Stdout.Close()
	// The mutex protects bookkeeping only, never terminal I/O or worker startup.
	var mu sync.Mutex
	active := 0
	last := time.Now()
	stopping := false
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				mu.Lock()
				idle := active == 0 && time.Since(last) > 30*time.Second
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
	handshakes := make(chan struct{}, 64)
	for {
		c, err := listener.Accept()
		if err != nil {
			mu.Lock()
			idle := stopping
			mu.Unlock()
			if idle {
				return nil
			}
			return err
		}
		select {
		case handshakes <- struct{}{}:
		default:
			c.Close()
			continue
		}
		mu.Lock()
		if stopping {
			mu.Unlock()
			c.Close()
			<-handshakes
			continue
		}
		active++
		mu.Unlock()
		go func() {
			pending := true
			defer func() {
				if pending {
					<-handshakes
				}
			}()
			defer c.Close()
			defer func() { mu.Lock(); active--; last = time.Now(); mu.Unlock() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			m, err := readMessage(c)
			if err != nil || m.Type != "local_auth" || subtle.ConstantTimeCompare([]byte(m.Token), []byte(record.Token)) != 1 {
				return
			}
			if m.SoftwareVersion != version {
				_ = writeMessage(c, message{Type: "fatal", SoftwareVersion: version, Error: "local daemon version mismatch"})
				return
			}
			if writeMessage(c, message{Type: "local_ready", SoftwareVersion: version}) != nil {
				return
			}
			m, err = readMessage(c)
			if err != nil {
				return
			}
			if m.Type != "daemon_request" || m.LocalRequest == nil {
				return
			}
			req := *m.LocalRequest
			if req.Kind != "spawn" && req.Kind != "shell" && req.Kind != "mount" && req.Kind != "forward" {
				return
			}
			if err = validSession(req.Record.Name); err != nil {
				return
			}
			if !filepath.IsAbs(req.Record.Directory) {
				return
			}
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
			env = append(env, "SSHORE_STATE_DIR="+filepath.Dir(filepath.Dir(path)))
			req.Env = env
			req.Record.ManagerPID = os.Getpid()
			if writeMessage(c, message{Type: "daemon_ready"}) != nil {
				return
			}
			_ = c.SetDeadline(time.Time{})
			<-handshakes
			pending = false
			if req.Kind == "spawn" {
				_ = runSpawnRemote(c, req)
				return
			}
			if req.Kind == "shell" {
				err = runLocalShell(req.Record, req.Env, func(r localShellRecord) error {
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
				_ = runDaemonResource(c, req)
			}
		}()
	}
}

func launchLocalDaemon() (localShellRecord, error) {
	var empty localShellRecord
	exe, err := os.Executable()
	if err != nil {
		return empty, err
	}
	cmd := exec.Command(exe, "local-daemon")
	detachLocalDaemon(cmd)
	dir, err := statefs.Dir("daemon")
	if err != nil {
		return empty, err
	}
	logPath := filepath.Join(dir, "local.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return empty, err
	}
	defer log.Close()
	if err = statefs.Private(logPath, false); err != nil {
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
	request := localShellRecord{Name: "daemon", Version: version, Token: resourceToken()}
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
