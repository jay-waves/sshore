package main

import (
	"slices"
	"strings"
)

type selectedTask struct {
	Kind, Name, Identity, Instance string
	Hosted                         bool
	Connection                     *recentConnection
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
	var target selectedTask
	if parsed.bareAttachID {
		reply, err := metadataRPC(metadataRequest{Action: "attach_by_id", Instance: ref})
		if err != nil {
			return empty, false, err
		}
		target = *reply.Target
	} else {
		snapshot, _, err := readTaskInventory(taskInventoryRequest{Native: true, Hosted: true, Resources: command != "attach", Detail: "health"})
		if err != nil {
			return empty, false, err
		}
		target, err = selectTask(snapshot, kind, ref, nil)
		if err != nil {
			return empty, false, err
		}
	}
	if target.Connection != nil {
		parsed.local = false
		parsed.operands = []string{target.Connection.Host, target.Instance}
		return target, true, nil
	}
	if len(parsed.ssh) != 0 || parsed.has("--server") {
		return empty, false, usage("local tasks do not accept SSH options")
	}
	if target.Kind == "shell" {
		parsed.local = true
		parsed.operands[0] = target.Name
	}
	return target, true, nil
}

func selectTask(snapshot taskInventorySnapshot, kind, ref string, routes []recentConnection) (selectedTask, error) {
	var idMatch, nameMatch selectedTask
	var ambiguousID, ambiguousName, ambiguousRoute bool
	prefix := strings.ToLower(ref)
	byID := isTaskIDPrefix(prefix)
	consider := func(task selectedTask) {
		if kind != "" && kind != task.Kind && !((kind == "spawn" || kind == "launch") && task.Kind == "shell") {
			return
		}
		switch {
		case byID && strings.HasPrefix(taskUID(task.Identity), prefix):
			ambiguousID = ambiguousID || idMatch.Kind != "" && idMatch.Identity != task.Identity
			if idMatch.Identity == task.Identity && task.Connection != nil {
				// Prefer the registered local shell when SSH reaches the same task.
				if saved := idMatch.Connection; saved != nil {
					route := task.Connection
					ambiguousRoute = ambiguousRoute || saved.Host != route.Host || saved.Server != route.Server ||
						saved.Directory != route.Directory || !slices.Equal(saved.SSH, route.SSH)
				}
				return
			}
			idMatch = task
		case ref == task.Name:
			ambiguousName = ambiguousName || nameMatch.Kind != "" && nameMatch.Identity != task.Identity
			nameMatch = task
		}
	}
	for identity, row := range snapshot.Native {
		consider(selectedTask{Kind: "shell", Name: row.Name, Identity: identity})
	}
	for _, row := range snapshot.Shells {
		consider(selectedTask{Kind: "shell", Name: row.Name, Identity: row.identity("local"), Instance: row.Instance, Hosted: true})
	}
	for _, row := range snapshot.Resources {
		consider(selectedTask{Kind: row.Kind, Name: row.Name, Identity: resourceIdentity(row)})
	}

	for i := range routes {
		route := &routes[i]
		if validTaskUID(route.Instance) && !route.viaShell() {
			consider(selectedTask{Kind: "shell", Identity: "task:" + route.Instance, Instance: route.Instance, Connection: route})
		}
	}
	switch {
	case ambiguousID:
		return selectedTask{}, usage("ambiguous task ID %s; use a longer prefix or HOST:ID", ref)
	case ambiguousRoute:
		return selectedTask{}, usage("multiple saved connections for task %s; use sshore attach HOST:ID", idMatch.Instance)
	case idMatch.Kind != "":
		return idMatch, nil
	case ambiguousName:
		return selectedTask{}, usage("ambiguous alias %s; use a task ID", ref)
	case nameMatch.Kind != "":
		return nameMatch, nil
	}
	return selectedTask{}, usage("unknown task %s; use sshore list or specify HOST:ID", ref)
}
