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
	RemoteHost                     string // Host of a remote attach candidate without a saved direct route.
	Connection                     *recentConnection
	Manager                        string // A visible task without a control route.
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
	root, instance, err := accountDatabaseLocation(time.Second)
	if err != nil || instance == "" {
		return nil, err
	}
	return q.published(root, instance)
}

func (q taskQuery) published(root, instance string) ([]selectedTask, error) {
	var inv inventory
	err := historyTransaction(root, false, func(tx *bolt.Tx) error {
		return inv.readTaskRows(currentLiveBucket(tx, instance), q.UID)
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
			attachable: shell.State == taskDetached && !shell.Stopping &&
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
	hostedOnly := attach && validTaskUID(os.Getenv("SSHORE_SOCKET"))
	candidates, err := publishedCandidates(parsed.task.ref, attach, hostedOnly)
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

func publishedCandidates(id string, attach, hostedOnly bool) ([]selectedTask, error) {
	var candidates []selectedTask
	root, instance, err := accountDatabaseLocation(time.Second)
	if err != nil {
		return nil, err
	}
	err = historyTransaction(root, false, func(tx *bolt.Tx) error {
		inv := emptyInventory()
		bucket := currentLiveBucket(tx, instance)
		if err := inv.readTaskRows(bucket, id); err != nil {
			return err
		}
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
		if validTaskUID(id) && len(candidates) > 0 {
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

func (s *shellTask) sessionTargets(request message) message {
	reply := message{Type: "session_targets", Action: request.Action, Name: s.taskName()}
	list := strings.HasPrefix(request.Action, consoleTargetLookup)
	parentLookup := strings.HasPrefix(request.Action, consoleParentLookup)
	if list {
		reply.Directory = s.session.WorkingDirectory()
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
