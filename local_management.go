package main

import (
	"fmt"
	"os"
)

func (s *localShell) rename(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle.Exited() {
		return fmt.Errorf("local shell has ended")
	}
	if s.record.Name == name {
		return nil
	}
	reservation, err := reserveResourceName("shell", name)
	if err != nil {
		return err
	}
	defer reservation.Close()
	next := s.record
	next.Name = name
	path, err := localSessionPath(name)
	if err != nil {
		return err
	}
	if err = writeStateJSON(path, next); err != nil {
		return err
	}
	oldPath, _ := localSessionPath(s.record.Name)
	if err = os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		os.Remove(path)
		return err
	}
	s.record = next
	s.nameLock.Close()
	s.nameLock = reservation.owner
	reservation.owner = nil
	return nil
}

func (s *localShell) targets(request message) message {
	s.mu.Lock()
	name := s.record.Name
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: name}
	if request.Action == "new" {
		reservation, err := reserveResourceName("shell", "")
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		reply.Names = []string{reservation.name}
		reservation.Close()
		return reply
	}
	rows, err := localShells()
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	for _, row := range rows {
		if row.State != "detached" {
			continue
		}
		if request.Action == "select" && row.Name != request.Name {
			continue
		}
		reply.Names = append(reply.Names, row.Name)
	}
	if request.Action == "select" && len(reply.Names) != 1 {
		reply.Error = fmt.Sprintf("local shell %s unavailable", cleanText(request.Name))
	}
	if len(reply.Names) > 128 {
		reply.Names = nil
		reply.Error = "too many local shells; use sshore list --local"
	}
	return reply
}

func detachLocalShell(identity string) error {
	rows, err := localShells()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.identity != identity {
			continue
		}
		m, err := localRequest(row.Name, message{Type: "detach"}, identity)
		if err != nil {
			return err
		}
		if m.Type != "detach_requested" && m.Type != "detached" {
			return fmt.Errorf("local detach unconfirmed")
		}
		return nil
	}
	return fmt.Errorf("local shell is unavailable")
}
