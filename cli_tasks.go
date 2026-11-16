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
		if compact && !typed && !parsed.has("--local") && !parsed.has("--all") {
			target, found, err := findTaskAlias(command, ref)
			if err != nil || !found {
				return empty, false, err
			}
			return routeSelectedTask(command, parsed, target, 0)
		}
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
	matched := false
	for key, value := range index.Codes {
		if value == strings.ToLower(code) {
			matched = true
			if strings.HasPrefix(key, "peer-task:") && validTaskUID(strings.TrimPrefix(key, "peer-task:")) {
				if _, err := findTaskIdentity("task:" + strings.TrimPrefix(key, "peer-task:")); err == nil {
					continue
				}
			}
			if typed || parsed.has("--local") || position == 1 {
				var hosts []string
				if position == 1 {
					hosts = []string{parsed.operands[0]}
				}
				candidate, err := findTaskIdentity(key, hosts...)
				if err != nil {
					continue
				}
				if typed && kind != candidate.kind && !((kind == "spawn" || kind == "launch") && candidate.kind == "shell") {
					continue
				}
				if parsed.has("--local") && (candidate.kind != "shell" || candidate.host != "") {
					continue
				}
				if position == 1 && (candidate.kind != "shell" || candidate.host != parsed.operands[0]) {
					continue
				}
			}
			if identity != "" {
				return empty, false, fmt.Errorf("ambiguous task ID %s; specify HOST, --local or a type prefix", code)
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
		if matched {
			return empty, false, usage("task ID %s does not belong to the requested scope", code)
		}
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
	var hosts []string
	if position == 1 {
		hosts = []string{parsed.operands[0]}
	}
	target, err := findTaskIdentity(identity, hosts...)
	if err != nil {
		return empty, false, err
	}
	if kind != "" && kind != target.kind && !((kind == "spawn" || kind == "launch") && target.kind == "shell") {
		return empty, false, usage("task ID %s is a %s", code, target.kind)
	}
	if parsed.has("--local") && (target.kind != "shell" || target.host != "") {
		return empty, false, usage("task ID %s is not a local shell", code)
	}
	if position == 1 && (target.kind != "shell" || target.host != parsed.operands[0]) {
		return empty, false, usage("task ID %s does not belong to host %s", code, parsed.operands[0])
	}
	return routeSelectedTask(command, parsed, target, position)
}

func routeSelectedTask(command string, parsed *cliArguments, target selectedTask, position int) (selectedTask, bool, error) {
	empty := selectedTask{}
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

func findTaskIdentity(identity string, hosts ...string) (selectedTask, error) {
	empty := selectedTask{}
	if strings.HasPrefix(identity, "local:") || strings.HasPrefix(identity, "task:") {
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
	}
	if strings.HasPrefix(identity, "remote:") || strings.HasPrefix(identity, "task:") {
		snapshots, err := readShellSnapshots()
		if err != nil {
			return empty, err
		}
		var best *shellSnapshot
		for i := range snapshots {
			s := &snapshots[i]
			if s.Forgotten || s.State == "renamed" || remoteShellIdentity(s.Host, s.Instance) != identity || len(hosts) > 0 && s.Host != hosts[0] {
				continue
			}
			if best == nil || endedShellState(best.State) && !endedShellState(s.State) || endedShellState(best.State) == endedShellState(s.State) && s.ConfirmedAt.After(best.ConfirmedAt) {
				best = s
			}
		}
		if best != nil {
			return selectedTask{kind: "shell", name: best.Name, host: best.Host, identity: identity, instance: best.Instance, connection: best.Connection}, nil
		}
	}
	if strings.HasPrefix(identity, "task:") || strings.HasPrefix(identity, "mount:") || strings.HasPrefix(identity, "forward:") || strings.HasPrefix(identity, "review:") {
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
	if strings.HasPrefix(identity, "peer:") || strings.HasPrefix(identity, "peer-task:") {
		return empty, usage("peer tasks are read-only here; cancel the resource on its managing computer")
	}
	return empty, fmt.Errorf("task is no longer available here; refresh with sshore list")
}

func findTaskAlias(command, alias string) (selectedTask, bool, error) {
	var matches []selectedTask
	add := func(target selectedTask) {
		if command == "attach" && target.kind != "shell" || command == "review" && target.kind != "review" {
			return
		}
		for _, old := range matches {
			if old.identity == target.identity {
				return
			}
		}
		matches = append(matches, target)
	}
	if command != "review" {
		dir, err := stateDir("local-shells")
		if err != nil {
			return selectedTask{}, false, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return selectedTask{}, false, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			r, err := readLocalRecord(dir+string(os.PathSeparator)+entry.Name(), strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return selectedTask{}, false, err
			}
			if r.Name == alias {
				add(selectedTask{kind: "shell", name: r.Name, identity: r.identity()})
			}
		}
		snapshots, err := readShellSnapshots()
		if err != nil {
			return selectedTask{}, false, err
		}
		for _, r := range snapshots {
			if r.Name == alias && !r.Forgotten && !endedShellState(r.State) {
				add(selectedTask{kind: "shell", name: r.Name, host: r.Host, identity: remoteShellIdentity(r.Host, r.Instance), instance: r.Instance, connection: r.Connection})
			}
		}
	}
	if command != "attach" {
		records, err := collectLocalResources("")
		if err != nil {
			return selectedTask{}, false, err
		}
		for _, r := range records {
			if r.Name == alias {
				add(selectedTask{kind: r.Kind, name: r.Name, identity: resourceIdentity(r), requestID: r.Token})
			}
		}
	}
	if len(matches) > 1 {
		return selectedTask{}, false, usage("ambiguous alias %s; use a task ID or explicit scope", alias)
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return selectedTask{}, false, nil
}
