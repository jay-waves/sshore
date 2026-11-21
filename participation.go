package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Participation records only tasks opened through this client account.
type participatingShell struct {
	Host, Instance, Name, State, Lifetime string
	PID                                   int
	Temporary                             bool
	Attachment                            string
	ChangedAt                             time.Time
}

type participationRefresh struct {
	Shells []shellView
	Since  time.Time
}

func readParticipatingShells(host string) ([]participatingShell, error) {
	reply, err := metadataRPC(metadataRequest{Action: "participation_get", Host: host})
	return reply.Participants, err
}

func reconcileParticipatingShells(host string, shells []shellView, since time.Time) error {
	refresh := participationRefresh{Shells: shells, Since: since}
	_, err := metadataRPC(metadataRequest{Action: "participation_refresh", Host: host, Participation: &refresh})
	return err
}

func forgetParticipatingShell(host, instance string) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_forget", Host: host, Instance: instance})
	return err
}

func renameParticipatingShell(host, instance, name string) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_rename", Host: host, Instance: instance, Name: name})
	return err
}

func observeParticipatingShell(row participatingShell) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_observe", Participant: &row})
	return err
}

func setParticipatingShellState(row participatingShell) error {
	_, err := metadataRPC(metadataRequest{Action: "participation_state", Participant: &row})
	return err
}

func participatingMutationTarget(o options) (participatingShell, error) {
	rows, err := readParticipatingShells(o.host)
	if err != nil {
		return participatingShell{}, err
	}
	var matched participatingShell
	ambiguous := false
	prefix := strings.ToLower(o.session)
	for _, row := range rows {
		if row.Instance == o.instance || o.instance == "" && row.Instance == prefix {
			return row, nil
		}
		if o.instance != "" || row.Name != o.session && !(isTaskIDPrefix(o.session) && strings.HasPrefix(row.Instance, prefix)) {
			continue
		}
		if matched.Instance != "" {
			ambiguous = true
		}
		matched = row
	}
	if ambiguous {
		return participatingShell{}, nil
	}
	return matched, nil
}

func validateParticipatingShell(row participatingShell) error {
	if validSSHDestination(row.Host) != nil || !validTaskUID(row.Instance) ||
		validSession(row.Name) != nil || row.PID <= 0 || !validTaskUID(row.Attachment) || !validShellLifetime(row.Lifetime) {
		return fmt.Errorf("invalid participating shell")
	}
	switch row.State {
	case "attached", "detached", "unconfirmed":
		return nil
	default:
		return fmt.Errorf("invalid participating shell state")
	}
}

func (m *daemonMetadata) loadParticipation() error {
	return m.participation.loadEntries("participation.json", 1024*1024, 4096, validateParticipatingShell)
}

func (m *daemonMetadata) readParticipatingShellsLocal(host string) ([]participatingShell, error) {
	if err := m.loadParticipation(); err != nil {
		return nil, err
	}
	var rows []participatingShell
	for _, row := range m.participation.rows {
		if host == "" || row.Host == host {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (m *daemonMetadata) updateParticipation(r metadataRequest) error {
	if err := m.loadParticipation(); err != nil {
		return err
	}
	rows := slices.Clone(m.participation.rows)
	now := time.Now()
	switch r.Action {
	case "participation_observe", "participation_state":
		if r.Participant == nil {
			return fmt.Errorf("missing participating shell")
		}
		row := *r.Participant
		if err := validateParticipatingShell(row); err != nil {
			return err
		}
		row.ChangedAt = now
		i := slices.IndexFunc(rows, func(old participatingShell) bool {
			return old.Host == row.Host && old.Instance == row.Instance
		})
		if r.Action == "participation_observe" {
			if i < 0 {
				if len(rows) == 4096 {
					return fmt.Errorf("participating shell limit reached")
				}
				rows = append(rows, row)
			} else {
				rows[i] = row
			}
		} else if i >= 0 && rows[i].Attachment == row.Attachment {
			if row.State == "detached" {
				rows[i].Name = row.Name
			}
			rows[i].State, rows[i].ChangedAt = row.State, now
		}
	case "participation_forget", "participation_rename":
		if !validTaskUID(r.Instance) || validSSHDestination(r.Host) != nil {
			return fmt.Errorf("invalid participating shell identity")
		}
		i := slices.IndexFunc(rows, func(row participatingShell) bool {
			return row.Host == r.Host && row.Instance == r.Instance
		})
		if i >= 0 {
			if r.Action == "participation_forget" {
				rows = slices.Delete(rows, i, i+1)
			} else {
				if err := validSession(r.Name); err != nil {
					return err
				}
				rows[i].Name, rows[i].ChangedAt = r.Name, now
			}
		}
	case "participation_refresh":
		if r.Participation == nil || validSSHDestination(r.Host) != nil {
			return fmt.Errorf("invalid participating shell refresh")
		}
		rows = refreshParticipation(rows, r.Host, *r.Participation, now)
	}
	if slices.Equal(rows, m.participation.rows) {
		return nil
	}
	return m.participation.replace(rows)
}

func refreshParticipation(rows []participatingShell, host string, refresh participationRefresh, now time.Time) []participatingShell {
	observed := make(map[string]shellView, len(refresh.Shells))
	for _, view := range refresh.Shells {
		if validTaskUID(view.Instance) {
			observed[view.Instance] = view
		}
	}
	updated := rows[:0]
	for _, row := range rows {
		if row.Host == host && row.ChangedAt.Before(refresh.Since) {
			view, found := observed[row.Instance]
			if !found {
				continue
			}
			if validSession(view.Name) == nil && view.PID > 0 && validShellLifetime(view.Lifetime) && (view.State == "attached" || view.State == "detached") {
				if row.Name != view.Name || row.State != view.State || row.PID != view.PID || row.Temporary != view.Temporary || row.Lifetime != view.Lifetime {
					row.Name, row.State, row.PID = view.Name, view.State, view.PID
					row.Temporary, row.Lifetime, row.ChangedAt = view.Temporary, view.Lifetime, now
				}
			}
		}
		updated = append(updated, row)
	}
	return updated
}
