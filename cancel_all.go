package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

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
