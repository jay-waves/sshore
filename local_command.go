package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/jay-waves/sshore/internal/jsonstream"
	"github.com/jay-waves/sshore/internal/termui"
)

type localShellView struct {
	SourceHost string    `json:"source_host,omitempty"`
	Owner      string    `json:"owner,omitempty"`
	Pinned     bool      `json:"pinned,omitempty"`
	PinUntil   time.Time `json:"pin_until,omitzero"`
	ShortID    string    `json:"short_id,omitempty"`
	Identity   string    `json:"identity"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	PID        int       `json:"pid,omitempty"`
	Version    string    `json:"version"`
	Shell      string    `json:"shell,omitempty"`
	Directory  string    `json:"directory,omitempty"`
}

func localRequest(ref, identity string, request message) (message, error) {
	c, record, err := openNativeShell(context.Background(), ref, identity)
	if err != nil {
		return message{}, err
	}
	defer c.Close()
	switch request.Type {
	case "handoff":
		_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	case "rename":
		request.Action = record.Name
	}
	if err = writeMessage(c, request); err != nil {
		return message{}, err
	}
	return readMessage(c)
}

func nativeShells() ([]localShellView, error) {
	snapshot, _, err := readTaskInventory(taskInventoryRequest{Native: true})
	return snapshot.nativeShells(), err
}

func sortLocalShells(rows []localShellView) {
	slices.SortStableFunc(rows, func(a, b localShellView) int {
		return cmp.Compare(taskUID(a.Identity), taskUID(b.Identity))
	})
}

func runLocalCommand(o options) (int, error) {
	if o.hosted {
		return runHostedCommand(o)
	}
	switch o.command {
	case "sh", "attach":
		return runClient(o)
	case "kill":
		m, err := localRequest(o.session, o.taskIdentity, message{Type: "kill"})
		if err == nil && m.Type != "killed" {
			err = fmt.Errorf("kill local shell: %s", cleanText(m.Error))
		}
		return 0, err
	case "rename":
		m, err := localRequest(o.session, o.taskIdentity, message{Type: "rename", Name: o.newName})
		if err == nil && m.Type != "renamed" {
			err = fmt.Errorf("rename local shell: %s", cleanText(m.Error))
		}
		if err == nil {
			termui.Log(os.Stdout, taskShortCode(o.taskIdentity), termui.Info, "Renamed local shell %s to %s", o.session, o.newName)
		}
		return 0, err
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
	parent          *shellTask
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
	if err = jsonstream.NewDecoder(c).Decode(&reply); err != nil {
		return empty, err
	}
	if reply.Error != "" {
		return empty, fmt.Errorf("start local shell: %s", reply.Error)
	}
	if !validTaskUID(reply.Record.UID) || validSession(reply.Record.Name) != nil || reply.Record.Version != version {
		return empty, fmt.Errorf("invalid local shell startup response")
	}
	if err = jsonstream.NewEncoder(c).Encode("ready"); err != nil {
		return empty, err
	}
	return reply.Record, nil
}
