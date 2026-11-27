package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jay-waves/sshore/internal/shell"
)

type localShellView struct {
	SourceHost  string    `json:"source_host,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	Pinned      bool      `json:"pinned,omitzero"`
	PinUntil    time.Time `json:"pin_until,omitzero"`
	ShortID     string    `json:"short_id,omitempty"`
	Identity    string    `json:"identity"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Shell       string    `json:"shell,omitempty"`
	Program     bool      `json:"program,omitzero"`
	ProgramLine string    `json:"program_line,omitempty"`
	Directory   string    `json:"directory,omitempty"`
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
