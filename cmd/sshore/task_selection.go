package main

import (
	"cmp"
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"slices"
	"strings"
	"time"
)

// Kind limits the published account view to a task type.
type taskQuery struct {
	Kind, Ref, UID, Parent string
	Detached, Recent       bool
}

type selectedTask struct {
	detachedAt                     time.Time
	attachable                     bool
	Kind, Name, Identity, Instance string
	Hosted                         bool
	RemoteHost                     string            `json:",omitempty"` // Host of a remote attach candidate without a saved direct route.
	Connection                     *recentConnection `json:",omitempty"`
	Manager                        string            `json:",omitempty"` // A visible task without a control route.
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

// Resolve ordinary selectors from bbolt. The account daemon rechecks the full
// ID when the caller opens a stream or changes a task.
func queryRegisteredTasks(q taskQuery) ([]selectedTask, error) {
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return q.published(record)
}

func (q taskQuery) published(record localShellRecord) ([]selectedTask, error) {
	var inv inventory
	err := historyTransaction(record.Directory, false, func(tx *bolt.Tx) error {
		bucket := currentLiveBucket(tx, record.UID)
		if bucket == nil {
			return nil
		}
		if q.UID != "" {
			key := []byte(q.UID)
			if data := bucket.Get(key); data != nil {
				return inv.readLiveRow(key, data)
			}
			return nil
		}
		return bucket.ForEach(func(key, data []byte) error {
			if validTaskUID(string(key)) {
				return inv.readLiveRow(key, data)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return q.selectPublished(inv)
}

func (q taskQuery) selectPublished(inv inventory) ([]selectedTask, error) {
	var rows []selectedTask
	for _, shell := range inv.Shells {
		switch q.Kind {
		case "", "shell":
		case "native":
			if shell.Hosted {
				continue
			}
		case "hosted":
			if !shell.Hosted {
				continue
			}
		default:
			continue
		}
		if shell.State == taskExited {
			continue
		}
		row := selectedTask{Kind: "shell", Name: shell.Name, Identity: taskKey(shell.Instance),
			Hosted: shell.Hosted, detachedAt: shell.DetachedAt,
			attachable: shell.State == taskDetached && !shell.Stopping && !shell.RemoteOwner &&
				(shell.Owner == "" || shell.Owner == q.Parent)}
		if shell.Hosted {
			row.Instance = shell.Instance
		}
		rows = append(rows, row)
	}
	for _, resource := range inv.Resources {
		kind, name := resource.kindAndName()
		if q.Kind == "" || q.Kind == kind {
			rows = append(rows, selectedTask{Kind: kind, Name: name, Identity: resource.Identity})
		}
	}

	// Resolve an exact ID before checking availability. A busy session never
	// turns a reference to its ID into a different session's name.
	if !q.Recent && (q.UID != "" || q.Ref != "") {
		var row selectedTask
		uid := cmp.Or(q.UID, strings.ToLower(q.Ref))
		if validTaskUID(uid) {
			for _, candidate := range rows {
				if canonicalTaskUID(candidate.Identity) == uid {
					row = candidate
					break
				}
			}
		}
		if row.Identity == "" && q.UID == "" {
			var err error
			row, err = matchTask(q.Ref, rows)
			if err != nil {
				return nil, err
			}
		}
		if row.Identity == "" || q.Detached && !row.attachable {
			return nil, nil
		}
		return []selectedTask{row}, nil
	}
	if q.Detached || q.Recent {
		rows = slices.DeleteFunc(rows, func(row selectedTask) bool { return !row.attachable })
		slices.SortFunc(rows, func(a, b selectedTask) int {
			return cmp.Or(b.detachedAt.Compare(a.detachedAt), strings.Compare(a.Name, b.Name), strings.Compare(a.Identity, b.Identity))
		})
		if q.Recent && len(rows) > 1 {
			rows = rows[:1]
		}
	}
	return rows, nil
}

type remoteChildQuery struct{ Parent, Ref string }

func queryRemoteChildren(parent, ref string) ([]selectedTask, error) {
	if activity := accountActivity.Load(); activity != nil && activity.tasks != nil {
		return activity.tasks.remoteChildCandidates(parent, ref), nil
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
	err = daemonJSON(ctx, record, "/tasks/remote-children", remoteChildQuery{parent, ref}, &result)
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
		children, err = queryRemoteChildren(parent, ref)
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
		target, err = matchTask(canonicalTaskUID(target.Identity), candidates)
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
			case validTaskUID(id):
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
			t := selectedTask{Kind: "shell", Name: s.Name, Identity: taskKey(s.Instance), Hosted: s.Hosted}
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
		if validTaskUID(id) && len(candidates) > before {
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
			id := taskKey(s.Instance)
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
				id := taskKey(p.UID)
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
	uid := canonicalTaskUID(target.Identity)
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
	s := tasks.shell(canonicalTaskUID(parent))
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []selectedTask
	byID := isTaskIDPrefix(ref)
	for child := range s.remoteChildren {
		if byID && strings.HasPrefix(child.uid, ref) || !byID && child.name == ref {
			candidates = append(candidates, selectedTask{
				Kind: "shell", Name: child.name, Identity: taskKey(child.uid), Instance: child.uid, RemoteHost: child.host,
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
			id := taskKey(child.uid)
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
		return nil, fmt.Errorf("child shell %s is %s; detach it before attaching", shortTaskObservation(target.Identity), cleanText(match.state))
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
	query.Recent = query.Ref == "" && query.Detached && !list
	rows, err := queryRegisteredTasks(query)
	if err == nil {
		reply.Targets, err = attachmentChoices(rows)
	}
	if err == nil && query.Detached && query.Ref != "" && len(reply.Targets) == 0 {
		err = missingAttachTarget(query.Ref)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}

func missingAttachTarget(ref string) error {
	return fmt.Errorf("no detached child or idle root matches %q on this host", cleanText(ref))
}
