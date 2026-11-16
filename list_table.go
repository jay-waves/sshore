package main

import (
	"fmt"
	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/xtaci/smux"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type taskTableRow struct {
	id                                                    uint64
	task, owner, target, state, detail, lifetime, checked string
}

func printTaskInventory(inv inventory, details bool) {
	writeTaskInventoryColor(os.Stdout, inv, details, termui.ColorEnabled(os.Stdout))
}

func writeTaskInventory(out io.Writer, inv inventory, details bool) {
	writeTaskInventoryColor(out, inv, details, false)
}

func writeTaskInventoryColor(out io.Writer, inv inventory, details, color bool) {
	now := time.Now()
	rows := []taskTableRow{}
	currentLocal := os.Getenv("SSHORE_LOCAL_SESSION")
	for _, s := range inv.LocalShells {
		state := s.State
		if currentLocal != "" && s.identity == currentLocal && state == "attached" {
			state += " (current)"
		}
		rows = append(rows, taskTableRow{s.ID, shellTaskName(s.Name, s.Temporary), "local", "-", state, s.Directory, shellLifetime(s.Temporary, false), "now"})
	}
	for _, s := range inv.Shells {
		if s.State == "renamed" || (s.Historical && endedShellState(s.State) && !details) {
			continue
		}
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		state, checked := s.State, "now"
		if s.Historical {
			checked = shellCheckedAge(s.ConfirmedAt, now)
			if s.ConfirmedAt.IsZero() {
				state = "unknown"
			}
			if s.Active {
				state += " (local active)"
			} else if s.Unconfirmed {
				state += " (unconfirmed)"
			}
		}
		rows = append(rows, taskTableRow{s.TaskID, shellTaskName(s.Name, s.Temporary), host, "-", state, s.Directory, shellLifetime(s.Temporary, true), checked})
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
		checked := "now"
		if r.Worker == "exited" {
			checked = "-"
		}
		rows = append(rows, taskTableRow{r.TaskID, r.ID, owner, host, r.Worker, r.Detail, "-", checked})
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
		rows = append(rows, taskTableRow{p.TaskID, name, p.Manager, target, p.State + " (read-only)", p.Detail, "-", "-"})
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
		checked := rows[start].checked
		for _, r := range rows[start:end] {
			if r.checked != checked {
				checked = ""
				break
			}
		}
		title := "HOST  " + cleanText(host)
		if color {
			title = "\x1b[1m" + title + "\x1b[22m"
		}
		fmt.Fprint(out, title)
		if checked != "" && checked != "-" {
			label := "checked " + checked
			if checked == "never" {
				label = "never checked"
			}
			fmt.Fprint(out, termui.Format(color, "90", " · %s", label))
		}
		fmt.Fprintln(out)
		table := [][]string{{"ID", "SESSION", "REMOTE", "STATE"}}
		if checked == "" {
			table[0] = append(table[0], "CHECKED")
		}
		if details {
			table[0] = append(table[0], "LIFETIME", "DETAIL")
		}
		for _, r := range rows[start:end] {
			id := "-"
			if r.id != 0 {
				id = strconv.FormatUint(r.id, 10)
			}
			cells := []string{id, r.task, r.target, r.state}
			if checked == "" {
				cells = append(cells, r.checked)
			}
			if details {
				cells = append(cells, r.lifetime, r.detail)
			}
			table = append(table, cells)
		}
		writeTaskTable(out, table, color)
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

func shellCheckedAge(checked, now time.Time) string {
	if checked.IsZero() {
		return "never"
	}
	age := now.Sub(checked)
	// Stored times cannot establish a future observation.
	if age < 0 {
		return "unknown"
	}
	if age < time.Minute {
		return "<1m ago"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
}

func writeTaskTable(out io.Writer, table [][]string, color bool) {
	widths := make([]int, len(table[0]))
	for _, row := range table {
		for j := range row {
			row[j] = cleanText(row[j])
			widths[j] = max(widths[j], (readline.Runes{}).WidthAll([]rune(row[j])))
		}
	}
	for i, row := range table {
		for j, cell := range row {
			level := termui.Color("")
			if i == 0 || j == 0 || j >= 4 || cell == "-" {
				level = "90"
			}
			if i > 0 && j == 3 {
				switch {
				case strings.Contains(cell, "(current)"):
					level = termui.Success
				case strings.Contains(cell, "unconfirmed"), strings.HasPrefix(cell, "unknown"), strings.HasPrefix(cell, "unavailable"), strings.HasPrefix(cell, "failed"), strings.HasPrefix(cell, "error"):
					level = termui.Warning
				}
			}
			fmt.Fprint(out, termui.Format(color && level != "", level, "%s", cell))
			if j < len(row)-1 {
				fmt.Fprint(out, strings.Repeat(" ", widths[j]-(readline.Runes{}).WidthAll([]rune(cell))+2))
			}
		}
		fmt.Fprintln(out)
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

func showConnectedTasks(control *smux.Stream, o options) error {
	if err := streamMessage(control, message{Type: "inventory"}); err != nil {
		return err
	}
	_ = control.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer control.SetReadDeadline(time.Time{})
	inv := emptyInventory()
	inv.Host, inv.RemoteQueried = o.host, true
	for count := 0; ; count++ {
		if count > 10000 {
			return fmt.Errorf("too many tasks")
		}
		m, err := readMessage(control)
		if err != nil {
			return err
		}
		if m.Inventory == nil || (m.Type != "inventory_row" && m.Type != "inventory_done") {
			return fmt.Errorf("invalid task inventory")
		}
		inv.Shells = append(inv.Shells, m.Inventory.Shells...)
		inv.Resources = append(inv.Resources, m.Inventory.Resources...)
		inv.Peers = append(inv.Peers, m.Inventory.Peers...)
		inv.Errors = append(inv.Errors, m.Inventory.Errors...)
		if m.Type == "inventory_done" {
			break
		}
	}
	if err := cacheRemoteShells(o.host, inv); err != nil {
		return err
	}
	shells := inv.Shells[:0]
	for _, s := range inv.Shells {
		if s.Instance != o.instance {
			shells = append(shells, s)
		}
	}
	inv.Shells = shells
	records, err := collectLocalResources(o.host)
	if err != nil {
		return err
	}
	inv.Resources = append(inv.Resources, inspectResources(records)...)
	// A local resource may also advertise itself as a peer on this host.
	inv.Peers = excludeLocalResourcePeers(inv.Peers, records)
	if err := numberInventoryShells(&inv); err != nil {
		return err
	}
	if err := numberInventoryResources(&inv); err != nil {
		return err
	}
	if err := numberInventoryPeers(&inv); err != nil {
		return err
	}
	if len(inv.Shells)+len(inv.Resources)+len(inv.Peers) > 0 {
		fmt.Fprintln(os.Stderr, "Background tasks:")
		writeTaskInventory(os.Stderr, inv, false)
	}
	for _, err := range inv.Errors {
		fmt.Fprintln(os.Stderr, "Task list: "+cleanText(err))
	}
	return nil
}

func excludeLocalResourcePeers(peers []peerView, records []resourceRecord) []peerView {
	localIDs := make(map[string]struct{}, len(records))
	for _, r := range records {
		localIDs[r.Kind+"/"+r.Name] = struct{}{}
	}
	manager := taskManagerLabel()
	filtered := peers[:0]
	for _, p := range peers {
		_, resourceID, wrapped := strings.Cut(strings.TrimPrefix(p.ID, "peer/"), "/")
		_, duplicate := localIDs[resourceID]
		duplicate = wrapped && strings.HasPrefix(p.ID, "peer/") && duplicate
		if p.Manager != manager || !duplicate {
			filtered = append(filtered, p)
		}
	}
	return filtered
}
