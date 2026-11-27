package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type taskSelectionRequest struct {
	Action                                  string
	Key, Host, Name, Instance, Kind, Parent string `json:",omitempty"`
}
type taskSelectionReply struct {
	Target *selectedTask `json:",omitempty"`

	Error string `json:",omitempty"`
}

func taskSelectionRPC(r taskSelectionRequest) (taskSelectionReply, error) {
	var reply taskSelectionReply
	if activity := accountActivity.Load(); activity != nil {
		reply = activity.handleTaskSelection(r)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		record, err := ensureLocalDaemonContext(ctx)
		if err != nil {
			return reply, err
		}
		if err := daemonJSON(ctx, record, "/tasks/resolve", r, &reply); err != nil {
			return reply, err
		}
	}
	if reply.Error != "" {
		return reply, fmt.Errorf("%s", reply.Error)
	}
	return reply, nil
}

func (a *daemonActivity) handleTaskSelection(r taskSelectionRequest) (reply taskSelectionReply) {
	var err error
	switch r.Action {
	case "task_by_id", "attach_by_id":
		reply.Target, err = a.selectTaskID(r.Instance, r.Kind, r.Parent, r.Action == "attach_by_id")
	case "task_by_name", "attach_by_name":
		reply.Target, err = a.selectTaskName(r.Name, r.Kind, r.Parent, r.Action == "attach_by_name")
	case "attach_recent":
		reply.Target, err = a.selectDetachedAttach(r.Key, r.Kind)
	default:
		err = fmt.Errorf("invalid task selection operation")
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return
}

type recentConnection struct {
	Hosted                   bool              `json:",omitempty"`
	RouteTarget              bool              `json:",omitempty"`
	Parent                   *recentConnection `json:",omitempty"`
	RouteToken               string            `json:",omitempty"`
	Instance                 string            `json:",omitempty"`
	Host, Session, Directory string
	SSH                      []string
}

func (r recentConnection) viaShell() bool { return r.Hosted || r.Parent != nil }

const maxConnectionHops = 64

func validateRecentConnection(r recentConnection) error {
	for depth, hop := 0, &r; hop != nil; depth, hop = depth+1, hop.Parent {
		if depth >= maxConnectionHops {
			return fmt.Errorf("too many connection hops")
		}
		if hop.Hosted && (hop.Host != "local" || !validTaskUID(hop.Instance) || len(hop.SSH) != 0 || hop.Parent != nil) {
			return fmt.Errorf("invalid hosted shell connection")
		}
		if hop.Parent != nil {
			token := hop.RouteToken
			if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" || hop.Instance == "" || hop.Parent.Instance == "" {
				return fmt.Errorf("invalid saved shell route")
			}
		} else if hop.RouteToken != "" || hop.RouteTarget {
			return fmt.Errorf("shell route parent missing")
		}
		if validSSHDestination(hop.Host) != nil || validSession(hop.Session) != nil {
			return fmt.Errorf("invalid recent connection")
		}
		if hop.Directory != "" && !filepath.IsAbs(hop.Directory) {
			return fmt.Errorf("invalid recent connection directory")
		}
		if len(hop.SSH) > 0 {
			if _, _, err := parseSSHOptions(hop.SSH); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateShellHistoryConnection(record recentConnection) error {
	if record.viaShell() {
		return fmt.Errorf("shell history must use a direct SSH connection")
	}
	return validateRecentConnection(record)
}

// A confirmed detach promotes one stable shell identity across all scopes.
// Connection history supplies direct SSH defaults.
type recentAttachment struct {
	Identity   string
	Local      bool
	Name       string
	Connection *recentConnection `json:",omitempty"`
}

func validateRecentAttachment(row recentAttachment) error {
	if row.Identity == "" || len(row.Identity) > 256 || validSession(row.Name) != nil {
		return fmt.Errorf("invalid detached attachment")
	}
	if !row.Local {
		if row.Connection == nil {
			return fmt.Errorf("detached attachment route missing")
		}
		if row.Identity != identityFromUID(row.Connection.Instance) {
			return fmt.Errorf("detached attachment identity does not match route")
		}
		return validateShellHistoryConnection(*row.Connection)
	}
	if row.Connection != nil {
		return fmt.Errorf("local detached attachment has remote route")
	}
	return nil
}

func (a *daemonActivity) selectDetachedAttach(parent, kind string) (*selectedTask, error) {
	detached := (taskQuery{Kind: kind, Parent: parent}).recentDetached(a.tasks)
	if len(detached) == 0 {
		return nil, fmt.Errorf("no detached local shells; use sshore shell")
	}
	return &detached[0], nil
}

// The SSH peer supplies eligible roots. Local history only ranks those identities.
func recentRemoteChoice(host string, targets []sessionTarget) (string, error) {
	rows, err := accountHistoryRows(attachmentBucket, validateRecentAttachment)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Connection == nil || row.Connection.Host != host {
			continue
		}
		for _, target := range targets {
			if target.Instance == row.Connection.Instance {
				return target.Name, nil
			}
		}
	}
	return "", nil
}
