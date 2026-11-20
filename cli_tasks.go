package main

import (
	"fmt"
	"strings"
)

type selectedTask struct {
	kind, name, host, identity, instance string
	snapshot                             *shellSnapshot
	hosted                               bool
}

// Selection reads local identities only. Execution still validates the selected
// instance against the daemon or resource record before acting on it.
func resolveShortTask(command string, parsed *cliArguments) (selectedTask, bool, error) {
	var empty selectedTask
	if len(parsed.operands) == 0 || parsed.has("--all") || parsed.explicitHost && len(parsed.operands) == 1 && !parsed.local {
		return empty, false, nil
	}
	switch command {
	case "attach", "kill", "rename":
	default:
		return empty, false, nil
	}
	position := 0
	compact := len(parsed.operands) == 1 || command == "rename" && len(parsed.operands) == 2
	if !compact && !parsed.local {
		position = 1
	}
	ref := parsed.operands[position]
	kind, code, typed := strings.Cut(ref, "/")
	if !typed {
		code = ref
		kind = ""
	}
	localTask := parsed.local && (command == "kill" || command == "rename")
	localShell := parsed.local && !localTask
	prefix := ""
	if isTaskIDPrefix(code) {
		prefix = strings.ToLower(code)
	}
	alias := ""
	if compact && !typed && (prefix == "" || len(prefix) < 6) {
		alias = ref
	}
	if prefix == "" && alias == "" {
		return empty, false, nil
	}
	host := ""
	if position == 1 {
		host = parsed.operands[0]
	}
	selections, err := readTaskSelections(command, alias, prefix, host, localShell)
	if err != nil {
		return empty, false, err
	}
	scoped := typed || localShell || localTask || host != ""
	var target selectedTask
	found := false
	matched := len(selections.byID) > 0
	for _, candidate := range selections.byID {
		if scoped {
			if candidate.kind == "" || candidate.kind == "peer" {
				continue
			}
			if typed && kind != candidate.kind && !((kind == "spawn" || kind == "launch") && candidate.kind == "shell") {
				continue
			}
			if localShell && (candidate.kind != "shell" || candidate.host != "") || localTask && candidate.host != "" {
				continue
			}
		}
		if found {
			return empty, false, fmt.Errorf("ambiguous task ID %s; use a longer prefix, full UID, HOST or a type prefix", code)
		}
		target, found = candidate, true
	}
	// A hexadecimal alias is a fallback only after checking the ID prefix.
	if !found && !matched && alias != "" {
		if selections.aliasAmbiguous {
			return empty, false, usage("ambiguous alias %s; use a task ID or explicit scope", alias)
		}
		target, found = selections.alias, selections.alias.kind != ""
	}
	if !found {
		if matched {
			return empty, false, usage("task ID %s does not belong to the requested scope", code)
		}
		if prefix == "" {
			return empty, false, nil
		}
		// Explicit HOST/here:/type scopes still accept hexadecimal names.
		if compact && !typed && !parsed.local {
			return empty, false, usage("unknown task ID %s; use sshore list", ref)
		}
		return empty, false, nil
	}
	switch target.kind {
	case "peer":
		return empty, false, usage("peer tasks are read-only here; kill the resource on its managing computer")
	case "":
		return empty, false, fmt.Errorf("task is no longer available here; refresh with sshore list")
	}
	return routeSelectedTask(command, parsed, target, position)
}

type taskSelections struct {
	byID           map[string]selectedTask
	alias          selectedTask
	aliasAmbiguous bool
}

// Compare two observations of the same identity. A registered local shell
// takes priority over saved history, and a native shell over a hosted copy.
func (t selectedTask) preferredTo(old selectedTask) bool {
	if old.kind == "" {
		return true
	}
	if t.snapshot == nil {
		return t.kind == "shell" && (old.snapshot != nil || !t.hosted && old.hosted)
	}
	if old.snapshot == nil {
		return false
	}
	ended, oldEnded := endedShellState(t.snapshot.State), endedShellState(old.snapshot.State)
	if ended != oldEnded {
		return !ended
	}
	return t.snapshot.ConfirmedAt.After(old.snapshot.ConfirmedAt)
}

// Retain only prefix matches. An empty entry records an observed identity that
// cannot supply a route in the requested scope.
func readTaskSelections(command, alias, prefix, host string, localShell bool) (taskSelections, error) {
	var selections taskSelections
	byID := prefix != ""
	if byID {
		selections.byID = make(map[string]selectedTask)
	}
	addAlias := func(task selectedTask) {
		if alias == "" || task.name != alias {
			return
		}
		if localShell && (task.kind != "shell" || task.host != "") {
			return
		}
		if command == "attach" && task.kind != "shell" {
			return
		}
		if selections.alias.kind != "" && selections.alias.identity != task.identity {
			selections.aliasAmbiguous = true
		} else if task.preferredTo(selections.alias) {
			selections.alias = task
		}
	}
	matchesID := func(identity string) bool {
		return byID && identity != "" && strings.HasPrefix(taskUID(identity), prefix)
	}
	add := func(task selectedTask, allowAlias, inScope bool) {
		if allowAlias {
			addAlias(task)
		}
		if !matchesID(task.identity) {
			return
		}
		current, observed := selections.byID[task.identity]
		if inScope && task.preferredTo(current) {
			selections.byID[task.identity] = task
		} else if !observed {
			selections.byID[task.identity] = selectedTask{}
		}
	}

	{
		rows, err := localShells(false)
		if err != nil {
			return taskSelections{}, err
		}
		for _, row := range rows {
			task := selectedTask{kind: "shell", name: row.Name, identity: row.identity, hosted: row.hosted}
			if row.hosted {
				task.instance = canonicalTaskUID(row.identity)
			}
			add(task, true, host == "")
		}
	}

	if byID {
		snapshots, err := readShellSnapshots()
		if err != nil {
			return taskSelections{}, err
		}
		for i := range snapshots {
			record := &snapshots[i]
			if record.Forgotten {
				continue
			}
			task := snapshotTaskSelection(record)
			add(task, false, record.State != "renamed" && (host == "" || record.Host == host))
		}
	}

	if byID || (!localShell && command != "attach") {
		resources, err := collectLocalResources("", false)
		if err != nil {
			return taskSelections{}, err
		}
		for _, resource := range resources {
			kind, name := resource.kindAndName()
			task := selectedTask{kind: kind, name: name, identity: resource.Identity}
			add(task, true, host == "")
		}
	}
	if byID {
		peers, err := collectPeerViews()
		if err != nil {
			return taskSelections{}, err
		}
		for _, peer := range peers {
			if validTaskUID(peer.UID) && selections.byID["task:"+peer.UID].kind != "" {
				continue
			}
			if identity := peerTaskIdentity(peer); matchesID(identity) {
				selections.byID[identity] = selectedTask{kind: "peer"}
			}
		}
	}
	return selections, nil
}

func snapshotTaskSelection(record *shellSnapshot) selectedTask {
	return selectedTask{
		kind: "shell", name: record.Name, host: record.Host,
		identity: remoteShellIdentity(record.Host, record.Instance), instance: record.Instance, snapshot: record,
	}
}

func routeSelectedTask(command string, parsed *cliArguments, target selectedTask, position int) (selectedTask, bool, error) {
	empty := selectedTask{}
	if command == "attach" && target.kind != "shell" {
		return empty, false, usage("%s does not accept %s tasks", command, target.kind)
	}
	if target.kind != "shell" {
		if len(parsed.ssh) != 0 || parsed.has("--server") {
			return empty, false, usage("local resources do not accept SSH options")
		}
		return target, true, nil
	}
	if target.host == "" {
		parsed.local = true
		parsed.operands = append([]string{target.name}, parsed.operands[position+1:]...)
	} else {
		parsed.operands = append([]string{target.host, target.name}, parsed.operands[position+1:]...)
	}
	return target, true, nil
}
