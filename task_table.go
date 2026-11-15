package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

type taskTableRow struct {
	id                        uint64
	task, host, state, detail string
}

func printTaskInventory(inv inventory, details bool) {
	writeTaskInventory(os.Stdout, inv, details)
}

func writeTaskInventory(out io.Writer, inv inventory, details bool) {
	rows := []taskTableRow{}
	for _, s := range inv.LocalShells {
		rows = append(rows, taskTableRow{s.ID, "shell/" + s.Name, "local", s.State, s.Directory})
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
		rows = append(rows, taskTableRow{s.TaskID, "shell/" + s.Name, host, state, detail})
	}
	for _, r := range inv.Resources {
		host := r.Host
		if host == "" {
			host = inv.Host
		}
		if host == "" {
			host = "local"
		}
		rows = append(rows, taskTableRow{r.TaskID, r.ID, host, r.Worker, r.Detail})
	}
	for _, p := range inv.Peers {
		rows = append(rows, taskTableRow{p.TaskID, p.ID, p.Manager, p.State + " (read-only)", p.Detail})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].id == 0 {
			return false
		}
		if rows[j].id == 0 {
			return true
		}
		return rows[i].id < rows[j].id
	})
	if len(rows) == 0 {
		fmt.Fprintln(out, "No tasks. Create a local shell with sshore shell.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTASK\tHOST\tSTATE\tDETAIL")
	for _, r := range rows {
		id := "-"
		if r.id != 0 {
			id = strconv.FormatUint(r.id, 10)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, cleanText(r.task), cleanText(r.host), cleanText(r.state), cleanText(r.detail))
	}
	w.Flush()
	if details {
		for _, s := range inv.Shells {
			if s.ProcessTree != "" {
				fmt.Fprintf(out, "\nshell/%s\n%s\n", cleanText(s.Name), cleanText(s.ProcessTree))
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
			fmt.Fprintf(out, "\nshell/%s exited %s: %s\n", cleanText(e.Name), e.Time.Format(time.RFC3339), cleanText(e.Reason))
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
