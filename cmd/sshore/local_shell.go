package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"github.com/jay-waves/sshore/internal/shell"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jay-waves/sshore/internal/sys"
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
	if identity == "" {
		if ref == "" {
			return nil, localShellRecord{}, os.ErrNotExist
		}
		matches, err := (taskQuery{Kind: "native", Ref: ref}).published(record)
		if err != nil {
			return nil, localShellRecord{}, err
		}
		if len(matches) == 0 {
			return nil, localShellRecord{}, os.ErrNotExist
		}
		ref, identity = matches[0].Name, matches[0].Identity
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
	if err := sys.PrivateStatePath(path, false); err != nil {
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

func runLocalShell(tasks *daemonTasks, record localShellRecord, env []string, parent *tmuxTask, ready func(localShellRecord) error) error {
	if record.Version != version || len(record.Token) != 64 || !filepath.IsAbs(record.Directory) {
		return fmt.Errorf("invalid local startup request")
	}
	// Register before publishing readiness. Attachments do not own the shell.
	s, err := tasks.newShell(record, env, parent, false)
	if err != nil {
		return err
	}
	defer s.runtime.Close()
	if err := ready(s.record); err != nil {
		return err
	}
	s.runtime.ExpireUnstarted(15 * time.Second)
	<-s.runtime.Done()
	return nil
}

func (s *tmuxTask) serveLocalTask(c net.Conn, m message) error {
	switch m.Type {
	case "handoff_confirm":
		return s.confirmHandoff(c, m)
	case "handoff":
		deadline := time.Now().Add(2 * time.Minute)
		_ = c.SetDeadline(deadline)
		if err := s.requestHandoff(nil, m.Handoff, deadline); err != nil {
			return writeMessage(c, message{Type: "error", Error: err.Error()})
		}
		return writeMessage(c, message{Type: "handoff_accepted"})
	}
	if handled, err := s.serveCommand(c, m, s.localAttachmentCommand); handled {
		return err
	}
	return fmt.Errorf("unexpected local request")
}

func (s *tmuxTask) localAttachmentCommand(p *attachment, m message) error {
	switch m.Type {
	case "handoff_result":
		s.finishHandoff(p, m)
	case "sh_child":
		s.mu.Lock()
		h := &clientHandoff{Shell: (options{command: "sh", local: true, host: "local", pin: m.Pinned}).shellConfig(), Directory: s.directoryOrStartupLocked(), Environment: append([]string(nil), s.environment...)}
		s.mu.Unlock()
		if m.Directory != "" {
			h.Directory = m.Directory
		}
		go func() { p.childResult(s.requestHandoff(p, h, time.Now().Add(2*time.Minute))) }()
	default:
		return fmt.Errorf("unexpected local control request")
	}
	return nil
}

func localRequest(ref, identity string, request message) (message, error) {
	c, _, err := openNativeShell(context.Background(), ref, identity)
	if err != nil {
		return message{}, err
	}
	defer c.Close()
	switch request.Type {
	case "handoff":
		_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	}
	if err = writeMessage(c, request); err != nil {
		return message{}, err
	}
	return readMessage(c)
}

func runLocalCommand(o options) (int, error) {
	if o.hosted {
		return runHostedCommand(o)
	}
	switch o.command {
	case "sh", "attach":
		return runClient(o)
	default:
		return 2, usage("local %s is not available yet", o.command)
	}
}

type localStartup struct {
	Record localShellRecord
	Error  string
}

type hostedShellRequest struct {
	Directory, Name string
	Env             []string
	parent          *tmuxTask
}

func createHostedShell(ctx context.Context, executable, name, cwd string, env []string) (localShellRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	record, err := ensureLocalDaemonContext(ctx, executable)
	if err != nil {
		return localShellRecord{}, err
	}
	req := hostedShellRequest{Directory: cwd, Name: name, Env: env}
	var target sessionTarget
	if err := daemonJSON(ctx, record, "/shells", req, &target); err != nil {
		return localShellRecord{}, err
	}
	record.UID, record.Name, record.Directory = target.Instance, target.Name, cwd
	return record, nil
}

func startLocalSessionContext(ctx context.Context, o options) (localShellRecord, error) {
	if err := ctx.Err(); err != nil {
		return localShellRecord{}, err
	}
	name := o.session
	var empty localShellRecord
	if isShortTaskID(name) {
		name = ""
	}
	var err error
	dir := o.sshDirectory
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			return empty, err
		}
	}
	dir, err = shell.ResolveStartupDirectory(dir, o.startupDirectory)
	if err != nil {
		return empty, err
	}
	env := o.environment
	if env == nil {
		env = os.Environ()
	}

	request := localShellRecord{Name: name, Directory: dir, Version: version}
	c, err := openLocalDaemonContext(ctx, localDaemonRequest{Kind: "shell", Record: request, Env: env, Parent: o.parent})
	if err != nil {
		return empty, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var reply localStartup
	if err = readControlJSON(c, &reply); err != nil {
		return empty, err
	}
	if reply.Error != "" {
		return empty, fmt.Errorf("start local shell: %s", reply.Error)
	}
	if !validTaskUID(reply.Record.UID) || validSession(reply.Record.Name) != nil || reply.Record.Version != version {
		return empty, fmt.Errorf("invalid local shell startup response")
	}
	if err = writeControlJSON(c, "ready"); err != nil {
		return empty, err
	}
	return reply.Record, nil
}
