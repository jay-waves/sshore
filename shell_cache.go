package main

import (
	"fmt"
	"os"
	"slices"
)

// A confirmed detach promotes one stable shell identity across all scopes.
// Connection history supplies direct SSH defaults.
type recentAttachment struct {
	Identity   string
	Local      bool
	Name       string
	Connection *recentConnection `json:",omitempty"`
}

func (m *daemonMetadata) rememberDetachedAttachmentLocal(row recentAttachment) error {
	if err := validateRecentAttachment(row); err != nil {
		return err
	}
	if err := m.loadDetachedAttachments(); err != nil {
		return err
	}
	row.Connection = row.Connection.clone()
	return m.attachments.promote(row, func(previous recentAttachment) bool {
		return previous.Identity == row.Identity
	})
}

func (m *daemonMetadata) loadDetachedAttachments() error {
	return m.attachments.load("detached.json", 1024*1024, validateRecentAttachment)
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

func rememberDetachedAttachment(o options) error {
	if ownedRemoteRoute(o) {
		return nil
	}
	o = directShellOptions(o)
	row := recentAttachment{Identity: o.taskIdentity, Local: o.local, Name: o.session}
	if row.Identity == "" {
		row.Identity = identityFromUID(o.instance)
	}
	if !o.local {
		connection, err := connectionFromOptions(o)
		if err != nil {
			return err
		}
		row.Connection = &connection
	}
	_, err := metadataRPC(metadataRequest{Action: "attach_detach", Attachment: &row})
	return err
}

func removeRecentDetach(o options) error {
	id := o.taskIdentity
	if id == "" {
		id = identityFromUID(o.instance)
	}
	if id == "" {
		return nil
	}
	_, err := metadataRPC(metadataRequest{Action: "attach_remove", Key: id})
	return err
}

func (m *daemonMetadata) removeRecentDetachLocal(id string) error {
	if err := m.loadDetachedAttachments(); err != nil {
		return err
	}
	rows := m.attachments.rows
	for _, row := range rows {
		if row.Identity == id {
			return m.attachments.replace(slices.DeleteFunc(slices.Clone(rows), func(entry recentAttachment) bool { return entry.Identity == id }))
		}
	}
	return nil
}

func resolveDetachedAttach(o *options, parent string) error {
	kind := "shell"
	if parent != "" && os.Getenv("SSHORE_SOCKET") != "" {
		kind = "hosted"
	}
	query := taskQuery{Kind: kind, Parent: parent, Ref: o.session, Detached: true}
	var target *selectedTask
	if query.Ref != "" {
		rows, err := queryRegisteredTasks(query)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return missingAttachTarget(query.Ref)
		}
		target = &rows[0]
	} else {
		reply, err := metadataRPC(metadataRequest{Action: "attach_recent", Key: parent, Kind: kind})
		if err != nil || reply.Target == nil {
			return err
		}
		target = reply.Target
	}
	o.session, o.taskIdentity, o.instance, o.hosted = target.Name, target.Identity, target.Instance, target.Hosted
	return nil
}

func (a *daemonActivity) selectDetachedAttach(parent, kind string) (*selectedTask, error) {
	if err := a.metadata.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	detached := (taskQuery{Kind: kind, Parent: parent, Detached: true}).registered(a.tasks)
	for _, row := range a.metadata.attachments.rows {
		if row.Local {
			for _, target := range detached {
				if row.Identity == target.Identity {
					return &target, nil
				}
			}
		}
	}
	switch len(detached) {
	case 0:
		return nil, fmt.Errorf("no detached local shells; use sshore sh")
	case 1:
		return &detached[0], nil
	default:
		return nil, fmt.Errorf("multiple local shells; use sshore list, then sshore attach :ID")
	}
}

// The SSH peer supplies eligible roots. Local history only ranks those identities.
func (m *daemonMetadata) recentRemoteChoice(host string, targets []sessionTarget) (string, error) {
	if err := m.loadDetachedAttachments(); err != nil {
		return "", err
	}
	for _, row := range m.attachments.rows {
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
