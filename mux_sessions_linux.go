package main

import "fmt"

func (s *remoteShell) sessionTargets(request message) message {
	s.mu.Lock()
	current, plain := s.name, s.plain
	s.mu.Unlock()
	reply := message{Type: "session_targets", Action: request.Action, Name: current}
	if plain {
		reply.Error = "session switching is disabled in recovery"
		return reply
	}
	if request.Action == "new" {
		occupied, err := occupiedObjectNames("")
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		for _, name := range automaticSessionNames() {
			if !occupied[name] {
				reply.Names = []string{name}
				return reply
			}
		}
		reply.Error = "no automatic shell names available"
		return reply
	}
	names, err := detachedSessionNames()
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	if request.Action == "select" {
		for _, name := range names {
			if name == request.Name {
				reply.Names = []string{name}
				return reply
			}
		}
		reply.Error = fmt.Sprintf("shell %s is unavailable", cleanText(request.Name))
		return reply
	}
	reply.Names = names
	return reply
}
