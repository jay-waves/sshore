package main

import (
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/taskid"
)

// Kind limits the registry scan to a task type. Native and hosted are two
// ways to reach a shell. Detached queries list idle roots and direct children.
type taskQuery struct {
	Kind, Ref, UID, Parent string
	Detached               bool
	Recent, RemoteChildren bool
}

type selectedTask struct {
	detachedAt                     time.Time
	Kind, Name, Identity, Instance string
	Hosted                         bool
	RemoteHost                     string            `json:",omitempty"` // Host of a remote attach candidate without a saved direct route.
	Connection                     *recentConnection `json:",omitempty"`
	Manager                        string            `json:",omitempty"` // A visible task without a control route.
}

func (q taskQuery) candidate(uid string, task daemonTask) (selectedTask, bool) {
	candidate := selectedTask{Identity: taskid.Key(uid)}
	switch t := task.(type) {
	case *tmuxTask:
		s := t
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
		detached := candidates[:0]
		for _, candidate := range candidates {
			if q.detached(tasks, &candidate) {
				detached = append(detached, candidate)
			}
		}
		candidates = detached
	}
	return candidates
}

func (q taskQuery) detached(tasks *daemonTasks, candidate *selectedTask) bool {
	task, ok := tasks.lookup(taskid.ParseKey(candidate.Identity)).(*tmuxTask)
	if !ok {
		return false
	}
	s := task
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.parentIdentityLocked()
	candidate.detachedAt = s.runtime.DetachedAt()
	return s.state() == taskDetached && !s.killing && s.remoteOwner == "" && (owner == "" || owner == q.Parent)
}

func (q taskQuery) recentDetached(tasks *daemonTasks) []selectedTask {
	q.Detached = true
	rows := q.registered(tasks)
	slices.SortFunc(rows, func(a, b selectedTask) int {
		if order := b.detachedAt.Compare(a.detachedAt); order != 0 {
			return order
		}
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}
		return strings.Compare(a.Identity, b.Identity)
	})
	return rows
}

// IDs take precedence over aliases, including aliases that look like short hex
// prefixes. A live registration takes precedence over saved routes to that UID.
func matchTask(ref string, candidates []selectedTask) (selectedTask, error) {
	var idMatch, alias selectedTask
	prefix := strings.ToLower(ref)
	byID := taskid.IsPrefix(prefix)
	var ambiguousRoute, ambiguousAlias bool
	for _, target := range candidates {
		switch {
		case byID && strings.HasPrefix(taskid.ParseKey(target.Identity), prefix):
			if idMatch.Identity != "" && idMatch.Identity != target.Identity {
				return selectedTask{}, usage("ambiguous task ID %s; use a longer prefix", ref)
			}
			if idMatch.Identity == target.Identity && idMatch.RemoteHost != target.RemoteHost {
				return selectedTask{}, usage("task ID %s matches multiple accounts", ref)
			}
			if idMatch.Identity == target.Identity && target.Connection != nil {
				if saved, route := idMatch.Connection, target.Connection; saved != nil {
					ambiguousRoute = ambiguousRoute || saved.Host != route.Host ||
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
	if taskid.Valid(uid) {
		target, _ = q.candidate(uid, tasks.lookupLocked(uid))
	}
	if target.Identity == "" && q.UID == "" {
		target, err = matchTask(q.Ref, q.registeredLocked(tasks))
	}
	return target, err
}

func (q taskQuery) run(tasks *daemonTasks) ([]selectedTask, error) {
	if q.RemoteChildren {
		return tasks.remoteChildCandidates(q.Parent, q.Ref), nil
	}
	if q.Recent {
		rows := q.recentDetached(tasks)
		if len(rows) > 1 {
			rows = rows[:1]
		}
		return rows, nil
	}
	if q.Ref == "" && q.UID == "" {
		return q.registered(tasks), nil
	}
	tasks.mu.Lock()
	task, err := q.selectLocked(tasks)
	tasks.mu.Unlock()
	if err != nil || task.Identity == "" || q.Detached && !q.detached(tasks, &task) {
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

// Selection reads the published account view. Opening or stopping the chosen UID
// is a separate authenticated operation against its manager.
func selectCLITask(parsed cliArguments) (selectedTask, error) {
	ref := parsed.task.ref
	byID := ref != ""
	if !byID {
		ref = parsed.task.name
	}
	if ref == "" {
		return selectedTask{}, nil
	}
	attach := parsed.command == "attach"
	var parent string
	if attach {
		parent = shellIdentityFromEnvironment()
	}
	var children []selectedTask
	var err error
	if parent != "" {
		children, err = queryRegisteredTasks(taskQuery{RemoteChildren: true, Parent: parent, Ref: ref})
		if err != nil {
			return selectedTask{}, err
		}
	}
	candidates, err := publishedCandidates(children, attach, parent != "" && os.Getenv("SSHORE_SOCKET") != "", parsed.task.ref)
	if err != nil {
		return selectedTask{}, err
	}
	target, err := matchTask(ref, candidates)
	if err != nil {
		return selectedTask{}, err
	}
	// A name and an ID must resolve to the same unambiguous route.
	if !byID && target.Identity != "" {
		target, err = matchTask(taskid.ParseKey(target.Identity), candidates)
		if err != nil {
			return selectedTask{}, err
		}
	}
	if target.Connection != nil {
		if err := validateShellHistoryConnection(*target.Connection); err != nil {
			return selectedTask{}, err
		}
	}
	if target.Manager != "" {
		return selectedTask{}, usage("task %s is managed on %s; use an authenticated connection to that account", ref, cleanText(target.Manager))
	}
	if target.Identity == "" {
		if byID && attach {
			return selectedTask{}, usage("unknown shell ID %s; refresh with sshore list", ref)
		}
		if byID {
			return selectedTask{}, usage("unknown task %s; refresh with sshore list", ref)
		}
		if !attach {
			return selectedTask{}, usage("unknown task %s; use kill HOST --all for a remote account", cleanText(ref))
		}
	}
	return target, nil
}

func publishedCandidates(candidates []selectedTask, attach, hostedOnly bool, id string) ([]selectedTask, error) {
	root, instance, err := accountDatabaseLocation(time.Second)
	if err != nil {
		return nil, err
	}
	err = historyTransaction(root, false, func(tx *bolt.Tx) error {
		inv := emptyInventory()
		bucket := currentLiveBucket(tx, instance)
		if bucket != nil {
			switch {
			case taskid.Valid(id):
				key := []byte(id)
				if data := bucket.Get(key); data != nil {
					if err := inv.readLiveRow(key, data); err != nil {
						return err
					}
				}
			case id != "":
				if err := inv.readPrefix(bucket, []byte(id)); err != nil {
					return err
				}
			default:
				if err := bucket.ForEach(inv.readLiveRow); err != nil {
					return err
				}
			}
		}
		before := len(candidates)
		for _, s := range inv.Shells {
			if taskHiddenFromInventory(s.State) || hostedOnly && !s.Hosted {
				continue
			}
			t := selectedTask{Kind: "shell", Name: s.Name, Identity: taskid.Key(s.Instance), Hosted: s.Hosted}
			if t.Hosted {
				t.Instance = s.Instance
			}
			candidates = append(candidates, t)
		}
		if !attach {
			for _, r := range inv.Resources {
				kind, name := r.kindAndName()
				candidates = append(candidates, selectedTask{Kind: kind, Name: name, Identity: r.Identity})
			}
		}
		if taskid.Valid(id) && len(candidates) > before {
			return nil
		}
		known := make(map[string]bool)
		for _, t := range candidates {
			known[t.Identity] = true
		}
		observed, err := participatingShells(tx)
		if err != nil {
			return err
		}
		var advisory []selectedTask
		for _, s := range observed {
			id := taskid.Key(s.Instance)
			if known[id] {
				continue
			}
			t := selectedTask{Kind: "shell", Name: s.Name, Identity: id, Instance: s.Instance}
			if route := s.Route; route != nil && s.Owner == "" {
				t.Connection = &recentConnection{Host: s.Host, Instance: s.Instance, Session: s.Name, Directory: route.Directory, SSH: route.SSH}
				candidates = append(candidates, t)
				continue
			}
			if attach {
				t.RemoteHost = s.Host
			} else {
				t.Manager = s.Host
			}
			advisory = append(advisory, t)
		}
		for _, t := range candidates {
			known[t.Identity] = true
		}
		if !attach && id != "" && bucket != nil {
			if err := inv.readPrefix(bucket, []byte("peer/")); err != nil {
				return err
			}
			for _, p := range inv.Peers {
				id := taskid.Key(p.UID)
				if p.Manager != "" {
					advisory = append(advisory, selectedTask{Kind: peerKind(p.ID), Identity: id, Manager: p.Manager})
				}
			}
		}
		for _, t := range advisory {
			if !known[t.Identity] {
				candidates = append(candidates, t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
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

func killRegisteredTask(target selectedTask) (int, error) {
	uid := taskid.ParseKey(target.Identity)
	if uid == "" {
		return 1, fmt.Errorf("invalid task identity")
	}
	if _, err := daemonResourceRequest(context.Background(), "/tasks/stop", struct{ UID string }{uid}); err != nil {
		return 1, err
	}
	if target.Kind != "shell" {
		printStoppedResource(target.Kind, target.Name, target.Identity)
	} else if target.Hosted {
		fmt.Fprintf(os.Stdout, "Ended shell %s\n", target.Name)
	}
	return 0, nil
}

func (tasks *daemonTasks) remoteChildCandidates(parent, ref string) []selectedTask {
	if parent == "" {
		return nil
	}
	source, ok := tasks.lookup(taskid.ParseKey(parent)).(*tmuxTask)
	if !ok {
		return nil
	}
	s := source
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []selectedTask
	byID := taskid.IsPrefix(ref)
	for child := range s.remoteChildren {
		if byID && strings.HasPrefix(child.uid, ref) || !byID && child.name == ref {
			candidates = append(candidates, selectedTask{
				Kind: "shell", Name: child.name, Identity: taskid.Key(child.uid), Instance: child.uid, RemoteHost: child.host,
			})
		}
	}
	return candidates
}

func (s *tmuxTask) selectRemoteChild(config *shellConfig) (*shellTarget, error) {
	if config.Name == "" && config.Instance == "" {
		return nil, fmt.Errorf("select a child with sshore attach ID")
	}
	s.mu.Lock()
	var children []*shellTarget
	for child := range s.remoteChildren {
		if child.host == config.Host && (config.Instance == "" || child.uid == config.Instance) {
			children = append(children, child)
		}
	}
	s.mu.Unlock()

	// Status and ownership come from the child's authenticated endpoint.
	type liveChild struct {
		child *shellTarget
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
			id := taskid.Key(child.uid)
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
		return nil, fmt.Errorf("child shell %s is %s; detach it before attaching", taskid.ShortObservation(target.Identity), cleanText(match.state))
	}
	return match.child, nil
}

func attachmentChoices(candidates []selectedTask) ([]sessionTarget, error) {
	if len(candidates) > 128 {
		return nil, fmt.Errorf("too many shells to list; specify a name or ID")
	}
	targets := make([]sessionTarget, 0, len(candidates))
	for _, t := range candidates {
		targets = append(targets, sessionTarget{Name: t.Name, Instance: taskid.ParseKey(t.Identity), Hosted: t.Hosted})
	}
	return targets, nil
}

func (s *tmuxTask) sessionTargets(request message) message {
	reply := message{Type: "session_targets", Action: request.Action, Name: s.taskName()}
	list := strings.HasPrefix(request.Action, consoleTargetLookup)
	parentLookup := strings.HasPrefix(request.Action, consoleParentLookup)
	if list {
		s.mu.Lock()
		reply.Directory = s.directoryOrStartupLocked()
		s.mu.Unlock()
	}
	query := taskQuery{Kind: "shell", Parent: s.treeIdentity(), Ref: request.Name, Detached: !parentLookup}
	if s.hosted && !request.NativeTargets {
		query.Kind = "hosted"
	}
	var err error
	reply.Targets, err = s.registry.attachmentTargets(query, list)
	if err == nil && query.Detached && query.Ref != "" && len(reply.Targets) == 0 {
		err = missingAttachTarget(query.Ref)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}

func (tasks *daemonTasks) attachmentTargets(query taskQuery, list bool) ([]sessionTarget, error) {
	if query.Ref == "" && query.Detached {
		rows := query.recentDetached(tasks)
		if !list && len(rows) > 1 {
			rows = rows[:1]
		}
		return attachmentChoices(rows)
	}
	rows, err := query.run(tasks)
	if err != nil {
		return nil, err
	}
	return attachmentChoices(rows)
}

func missingAttachTarget(ref string) error {
	return fmt.Errorf("no detached child or idle root matches %q on this host", cleanText(ref))
}
