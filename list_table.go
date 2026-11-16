package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type taskTableRow struct {
	id                                 uint64
	task, owner, target, state, detail string
}

func printTaskInventory(inv inventory, details bool) {
	writeTaskInventory(os.Stdout, inv, details)
}

func writeTaskInventory(out io.Writer, inv inventory, details bool) {
	rows := []taskTableRow{}
	currentLocal := os.Getenv("SSHORE_LOCAL_SESSION")
	for _, s := range inv.LocalShells {
		state := s.State
		if currentLocal != "" && s.identity == currentLocal && state == "attached" {
			state += " (current)"
		}
		rows = append(rows, taskTableRow{s.ID, shellTaskName(s.Name, s.Temporary), "local", "-", state, s.Directory})
	}
	for _, s := range inv.Shells {
		if s.State == "renamed" {
			continue
		}
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		state := s.State
		if s.Historical {
			state += " (cached; " + snapshotState(s) + ")"
		}
		detail := s.Directory
		if s.Historical && !s.ConfirmedAt.IsZero() {
			detail = "confirmed " + time.Since(s.ConfirmedAt).Round(time.Second).String() + " ago"
		}
		rows = append(rows, taskTableRow{s.TaskID, shellTaskName(s.Name, s.Temporary), host, "-", state, detail})
	}
	for _, r := range inv.Resources {
		host := r.Host
		if host == "" {
			host = "-"
		}
		owner := "local"
		if inv.RemoteQueried && !r.local {
			owner = inv.Host
		}
		rows = append(rows, taskTableRow{r.TaskID, r.ID, owner, host, r.Worker, r.Detail})
	}
	for _, p := range inv.Peers {
		name := p.ID
		if strings.HasPrefix(name, "peer/") {
			if _, resource, ok := strings.Cut(strings.TrimPrefix(name, "peer/"), "/"); ok {
				name = resource
			}
		}
		target := inv.Host
		if target == "" {
			target = "local"
		}
		rows = append(rows, taskTableRow{p.TaskID, name, p.Manager, target, p.State + " (read-only)", p.Detail})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].owner != rows[j].owner {
			if rows[i].owner == "local" {
				return true
			}
			if rows[j].owner == "local" {
				return false
			}
			return rows[i].owner < rows[j].owner
		}
		if rows[i].id == 0 {
			return false
		}
		if rows[j].id == 0 {
			return true
		}
		return rows[i].id < rows[j].id
	})
	if len(rows) == 0 {
		fmt.Fprintln(out, "No tasks. Create a local shell with sshore launch.")
		return
	}
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].owner == rows[start].owner {
			end++
		}
		if start > 0 {
			fmt.Fprintln(out)
		}
		host := rows[start].owner
		if host == "" {
			host = "unknown"
		}
		fmt.Fprintf(out, "HOST · %s\n", cleanText(host))
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprint(w, "ID\tSESSION\tREMOTE\tSTATE")
		if details {
			fmt.Fprint(w, "\tDETAIL")
		}
		fmt.Fprintln(w)
		for _, r := range rows[start:end] {
			id := "-"
			if r.id != 0 {
				id = strconv.FormatUint(r.id, 10)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s", id, cleanText(r.task), cleanText(r.target), cleanText(r.state))
			if details {
				fmt.Fprintf(w, "\t%s", cleanText(r.detail))
			}
			fmt.Fprintln(w)
		}
		w.Flush()
		start = end
	}
	if details {
		for _, p := range inv.Peers {
			fmt.Fprintf(out, "\n%s: managed by %s (read-only mirror)\n", cleanText(p.ID), cleanText(p.Manager))
		}
		for _, s := range inv.Shells {
			if s.ProcessTree != "" {
				fmt.Fprintf(out, "\n%s\n", shellTaskName(cleanText(s.Name), s.Temporary))
				for _, line := range strings.Split(strings.TrimSuffix(s.ProcessTree, "\n"), "\n") {
					fmt.Fprintln(out, cleanText(line))
				}
			}
		}
		for _, r := range inv.Resources {
			fmt.Fprintf(out, "\n%s: transport=%s target=%s protocol=%s read-only=%t\n", cleanText(r.ID), cleanText(r.Transport), cleanText(r.Target), cleanText(r.Protocol), r.ReadOnly)
			if r.LastError != "" {
				fmt.Fprintln(out, "  "+cleanText(r.LastError))
			}
			if r.Log != "" {
				fmt.Fprintln(out, "  log: "+cleanText(r.Log))
			}
		}
		for _, e := range inv.Exits {
			fmt.Fprintf(out, "\n%s exited %s: %s\n", cleanText(e.Name), e.Time.Format(time.RFC3339), cleanText(e.Reason))
		}
	}
}

func numberInventoryPeers(inv *inventory) error {
	keys := []string{}
	for _, p := range inv.Peers {
		keys = append(keys, peerTaskIdentity(p))
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Peers {
		p := &inv.Peers[i]
		p.TaskID = ids[peerTaskIdentity(*p)]
	}
	return nil
}

func peerTaskIdentity(p peerView) string {
	if p.Identity != "" {
		return "peer:" + p.Identity
	}
	return "peer:" + p.Manager + ":" + p.ID
}
