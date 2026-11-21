package main

import (
	"fmt"
	"slices"
)

// A confirmed detach promotes one stable shell identity across all scopes.
// Connection history remains responsible only for SSH defaults and old clients.
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
		if row.Identity != remoteShellIdentity(row.Connection.Host, row.Connection.Instance) {
			return fmt.Errorf("detached attachment identity does not match route")
		}
		return validateRecentConnection(*row.Connection)
	}
	if row.Connection != nil {
		return fmt.Errorf("local detached attachment has remote route")
	}
	return nil
}

func rememberDetachedAttachment(o options) error {
	row := recentAttachment{Identity: o.taskIdentity, Local: o.local, Name: o.session}
	if row.Identity == "" {
		row.Identity = remoteShellIdentity(o.host, o.instance)
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
		id = remoteShellIdentity(o.host, o.instance)
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

func resolveDetachedAttach(o *options) error {
	reply, err := metadataRPC(metadataRequest{Action: "attach_recent", Local: o.local, Host: o.host})
	if err != nil || reply.Target == nil {
		return err
	}
	target := reply.Target
	o.session, o.taskIdentity, o.instance, o.hosted = target.Name, target.Identity, target.Instance, target.Hosted
	if route := target.Connection; route != nil {
		o.host = route.Host
		applyConnectionOptions(o, *route)
	}
	return nil
}

func (a *daemonActivity) selectDetachedAttach(local bool, host string) (*selectedTask, error) {
	if err := a.metadata.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	history := a.metadata.attachments.rows
	if !local {
		for _, row := range history {
			if route := row.Connection; route != nil && route.Host == host {
				return &selectedTask{Name: row.Name, Identity: row.Identity, Instance: route.Instance, Connection: route.clone()}, nil
			}
		}
		return nil, nil
	}

	snapshot, _ := a.tasks.taskInventory(taskInventoryRequest{Native: true, Hosted: true, Detail: "health"})
	detached := make(map[string]*selectedTask)
	for _, row := range snapshot.localShells() {
		if row.State == "detached" {
			target := &selectedTask{Name: row.Name, Identity: row.identity, Hosted: row.hosted}
			if row.hosted {
				target.Instance = canonicalTaskUID(row.identity)
			}
			detached[row.identity] = target
		}
	}
	for _, row := range history {
		if target := detached[row.Identity]; row.Local && target != nil {
			return target, nil
		}
	}
	// A registered shell can be detached without a saved history entry.
	switch len(detached) {
	case 0:
		return nil, fmt.Errorf("no detached local shells; use sshore launch")
	case 1:
		for _, target := range detached {
			return target, nil
		}
	}
	return nil, fmt.Errorf("multiple local shells; use sshore list, then sshore attach :ID")
}
