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
	Connection                     *recentConnection `json:",omitempty"`
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
		s.mu.Lock()
		defer s.mu.Unlock()
		if q.Detached {
			owner := s.parentIdentityLocked()
			if s.state() != taskDetached || s.killing || s.remoteOwner != "" || owner != "" && owner != q.Parent {
				return selectedTask{}, false
			}
		}
		candidate.Kind, candidate.Name, candidate.Hosted = "shell", s.record.Name, s.hosted
		if s.hosted {
			candidate.Instance = uid
		}
	case *daemonResourceTask:
		t.mu.Lock()
		defer t.mu.Unlock()
		if q.Detached || q.Kind != "" && q.Kind != t.record.Kind {
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
	defer tasks.mu.Unlock()
	return q.registeredLocked(tasks)
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
				return selectedTask{}, usage("ambiguous task ID %s; use a longer prefix or HOST:ID", ref)
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
		return selectedTask{}, usage("multiple saved connections for task %s; use sshore attach HOST:ID", idMatch.Instance)
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
	namespace := taskQuery{Kind: q.Kind}
	uid := q.UID
	if uid == "" {
		uid = strings.ToLower(q.Ref)
	}
	if validTaskUID(uid) {
		target, _ = namespace.candidate(uid, tasks.lookupLocked(uid))
	}
	if target.Identity == "" && q.UID == "" {
		target, err = matchTask(q.Ref, namespace.registeredLocked(tasks))
	}
	if q.Detached && target.Identity != "" {
		uid = canonicalTaskUID(target.Identity)
		target, _ = q.candidate(uid, tasks.records[uid])
	}
	return target, err
}

func (q taskQuery) run(tasks *daemonTasks) ([]selectedTask, error) {
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if q.Ref == "" && q.UID == "" {
		return q.registeredLocked(tasks), nil
	}
	task, err := q.selectLocked(tasks)
	if err != nil || task.Identity == "" {
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
	query := taskQuery{Kind: "shell", Parent: s.treeIdentity(), Ref: request.Name, Detached: true}
	if s.hosted && !request.NativeTargets {
		query.Kind = "hosted"
	}
	rows, err := query.run(s.tasks)
	if err == nil {
		reply.Targets, err = attachmentChoices(rows)
	}
	if err != nil {
		reply.Error = err.Error()
	} else if request.Name != "" && len(rows) == 0 {
		reply.Error = missingAttachTarget(request.Name).Error()
	}
	return reply
}

func missingAttachTarget(ref string) error {
	return fmt.Errorf("no detached child or idle root matches %q on this host", cleanText(ref))
}
