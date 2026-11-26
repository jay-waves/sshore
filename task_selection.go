package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// Kind limits the registry scan to a task type. Native and hosted are two
// ways to reach a shell. Detached queries list idle roots and direct children.
type taskQuery struct {
	Kind, Ref, UID, Parent string
	Detached               bool
}

type selectedTask struct {
	Kind, Name, Identity, Instance string
	Hosted                         bool
	RemoteHost                     string            `json:",omitempty"` // Host of a remote attach candidate without a saved direct route.
	Connection                     *recentConnection `json:",omitempty"`
	Manager                        string            `json:",omitempty"` // A visible task without a control route.
}

func (q taskQuery) candidate(uid string, task daemonTask) (selectedTask, bool) {
	candidate := selectedTask{Identity: identityFromUID(uid)}
	switch t := task.(type) {
	case interface{ managedShell() *shellTask }:
		s := t.managedShell()
		switch q.Kind {
		case "", "shell":
		case "native":
			if s.hosted {
				return selectedTask{}, false
			}
		case "hosted":
			if !s.hosted {
				return selectedTask{}, false
			}
		default:
			return selectedTask{}, false
		}
		candidate.Kind, candidate.Name, candidate.Hosted = "shell", s.taskName(), s.hosted
		if s.hosted {
			candidate.Instance = uid
		}
	case *daemonResourceTask:
		t.mu.Lock()
		defer t.mu.Unlock()
		if q.Kind != "" && q.Kind != t.record.Kind {
			return selectedTask{}, false
		}
		candidate.Kind, candidate.Name = t.record.Kind, t.record.Name
	default:
		return selectedTask{}, false
	}
	return candidate, true
}

func (q taskQuery) registeredLocked(tasks *daemonTasks) []selectedTask {
	var candidates []selectedTask
	for uid, task := range tasks.liveLocked() {
		if target, ok := q.candidate(uid, task); ok {
			candidates = append(candidates, target)
		}
	}
	return candidates
}

func (q taskQuery) registered(tasks *daemonTasks) []selectedTask {
	tasks.mu.Lock()
	candidates := q.registeredLocked(tasks)
	tasks.mu.Unlock()
	if q.Detached {
		candidates = slices.DeleteFunc(candidates, func(candidate selectedTask) bool {
			return !q.detached(tasks, candidate)
		})
	}
	return candidates
}

func (q taskQuery) detached(tasks *daemonTasks, candidate selectedTask) bool {
	task, ok := tasks.lookup(canonicalTaskUID(candidate.Identity)).(interface{ managedShell() *shellTask })
	if !ok {
		return false
	}
	s := task.managedShell()
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.parentIdentityLocked()
	return s.state() == taskDetached && !s.killing && s.remoteOwner == "" && (owner == "" || owner == q.Parent)
}

// IDs take precedence over aliases, including aliases that look like short hex
// prefixes. A live registration takes precedence over saved routes to that UID.
func matchTask(ref string, candidates []selectedTask) (selectedTask, error) {
	var idMatch, alias selectedTask
	prefix := strings.ToLower(ref)
	byID := isTaskIDPrefix(prefix)
	var ambiguousRoute, ambiguousAlias bool
	for _, target := range candidates {
		switch {
		case byID && strings.HasPrefix(canonicalTaskUID(target.Identity), prefix):
			if idMatch.Identity != "" && idMatch.Identity != target.Identity {
				return selectedTask{}, usage("ambiguous task ID %s; use a longer prefix", ref)
			}
			if idMatch.Identity == target.Identity && idMatch.RemoteHost != target.RemoteHost {
				return selectedTask{}, usage("task ID %s matches multiple accounts", ref)
			}
			if idMatch.Identity == target.Identity && target.Connection != nil {
				if saved, route := idMatch.Connection, target.Connection; saved != nil {
					ambiguousRoute = ambiguousRoute || saved.Host != route.Host || saved.Server != route.Server ||
						saved.Directory != route.Directory || !slices.Equal(saved.SSH, route.SSH)
				}
				continue
			}
			idMatch = target
		case target.Name == ref:
			ambiguousAlias = ambiguousAlias || alias.Identity != "" && alias.Identity != target.Identity
			alias = target
		}
	}
	if ambiguousRoute {
		return selectedTask{}, usage("multiple saved connections for task %s", idMatch.Instance)
	}
	if idMatch.Identity != "" {
		return idMatch, nil
	}
	if ambiguousAlias {
		return selectedTask{}, usage("ambiguous task name %s; use a task ID", cleanText(ref))
	}
	return alias, nil
}

// An explicit UID is binding even if its previous label now belongs to another task.
func (q taskQuery) selectLocked(tasks *daemonTasks) (target selectedTask, err error) {
	// Resolve the reference before testing availability so a busy shell's ID
	// cannot silently become a different shell's alias.
	uid := q.UID
	if uid == "" {
		uid = strings.ToLower(q.Ref)
	}
	if validTaskUID(uid) {
		target, _ = q.candidate(uid, tasks.lookupLocked(uid))
	}
	if target.Identity == "" && q.UID == "" {
		target, err = matchTask(q.Ref, q.registeredLocked(tasks))
	}
	return target, err
}

func (q taskQuery) run(tasks *daemonTasks) ([]selectedTask, error) {
	if q.Ref == "" && q.UID == "" {
		return q.registered(tasks), nil
	}
	tasks.mu.Lock()
	task, err := q.selectLocked(tasks)
	tasks.mu.Unlock()
	if err != nil || task.Identity == "" || q.Detached && !q.detached(tasks, task) {
		return nil, err
	}
	return []selectedTask{task}, nil
}

func queryRegisteredTasks(q taskQuery) ([]selectedTask, error) {
	if activity := accountActivity.Load(); activity != nil && activity.tasks != nil {
		return q.run(activity.tasks)
	}
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var result []selectedTask
	err = daemonJSON(ctx, record, "/tasks/select", q, &result)
	return result, err
}

// Resolve one ID across the registry, saved routes and visible peer records.
// Only a registered task or a saved authenticated route can accept control.
func (a *daemonActivity) selectTaskID(ref, kind, parent string, attach bool) (*selectedTask, error) {
	prefix := strings.ToLower(ref)
	if !isTaskIDPrefix(prefix) {
		return nil, usage("invalid task ID")
	}
	query := taskQuery{Kind: kind}
	if validTaskUID(prefix) {
		if target, ok := query.candidate(prefix, a.tasks.lookup(prefix)); ok {
			return &target, nil
		}
	}
	candidates := query.registered(a.tasks)
	if attach && parent != "" {
		if source, ok := a.tasks.lookup(canonicalTaskUID(parent)).(interface{ managedShell() *shellTask }); ok {
			candidates = append(candidates, source.managedShell().remoteChildIDs(prefix)...)
		}
	}
	live := make(map[string]bool)
	for _, target := range candidates {
		live[target.Identity] = true
	}
	a.metadata.shellMu.Lock()
	defer a.metadata.shellMu.Unlock()
	records, err := a.metadata.readRecentConnectionsLocal()
	if err != nil {
		return nil, err
	}
	if err := a.metadata.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	observed, err := a.metadata.readParticipatingShellsLocal("")
	if err != nil {
		return nil, err
	}
	addRoute := func(route *recentConnection) {
		if route != nil && !route.viaShell() && strings.HasPrefix(route.Instance, prefix) && (!attach || !live[identityFromUID(route.Instance)]) {
			candidates = append(candidates, selectedTask{Kind: "shell", Identity: identityFromUID(route.Instance), Instance: route.Instance, Connection: route})
		}
	}
	for i := range records {
		addRoute(&records[i])
	}
	for _, row := range a.metadata.attachments.rows {
		addRoute(row.Connection)
	}
	known := make(map[string]bool, len(candidates))
	for _, target := range candidates {
		known[target.Identity] = true
	}
	var advisory []selectedTask
	for _, row := range observed {
		if !strings.HasPrefix(row.Instance, prefix) || known[identityFromUID(row.Instance)] {
			continue
		}
		i := slices.IndexFunc(records, func(route recentConnection) bool { return route.Host == row.Host })
		if attach && (i < 0 || records[i].viaShell()) {
			candidates = append(candidates, selectedTask{Kind: "shell", Identity: identityFromUID(row.Instance), Instance: row.Instance, RemoteHost: row.Host})
			continue
		}
		if i < 0 {
			advisory = append(advisory, selectedTask{Kind: "shell", Identity: identityFromUID(row.Instance), Manager: row.Host})
			continue
		}
		route := records[i]
		route.Instance, route.Session = row.Instance, row.Name
		addRoute(&route)
	}
	if kind == "" {
		for _, peer := range a.incomingPeers() {
			if strings.HasPrefix(peer.UID, prefix) {
				advisory = append(advisory, selectedTask{Kind: peerKind(peer.ID), Identity: identityFromUID(peer.UID), Manager: peer.Manager})
			}
		}
	}
	for _, target := range candidates {
		known[target.Identity] = true
	}
	for _, target := range advisory {
		if !known[target.Identity] {
			candidates = append(candidates, target)
			known[target.Identity] = true
		}
	}
	target, err := matchTask(ref, candidates)
	if err != nil {
		return nil, err
	}
	if target.Identity == "" && attach {
		return nil, usage("unknown shell ID %s; refresh with sshore list", ref)
	}
	if target.Identity == "" {
		return nil, usage("unknown task %s; refresh with sshore list", ref)
	}
	if target.Manager != "" {
		return nil, usage("task %s is managed on %s; use an authenticated connection to that account", ref, cleanText(target.Manager))
	}
	return &target, nil
}

// Resolve names from account-owned registrations and already known routes only.
func (a *daemonActivity) selectTaskName(name, kind string, attach bool) (*selectedTask, error) {
	candidates := (taskQuery{Kind: kind}).registered(a.tasks)
	a.metadata.shellMu.Lock()
	defer a.metadata.shellMu.Unlock()
	routes, err := a.metadata.readRecentConnectionsLocal()
	if err != nil {
		return nil, err
	}
	if err := a.metadata.loadDetachedAttachments(); err != nil {
		return nil, err
	}
	observed, err := a.metadata.readParticipatingShellsLocal("")
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool)
	for _, target := range candidates {
		known[target.Identity] = true
	}
	addRoute := func(route *recentConnection) {
		if route == nil || route.viaShell() || route.Session != name || !validTaskUID(route.Instance) {
			return
		}
		id := identityFromUID(route.Instance)
		if !known[id] {
			candidates = append(candidates, selectedTask{Kind: "shell", Name: name, Identity: id, Instance: route.Instance, Connection: route})
			known[id] = true
		}
	}
	for i := range routes {
		addRoute(&routes[i])
	}
	for _, row := range a.metadata.attachments.rows {
		addRoute(row.Connection)
	}
	for _, row := range observed {
		id := identityFromUID(row.Instance)
		if row.Name != name || !validTaskUID(row.Instance) || known[id] {
			continue
		}
		i := slices.IndexFunc(routes, func(route recentConnection) bool { return route.Host == row.Host && !route.viaShell() })
		if i >= 0 {
			route := routes[i]
			route.Instance, route.Session = row.Instance, row.Name
			addRoute(&route)
		} else if attach {
			candidates = append(candidates, selectedTask{Kind: "shell", Name: name, Identity: id, Instance: row.Instance, RemoteHost: row.Host})
			known[id] = true
		} else {
			candidates = append(candidates, selectedTask{Kind: "shell", Name: name, Identity: id, Instance: row.Instance, Manager: row.Host})
			known[id] = true
		}
	}
	target, err := matchTask(name, candidates)
	if err != nil || target.Identity == "" {
		return nil, err
	}
	if target.Manager != "" {
		return nil, usage("task %s is managed on %s; use its managing account", name, cleanText(target.Manager))
	}
	return &target, nil
}

func selectCLITask(parsed cliArguments) (selectedTask, error) {
	command, target := parsed.command, parsed.task
	if target.name != "" {
		action, kind := "task_by_name", ""
		if command == "attach" {
			action, kind = "attach_by_name", "shell"
			if shellIdentityFromEnvironment() != "" && os.Getenv("SSHORE_SOCKET") != "" {
				kind = "hosted"
			}
		}
		reply, err := metadataRPC(metadataRequest{Action: action, Name: target.name, Kind: kind})
		if err != nil {
			return selectedTask{}, err
		}
		if reply.Target != nil {
			return *reply.Target, nil
		}
		if command == "kill" {
			return selectedTask{}, usage("unknown task %s; use kill HOST --all for a remote account", cleanText(target.name))
		}
		return selectedTask{}, nil
	}
	if target.ref == "" {
		return selectedTask{}, nil
	}
	parent := shellIdentityFromEnvironment()
	action, kind := "task_by_id", ""
	if command == "attach" {
		action, kind = "attach_by_id", "shell"
		if parent != "" && os.Getenv("SSHORE_SOCKET") != "" {
			kind = "hosted"
		}
	}
	reply, err := metadataRPC(metadataRequest{Action: action, Kind: kind, Instance: target.ref, Parent: parent})
	if err != nil {
		return selectedTask{}, err
	}
	if reply.Target == nil {
		return selectedTask{}, fmt.Errorf("missing task selection")
	}
	return *reply.Target, nil
}

func selectRegisteredTask(query taskQuery) (selectedTask, error) {
	rows, err := queryRegisteredTasks(query)
	if err != nil {
		return selectedTask{}, err
	}
	if len(rows) == 0 {
		return selectedTask{}, usage("unknown task %s; refresh with sshore list", cleanText(query.Ref))
	}
	return rows[0], nil
}

func mutateRegisteredTask(command string, target selectedTask) (int, error) {
	if target.Kind == "shell" {
		return runLocalCommand(options{command: command, session: target.Name,
			hosted: target.Hosted, taskIdentity: target.Identity, instance: target.Instance})
	}
	change := resourceMutation{Kind: target.Kind, Ref: canonicalTaskUID(target.Identity), Identity: target.Identity}
	if _, err := daemonResourceRequest(context.Background(), "/resources/"+command, change); err != nil {
		return 1, err
	}
	printStoppedResource(target.Kind, target.Name, target.Identity)
	return 0, nil
}

func (s *shellTask) remoteChildIDs(prefix string) []selectedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []selectedTask
	for child := range s.remoteChildren {
		if strings.HasPrefix(child.uid, prefix) {
			candidates = append(candidates, selectedTask{Kind: "shell", Identity: identityFromUID(child.uid), Instance: child.uid, RemoteHost: child.host})
		}
	}
	return candidates
}

func (s *shellTask) selectRemoteChild(config *shellConfig) (*remoteShellChild, error) {
	if config.Name == "" && config.Instance == "" {
		return nil, fmt.Errorf("select a child with sshore attach ID")
	}
	s.mu.Lock()
	var children []*remoteShellChild
	for child := range s.remoteChildren {
		if child.host == config.Host && (config.Instance == "" || child.uid == config.Instance) {
			children = append(children, child)
		}
	}
	s.mu.Unlock()

	// Status and ownership come from the child's authenticated endpoint.
	type liveChild struct {
		child *remoteShellChild
		state string
	}
	live := make(map[string]liveChild)
	var candidates []selectedTask
	var lookupErr error
	for _, child := range children {
		status, err := child.status()
		if err != nil {
			lookupErr = fmt.Errorf("could not check child %s on %s: %w", child.uid[:6], cleanText(child.host), err)
			continue
		}
		if status.Owner == s.treeIdentity() {
			id := identityFromUID(child.uid)
			live[id] = liveChild{child, status.Action}
			candidates = append(candidates, selectedTask{Identity: id, Name: status.Name})
		}
	}
	ref := config.Name
	if config.Instance != "" {
		ref = config.Instance
	}
	target, err := matchTask(ref, candidates)
	if err != nil {
		return nil, err
	}
	match, ok := live[target.Identity]
	if !ok {
		if lookupErr != nil {
			return nil, lookupErr
		}
		return nil, fmt.Errorf("no live child matches %s on %s; enter it from the shell that opened it", cleanText(ref), cleanText(config.Host))
	}
	if match.state != taskDetached {
		return nil, fmt.Errorf("child shell %s is %s; detach it before attaching", taskShortCode(target.Identity), cleanText(match.state))
	}
	return match.child, nil
}

func attachmentChoices(candidates []selectedTask) ([]sessionTarget, error) {
	if len(candidates) > 128 {
		return nil, fmt.Errorf("too many shells to list; specify a name or ID")
	}
	targets := make([]sessionTarget, 0, len(candidates))
	for _, t := range candidates {
		targets = append(targets, sessionTarget{Name: t.Name, Instance: canonicalTaskUID(t.Identity), Hosted: t.Hosted})
	}
	slices.SortFunc(targets, func(a, b sessionTarget) int { return strings.Compare(a.Name, b.Name) })
	return targets, nil
}

func (s *shellTask) sessionTargets(request message) message {
	reply := message{Type: "session_targets", Action: request.Action, Name: s.taskName()}
	if request.Name == "" && strings.HasPrefix(request.Action, "console:") {
		s.mu.Lock()
		reply.Directory = s.directoryOrStartupLocked()
		s.mu.Unlock()
	}
	var err error
	reply.Targets, err = s.registry.attachmentTargets(s.treeIdentity(), request.Name, s.hosted && !request.NativeTargets)
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}

func (tasks *daemonTasks) attachmentTargets(parent, ref string, hosted bool) ([]sessionTarget, error) {
	query := taskQuery{Kind: "shell", Parent: parent, Ref: ref, Detached: true}
	if hosted {
		query.Kind = "hosted"
	}
	rows, err := query.run(tasks)
	if err != nil {
		return nil, err
	}
	if ref != "" && len(rows) == 0 {
		return nil, missingAttachTarget(ref)
	}
	return attachmentChoices(rows)
}

func missingAttachTarget(ref string) error {
	return fmt.Errorf("no detached child or idle root matches %q on this host", cleanText(ref))
}
