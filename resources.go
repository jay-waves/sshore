package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
)

// Local resource IDs are type/name, stable across list calls. Shell identities
// remain HOST/NAME and retain the existing version/process-safe kill path. List
// without HOST is offline discovery: it never authenticates to guessed hosts.

func collectLocalResources(host string) ([]resourceRecord, error) {
	var records []resourceRecord
	for _, kind := range []string{"mount", "forward"} {
		dir, e := resourceStateDir(kind)
		if e != nil {
			return nil, e
		}
		files, e := os.ReadDir(dir)
		if e != nil {
			return nil, e
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			r, e := readResourceRecord(kind, strings.TrimSuffix(f.Name(), ".json"))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return nil, e
			}
			if host == "" || r.Host == host {
				records = append(records, r)
			}
		}
	}
	dir, e := approvalDir()
	if e != nil {
		return nil, e
	}
	pending, e := pendingApprovals(dir)
	if e != nil {
		return nil, e
	}
	for _, r := range pending {
		if host == "" || r.Host == host {
			records = append(records, resourceRecord{Token: r.ID, Kind: "review", Name: r.DisplayName(), Host: r.Host, Detail: r.Session + ": " + approvalSummary(r.Operations)})
		}
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return records, nil
}

// Explicit type prefixes route local cancellation; HOST NAME uses the existing
// version- and process-identity-checked remote session termination path.
func runCancel(args []string) (int, error) {
	return runTargetCommand(append([]string{"cancel"}, args...))
}

func runCancelResource(kind, name string) (int, error) {
	switch kind {
	case "review":
		dir, err := approvalDir()
		if err != nil {
			return 1, err
		}
		record, readErr := findApproval(dir, name)
		if readErr != nil {
			return 1, readErr
		}
		if err = consumeApproval(dir, record.ID); err != nil {
			return 1, err
		}
		removeRejectedPeerTask(record)
		termui.Log(os.Stderr, termui.Success, "[sshore] Request cancelled\n")
		return 0, nil
	case "mount", "forward":
		return 0, stopResource(kind, name)
	default:
		return 2, usage("resource ID must be review/NAME, mount/NAME or forward/NAME")
	}
}

func runCancelAll(o options) (int, error) {
	inv, err := collectInventory(o)
	if err != nil {
		return 1, err
	}
	// Offline shell history and tasks owned by other computers are not targets.
	inv.Peers = nil
	if o.host == "" {
		inv.Shells = nil
	}
	remoteResources := append([]resourceView(nil), inv.Resources...)
	records, err := collectLocalResources(o.host)
	if err != nil {
		return 1, err
	}
	if o.host != "" {
		inv.Resources = append(inv.Resources, inspectResources(records)...)
		if err = numberInventoryResources(&inv); err != nil {
			return 1, err
		}
	}
	if len(inv.LocalShells)+len(inv.Shells)+len(inv.Resources) == 0 {
		if err := stopIdleLocalDaemon(); err != nil {
			return 1, err
		}
		fmt.Println("No tasks to cancel.")
		return 0, nil
	}
	printTaskInventory(inv, false)
	fmt.Fprint(os.Stderr, "End these shells and tasks, unmount mounts, and stop forwards? Type cancel: ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(answer) != "cancel" {
		return 1, fmt.Errorf("cancelled; no tasks ended")
	}
	var failures []error
	for _, s := range inv.LocalShells {
		_, err := runLocalCommand(options{local: true, host: "local", command: "cancel", session: s.Name, taskIdentity: s.identity})
		if err != nil {
			failures = append(failures, err)
		}
	}
	for _, s := range inv.Shells {
		if s.Instance == "" {
			failures = append(failures, fmt.Errorf("shell %s has no confirmed identity", s.Name))
			continue
		}
		target := o
		target.list, target.command, target.session, target.instance = false, "cancel", s.Name, s.Instance
		if _, err := runClient(target); err != nil {
			failures = append(failures, err)
		}
	}
	if o.host != "" {
		for _, r := range remoteResources {
			target := o
			target.list, target.command, target.session, target.taskIdentity = false, "cancel-resource", r.ID, r.Identity
			if _, err := runClient(target); err != nil {
				failures = append(failures, err)
			}
		}
	}

	for _, r := range records {
		if err := cancelResourceIdentity(r.Kind+"/"+r.Name, resourceIdentity(r)); err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return 1, err
	}
	if err := stopIdleLocalDaemon(); err != nil {
		return 1, err
	}
	fmt.Println("Tasks cancelled.")
	return 0, nil
}

func cancelResourceIdentity(id, identity string) error {
	kind, name, ok := strings.Cut(id, "/")
	if !ok || identity == "" {
		return fmt.Errorf("missing resource identity")
	}
	if kind != "review" {
		return stopResource(kind, name, identity)
	}
	dir, err := approvalDir()
	if err != nil {
		return err
	}
	record, err := findApproval(dir, name)
	if err != nil {
		return err
	}
	if resourceIdentity(resourceRecord{Kind: "review", Token: record.ID}) != identity {
		return fmt.Errorf("request changed; refresh with sshore list")
	}
	if err = consumeApproval(dir, record.ID); err == nil {
		removeRejectedPeerTask(record)
	}
	return err
}
