package main

import (
	"fmt"
	"strings"
)

type selectedTask struct {
	kind, name, identity, instance string
	hosted                         bool
}

// A selector with a host is resolved by that host's daemon. Local selection
// reads registered tasks together so a prefix is checked against one task set.
func resolveShortTask(command string, parsed *cliArguments) (selectedTask, bool, error) {
	var empty selectedTask
	if len(parsed.operands) == 0 || parsed.has("--all") {
		return empty, false, nil
	}
	switch command {
	case "attach", "kill", "rename":
	default:
		return empty, false, nil
	}
	kind, ref, typed := strings.Cut(parsed.operands[0], "/")
	if !typed {
		if !parsed.local {
			return empty, false, nil
		}
		ref, kind = kind, ""
	}
	prefix := strings.ToLower(ref)
	byID := isTaskIDPrefix(prefix)
	snapshot, _, err := readTaskInventory(taskInventoryRequest{Native: true, Hosted: true, Resources: command != "attach", Detail: "health"})
	if err != nil {
		return empty, false, err
	}
	var idMatch, nameMatch selectedTask
	var ambiguousID, ambiguousName bool
	consider := func(task selectedTask) {
		if kind != "" && kind != task.kind && !((kind == "spawn" || kind == "launch") && task.kind == "shell") {
			return
		}
		switch {
		case byID && strings.HasPrefix(taskUID(task.identity), prefix):
			ambiguousID = ambiguousID || idMatch.kind != "" && idMatch.identity != task.identity
			idMatch = task
		case ref == task.name:
			ambiguousName = ambiguousName || nameMatch.kind != "" && nameMatch.identity != task.identity
			nameMatch = task
		}
	}
	for identity, row := range snapshot.Native {
		consider(selectedTask{kind: "shell", name: row.Name, identity: identity})
	}
	for _, row := range snapshot.Shells {
		consider(selectedTask{kind: "shell", name: row.Name, identity: row.identity("local"), instance: row.Instance, hosted: true})
	}
	for _, row := range snapshot.Resources {
		consider(selectedTask{kind: row.Kind, name: row.Name, identity: resourceIdentity(row)})
	}

	if ambiguousID {
		return empty, false, fmt.Errorf("ambiguous task ID %s; use a longer prefix", ref)
	}
	target := idMatch
	if target.kind == "" {
		if ambiguousName {
			return empty, false, usage("ambiguous alias %s; use a task ID", ref)
		}
		target = nameMatch
	}
	if target.kind == "" {
		return empty, false, usage("unknown local task %s; use sshore list", ref)
	}
	if len(parsed.ssh) != 0 || parsed.has("--server") {
		return empty, false, usage("local tasks do not accept SSH options")
	}
	if target.kind == "shell" {
		parsed.local = true
		parsed.operands[0] = target.name
	}
	return target, true, nil
}
