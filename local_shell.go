package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The token is a local connection capability, never part of public inventory.
type localShellRecord struct {
	UID                                      string `json:",omitempty"`
	ManagerPID                               int
	Name, Address, Token, Version, Directory string
}

func openNativeShell(ctx context.Context, ref, identity string) (net.Conn, localShellRecord, error) {
	record, err := existingDaemonRecord()
	if err != nil {
		return nil, localShellRecord{}, err
	}
	path := "/tasks/native/open?" + url.Values{"ref": {ref}, "identity": {identity}}.Encode()
	conn, err := dialDaemonStream(ctx, record, time.Now().Add(5*time.Second), path, nil)
	if err != nil {
		return nil, localShellRecord{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var reply localStartup
	err = readControlJSON(conn, &reply)
	if err == nil {
		switch {
		case reply.Error != "":
			err = errors.New(reply.Error)
		case !validTaskUID(reply.Record.UID):
			err = os.ErrNotExist
		}
	}
	if err != nil {
		conn.Close()
		return nil, localShellRecord{}, err
	}
	return conn, reply.Record, nil
}

func readLocalRecord(path, name string) (localShellRecord, error) {
	var r localShellRecord
	if err := privateStatePath(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	err = json.UnmarshalRead(io.LimitReader(f, 16*1024), &r)
	if err != nil {
		return r, err
	}
	host, port, err := net.SplitHostPort(r.Address)
	n, portErr := strconv.Atoi(port)
	token, tokenErr := hex.DecodeString(r.Token)
	if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || tokenErr != nil || len(token) != 32 || (r.Name != name && r.UID != name) || r.UID != "" && !validTaskUID(r.UID) || r.Version == "" {
		return r, fmt.Errorf("invalid local shell record: %s", name)
	}
	return r, nil
}

// Authenticate before sending any shell commands. Each exchange is bounded;
// callers clear the deadline only when they take ownership of the connection.
func dialLocalSessionContext(ctx context.Context, r localShellRecord, deadline time.Time) (net.Conn, error) {
	return dialDaemonStream(ctx, r, deadline, "/tasks/"+r.UID+"/open", nil)
}
func dialDaemonStream(ctx context.Context, r localShellRecord, deadline time.Time, path string, input any) (net.Conn, error) {
	if err := checkPeerVersion(r.Version, "local shell"); err != nil {
		return nil, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, os.ErrDeadlineExceeded
	}
	c, err := (&net.Dialer{Timeout: min(time.Second, remaining)}).DialContext(ctx, "tcp", r.Address)
	if err != nil {
		return nil, fmt.Errorf("local shell %s unavailable: %w", r.Name, err)
	}
	_ = c.SetDeadline(deadline)
	return upgradeDaemonConn(ctx, c, r, path, input)
}

func (r localShellRecord) identity() string {
	if validTaskUID(r.UID) {
		return "task:" + r.UID
	}
	return fmt.Sprintf("local:%x", sha256.Sum256([]byte(r.Token)))
}

// Register the terminal task before publishing readiness. Attachments use the
// account listener and never own the task lifetime.
func serveLocalShell(tasks *daemonTasks, s *localShell, r localShellRecord, ready func(localShellRecord) error, serve func(net.Conn, message) error) error {
	if r.Version != version || len(r.Token) != 64 || !filepath.IsAbs(r.Directory) {
		return fmt.Errorf("invalid local startup request")
	}
	parent := s.parent
	if err := tasks.register("", &nativeShellTask{shell: s, serve: serve}); err != nil {
		return err
	}
	r.UID, r.Name = s.record.UID, s.taskName()
	s.watchParent(parent)
	if err := ready(r); err != nil {
		return err
	}
	startupTimer := time.AfterFunc(15*time.Second, func() {
		s.mu.Lock()
		empty := s.terminal == nil
		if empty {
			s.endTerminal()
		}
		s.mu.Unlock()
		if empty {
			s.close()
		}
	})
	defer startupTimer.Stop()
	<-s.done
	return nil
}

type localShell struct {
	shellTask
	handoff *localHandoff
}

func (s *localShell) view(includeDirectory bool) localShellView {
	s.mu.Lock()
	view := localShellView{
		Owner: s.parentIdentityLocked(), Pinned: s.pinned, PinUntil: s.pinUntil, Identity: s.record.identity(),
		Name: s.record.Name, Version: s.record.Version, PID: s.pid, Shell: s.shellName,
	}
	reportedDirectory := s.reportedDirectory
	view.State = s.state()
	if s.active != nil {
		view.SourceHost = s.active.sourceHost
	}
	s.mu.Unlock()
	if includeDirectory {
		view.Directory = currentShellDirectory(view.PID, reportedDirectory)
	}
	return view
}

func retentionLocalShell(identity string, m message) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	c, _, err := openNativeShell(ctx, "", identity)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	return requestShellRetention(c, m)
}

func runLocalShell(tasks *daemonTasks, record localShellRecord, env []string, parent *shellTask, ready func(localShellRecord) error) error {
	s := &localShell{shellTask: tasks.newShellTask(record, env, parent, false)}
	defer s.close()
	return serveLocalShell(tasks, s, record, ready, func(c net.Conn, m message) error {
		switch m.Type {
		case "handoff_confirm":
			return s.confirmHandoff(c, m)
		case "handoff":
			return s.requestHandoff(c, m.Handoff)

		default:
			handled, err := s.serveCommand(c, m, s.attachmentCommand, s.status)
			if handled {
				return err
			}
			return fmt.Errorf("unexpected local request")
		}
	})
}

func (s *localShell) status(_ string) message {
	view := s.view(false)
	return message{Type: "session", Pinned: view.Pinned, PinUntil: view.PinUntil, Name: view.Name, SoftwareVersion: view.Version,
		Attached: view.State == taskActive, PID: view.PID}
}

func (s *localShell) attachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "handoff_result":
		s.finishHandoff(p, m)
	case "sh_child":
		s.mu.Lock()
		h := &clientHandoff{Shell: (options{command: "sh", local: true, host: "local", pin: m.Pinned}).shellConfig(), Directory: s.directoryOrStartupLocked(), Environment: append([]string(nil), s.environment...)}
		s.mu.Unlock()
		go serveChildShortcut(p, s.done, func(c net.Conn) { _ = s.requestHandoff(c, h) })
	default:
		return fmt.Errorf("unexpected local control request")
	}
	return nil
}
