//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jay-waves/sshore/internal/mcp"
)

type hostedTaskRecord struct {
	ID, Name string
}

func (tasks *daemonTasks) hostedRecords() []hostedTaskRecord {
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	tasks.prune(time.Now())
	var records []hostedTaskRecord
	for id, task := range tasks.records {
		if shell, ok := task.(*remoteShell); ok {
			shell.mu.Lock()
			records = append(records, hostedTaskRecord{ID: id, Name: shell.name})
			shell.mu.Unlock()
		}
	}
	return records
}

func hostedTaskRecords() ([]hostedTaskRecord, error) {
	r, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := dialLocalSession(r)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := writeMessage(c, message{Type: "task_list_hosted", SoftwareVersion: version}); err != nil {
		return nil, err
	}
	var records []hostedTaskRecord
	err = json.NewDecoder(c).Decode(&records)
	return records, err
}

func dialHostedTask(ctx context.Context, ref string, timeout time.Duration) (net.Conn, error) {
	r, err := existingDaemonRecord()
	if err != nil {
		return nil, err
	}
	if !validTaskUID(ref) {
		return nil, fmt.Errorf("invalid task ID")
	}
	id := ref
	r.UID = id
	return dialLocalSessionContext(ctx, r, time.Now().Add(timeout))
}

type unixDaemonServices struct {
	sessions *trackedMCPSessions
}

func newDaemonServices(tasks *daemonTasks) daemonServices {
	return &unixDaemonServices{sessions: newTrackedMCPSessions(tasks, checkMCPSessionSetup)}
}

func (s *unixDaemonServices) Close() { s.sessions.closeAll() }

func validateDaemonRuntime(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("invalid shell runtime directory")
	}
	return privateStatePath(dir, true)
}

func (s *unixDaemonServices) Serve(c net.Conn, request message, tasks *daemonTasks) {
	fail := func(err error) {
		_ = writeMessage(c, message{Type: "error", SoftwareVersion: version, Error: err.Error()})
	}
	if err := checkPeerVersion(request.SoftwareVersion, "client"); err != nil {
		fail(err)
		return
	}
	if request.Type == "task_list_hosted" {
		_ = json.NewEncoder(c).Encode(tasks.hostedRecords())
		return
	}
	if request.Type == "mcp_request" && request.MCPRequest != nil {
		_ = c.SetDeadline(time.Now().Add(40 * time.Second))
		response := mcp.Dispatch(context.Background(), s.sessions, *request.MCPRequest, version)
		_ = writeMessage(c, message{Type: "mcp_response", SoftwareVersion: version, MCPResponse: &response})
		return
	}
	if request.Type != "create_shell" || request.LocalRequest == nil || !validTaskUID(request.Instance) || validSession(request.Name) != nil || validSession(request.Action) != nil || !filepath.IsAbs(request.LocalRequest.Record.Directory) {
		fail(fmt.Errorf("invalid shell creation"))
		return
	}
	if err := validateDaemonRuntime(request.Directory); err != nil {
		fail(err)
		return
	}
	ready := false
	err := serveRemoteShellAt(tasks, request.Directory, request.Action, request.Instance, request.LocalRequest.Env, request.LocalRequest.Record.Directory, func() error {
		err := writeMessage(c, message{Type: "shell_created", SoftwareVersion: version})
		if err == nil {
			ready = true
		}
		c.Close()
		return err
	}, request.Temporary)
	if err != nil && !ready {
		fail(err)
	}
	return
}

// SSH bootstrap and native clients authenticate to the same account daemon.
func connectRemoteDaemon(executable string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record, err := ensureLocalDaemonContext(ctx, executable)
	if err != nil {
		return nil, err
	}
	return dialLocalSessionContext(ctx, record, time.Now().Add(5*time.Second))
}

func createRemoteShell(executable string, id taskIdentitySelection, temporary bool, cwd string, env []string) error {
	c, err := connectRemoteDaemon(executable)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	request := message{Directory: dir, Type: "create_shell", Instance: id.uid, Temporary: temporary, SoftwareVersion: version, Name: id.uid, Action: id.name, LocalRequest: &localDaemonRequest{Record: localShellRecord{Directory: cwd}, Env: env}}
	if err = writeMessage(c, request); err != nil {
		return err
	}
	reply, err := readMessage(c)
	if err != nil {
		return err
	}
	if reply.Type != "shell_created" || reply.SoftwareVersion != version {
		return fmt.Errorf("remote shell creation failed: %s", reply.Error)
	}
	return nil
}
