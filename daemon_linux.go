//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	var records []hostedTaskRecord
	err = daemonJSON(context.Background(), r, "/tasks/hosted", nil, &records)
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

type hostedShellRequest struct {
	Runtime, Directory, Name, UID string
	Env                           []string
	Temporary                     bool
}

func (s *unixDaemonServices) Routes(mux *http.ServeMux, tasks *daemonTasks) {
	mux.HandleFunc("POST /tasks/hosted", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(tasks.hostedRecords()) })
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		var req mcp.Request
		if readDaemonBody(w, r, &req) {
			_ = json.NewEncoder(w).Encode(mcp.Dispatch(r.Context(), s.sessions, req, version))
		}
	})
	mux.HandleFunc("POST /shells", func(w http.ResponseWriter, r *http.Request) {
		var req hostedShellRequest
		if !readDaemonBody(w, r, &req) {
			return
		}
		if !validTaskUID(req.UID) || validSession(req.Name) != nil || !filepath.IsAbs(req.Directory) || !filepath.IsAbs(req.Runtime) {
			http.Error(w, "invalid shell creation", 400)
			return
		}
		if err := privateStatePath(req.Runtime, true); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := tasks.registerHostedShell(req); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errDaemonDraining) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
func createRemoteShell(executable string, id taskIdentitySelection, temporary bool, cwd string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record, err := ensureLocalDaemonContext(ctx, executable)
	if err != nil {
		return err
	}
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	req := hostedShellRequest{Runtime: dir, Directory: cwd, Name: id.name, UID: id.uid, Env: env, Temporary: temporary}
	return daemonJSON(ctx, record, "/shells", req, nil)
}
