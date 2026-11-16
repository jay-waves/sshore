package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

type selectedTask struct {
	kind, name, host, identity, instance, requestID string
	connection                                      *recentConnection
}

// Selection reads local identities only. Execution still validates the selected
// instance against the daemon or resource record before acting on it.
func resolveShortTask(command string, parsed *cliArguments) (selectedTask, bool, error) {
	var empty selectedTask
	if len(parsed.operands) == 0 {
		return empty, false, nil
	}
	switch command {
	case "attach", "cancel", "rename", "review":
	default:
		return empty, false, nil
	}
	position := 0
	compact := len(parsed.operands) == 1 || command == "rename" && len(parsed.operands) == 2
	if !compact && !parsed.has("--local") && command != "review" {
		position = 1
	}
	ref := parsed.operands[position]
	kind, code, typed := strings.Cut(ref, "/")
	if !typed {
		code = ref
		kind = ""
	}
	raw, err := hex.DecodeString(code)
	if err != nil || len(raw) != 3 {
		return empty, false, nil
	}
	path, err := statePath("tasks/ids.json")
	if err != nil {
		return empty, false, err
	}
	index, err := readTaskIDIndex(path)
	if err != nil {
		return empty, false, err
	}
	identity := ""
	for key, value := range index.Codes {
		if value == strings.ToLower(code) {
			if identity != "" {
				return empty, false, fmt.Errorf("ambiguous task ID %s", code)
			}
			identity = key
		}
	}
	if parsed.has("--all") {
		if identity != "" {
			return empty, false, usage("cancel ID does not accept --all")
		}
		return empty, false, nil
	}
	if identity == "" {
		// Explicit HOST/--local/type scopes still accept hexadecimal names.
		if compact && !typed && !parsed.has("--local") {
			if command == "review" {
				dir, err := approvalDir()
				if err != nil {
					return empty, false, err
				}
				if record, err := findApproval(dir, ref); err == nil && record.DisplayName() == ref {
					return empty, false, nil
				}
			}
			return empty, false, usage("unknown task ID %s; use sshore list", ref)
		}
		return empty, false, nil
	}
	target, err := findTaskIdentity(identity)
	if err != nil {
		return empty, false, err
	}
	if kind != "" && kind != target.kind && !(kind == "spawn" && target.kind == "shell") {
		return empty, false, usage("task ID %s is a %s", code, target.kind)
	}
	if parsed.has("--local") && (target.kind != "shell" || target.host != "") {
		return empty, false, usage("task ID %s is not a local shell", code)
	}
	if position == 1 && (target.kind != "shell" || target.host != parsed.operands[0]) {
		return empty, false, usage("task ID %s does not belong to host %s", code, parsed.operands[0])
	}
	if command == "attach" && target.kind != "shell" || command == "review" && target.kind != "review" {
		return empty, false, usage("%s does not accept %s tasks", command, target.kind)
	}
	if target.kind != "shell" {
		if parsed.has("--local") || len(parsed.ssh) != 0 || parsed.has("--server") {
			return empty, false, usage("local resources do not accept SSH options or --local")
		}
		return target, true, nil
	}
	if target.host == "" {
		parsed.values["--local"] = ""
		parsed.operands = append([]string{target.name}, parsed.operands[position+1:]...)
	} else {
		parsed.operands = append([]string{target.host, target.name}, parsed.operands[position+1:]...)
	}
	return target, true, nil
}

func findTaskIdentity(identity string) (selectedTask, error) {
	empty := selectedTask{}
	if strings.HasPrefix(identity, "local:") {
		dir, err := stateDir("local-shells")
		if err != nil {
			return empty, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return empty, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			r, err := readLocalSession(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return empty, err
			}
			if r.identity() == identity {
				return selectedTask{kind: "shell", name: r.Name, identity: identity}, nil
			}
		}
	} else if strings.HasPrefix(identity, "remote:") {
		snapshots, err := readShellSnapshots()
		if err != nil {
			return empty, err
		}
		var best *shellSnapshot
		for i := range snapshots {
			s := &snapshots[i]
			if s.Forgotten || s.State == "renamed" || remoteShellIdentity(s.Host, s.Instance) != identity {
				continue
			}
			if best == nil || endedShellState(best.State) && !endedShellState(s.State) || endedShellState(best.State) == endedShellState(s.State) && s.ConfirmedAt.After(best.ConfirmedAt) {
				best = s
			}
		}
		if best != nil {
			return selectedTask{kind: "shell", name: best.Name, host: best.Host, identity: identity, instance: best.Instance, connection: best.Connection}, nil
		}
	} else if strings.HasPrefix(identity, "mount:") || strings.HasPrefix(identity, "forward:") || strings.HasPrefix(identity, "review:") {
		records, err := collectLocalResources("")
		if err != nil {
			return empty, err
		}
		for _, r := range records {
			if resourceIdentity(r) == identity {
				target := selectedTask{kind: r.Kind, name: r.Name, identity: identity}
				if r.Kind == "review" {
					target.requestID = r.Token
				}
				return target, nil
			}
		}
	}
	if strings.HasPrefix(identity, "peer:") {
		return empty, usage("peer tasks are read-only here; cancel the resource on its managing computer")
	}
	return empty, fmt.Errorf("task is no longer available here; refresh with sshore list")
}
