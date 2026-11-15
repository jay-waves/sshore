package main

import "fmt"

func remoteShellIdentity(host, instance string) string {
	if instance == "" {
		return ""
	}
	return "remote:" + host + ":" + instance
}

func numberInventoryShells(inv *inventory) error {
	keys := []string{}
	for _, s := range inv.Shells {
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		if key := remoteShellIdentity(host, s.Instance); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Shells {
		s := &inv.Shells[i]
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		s.TaskID = ids[remoteShellIdentity(host, s.Instance)]
	}
	return nil
}

func runRemoteShellID(id uint64, args []string) (int, bool, error) {
	rows, err := readShellSnapshots()
	if err != nil {
		return 1, true, err
	}
	keys := []string{}
	for _, s := range rows {
		if key := remoteShellIdentity(s.Host, s.Instance); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return 1, true, err
	}
	for _, s := range rows {
		if s.State == "renamed" || s.State == "not-listed" || s.State == "exited" {
			continue
		}
		if ids[remoteShellIdentity(s.Host, s.Instance)] != id {
			continue
		}
		if args[0] == "review" {
			return 2, true, fmt.Errorf("task %d is a shell, not a review", id)
		}
		o := options{host: s.Host, session: s.Name, instance: s.Instance, command: "attach", server: "sshore"}
		if err := resolveRecentAttach(&o); err != nil {
			return 1, true, err
		}
		o.command = args[0]
		if o.command == "rename" {
			o.newName = args[2]
		}
		code, err := runClient(o)
		return code, true, err
	}
	return 0, false, nil
}
