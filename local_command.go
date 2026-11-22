package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"
)

type localShellView struct {
	hosted        bool
	Lifetime      string `json:"lifetime"`
	Temporary     bool   `json:"temporary,omitempty"`
	ShortID       string `json:"short_id,omitempty"`
	identity      string
	Name          string `json:"name"`
	State         string `json:"state"`
	ConnectedHost string `json:"connected_host,omitempty"`
	PID           int    `json:"pid,omitempty"`
	Version       string `json:"version"`
	Directory     string `json:"directory,omitempty"`
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

func localShells(withDirectory bool) ([]localShellView, error) {
	detail := "health"
	if withDirectory {
		detail = "inventory"
	}
	snapshot, _, err := readTaskInventory(taskInventoryRequest{Native: true, Hosted: true, Detail: detail})
	if err != nil {
		return nil, err
	}
	return snapshot.localShells(), nil
}

func (snapshot taskInventorySnapshot) localShells() []localShellView {
	rows := snapshot.nativeShells()
	for _, s := range snapshot.Shells {
		identity := s.identity("local")
		rows = append(rows, localShellView{hosted: true, Lifetime: s.Lifetime, Temporary: s.Temporary, ShortID: taskShortCode(identity), identity: identity, Name: s.Name, State: s.State, PID: s.PID, Directory: s.Directory})
	}
	sortLocalShells(rows)
	return rows
}

func sortLocalShells(rows []localShellView) {
	slices.SortStableFunc(rows, func(a, b localShellView) int {
		return cmp.Compare(taskUID(a.identity), taskUID(b.identity))
	})
}

func runLocalCommand(o options) (int, error) {
	if o.hosted {
		return runHostedCommand(o)
	}
	switch o.command {
	case "launch", "spawn", "attach":
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
			fmt.Fprintf(os.Stdout, "Renamed local shell %s to %s\n", o.session, o.newName)
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

func startLocalSessionContext(ctx context.Context, o options) (localShellRecord, error) {
	if err := ctx.Err(); err != nil {
		return localShellRecord{}, err
	}
	name := o.session
	var empty localShellRecord
	if isShortTaskID(name) {
		name = ""
	}
	reservation, err := chooseTaskIdentity("shell", name)
	if err != nil {
		return empty, err
	}

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

	request := localShellRecord{UID: reservation.uid, Name: reservation.name, Directory: dir, Version: version}
	request.Owner = o.owner
	c, err := openLocalDaemonContext(ctx, localDaemonRequest{Kind: "shell", Record: request, Env: env})
	if err != nil {
		return empty, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var reply localStartup
	if err = json.NewDecoder(c).Decode(&reply); err != nil {
		return empty, err
	}
	if reply.Error != "" {
		return empty, fmt.Errorf("start local shell: %s", reply.Error)
	}
	if reply.Record.Name != request.Name || reply.Record.UID != request.UID || reply.Record.Version != version {
		return empty, fmt.Errorf("invalid local shell startup response")
	}
	if err = json.NewEncoder(c).Encode("ready"); err != nil {
		return empty, err
	}
	return reply.Record, nil
}
