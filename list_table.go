package main

import (
	"fmt"
	"github.com/chzyer/readline"
	"github.com/jay-waves/sshore/internal/termui"
	"github.com/xtaci/smux"
	"golang.org/x/term"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type taskTableRow struct {
	order                                                    uint64
	code, alias, kind, owner, target, state, detail, checked string
}

func printTaskInventory(inv inventory) {
	writeTaskInventoryColor(os.Stdout, inv, termui.ColorEnabled(os.Stdout))
}

func writeTaskInventory(out io.Writer, inv inventory) {
	writeTaskInventoryColor(out, inv, false)
}

func writeTaskInventoryColor(out io.Writer, inv inventory, color bool) {
	now := time.Now()
	screenWidth := 0
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		screenWidth, _, _ = term.GetSize(int(f.Fd()))
	}
	rows := []taskTableRow{}
	currentLocal := os.Getenv("SSHORE_LOCAL_SESSION")
	if currentLocal == "" {
		currentLocal = os.Getenv("SSHORE_TREE_ID")
	}
	for _, s := range inv.LocalShells {
		state := s.State
		if currentLocal != "" && s.identity == currentLocal && state == "attached" {
			state += " (current)"
		}
		rows = append(rows, taskTableRow{s.order, s.ShortID, s.Name, shellTaskKind(s.Temporary), "local", "-", state, s.Directory, "now"})
	}
	for _, s := range inv.Shells {
		if s.State == "renamed" || (s.Historical && endedShellState(s.State)) {
			continue
		}
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		state, checked := s.State, "now"
		if !s.Historical && inv.Host == "" && s.Instance != "" &&
			remoteTreeIdentity(s.Instance) == os.Getenv("SSHORE_TREE_ID") && state == "attached" {
			state += " (current)"
		}
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
		rows = append(rows, taskTableRow{s.order, s.ShortID, s.Name, shellTaskKind(s.Temporary), host, "-", state, s.Directory, checked})
	}
	for _, r := range inv.Resources {
		if r.Kind == "" {
			r.Kind, _, _ = strings.Cut(r.ID, "/")
		}
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
		rows = append(rows, taskTableRow{r.order, r.ShortID, strings.TrimPrefix(r.ID, r.Kind+"/"), r.Kind, owner, host, r.Worker, resourceListDetail(r), checked})
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
		rows = append(rows, taskTableRow{p.order, p.ShortID, peerName(name), peerKind(name), p.Manager, target, p.State + " (read-only)", p.Detail, "-"})
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
		if rows[i].order == 0 {
			return false
		}
		if rows[j].order == 0 {
			return true
		}
		return rows[i].order < rows[j].order
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
		table := [][]string{{"ID", "TYPE", "REMOTE", "STATE"}}
		if checked == "" {
			table[0] = append(table[0], "CHECKED")
		}
		table[0] = append(table[0], "DETAIL")
		for _, r := range rows[start:end] {
			code := r.code
			if code == "" {
				code = "-"
			}
			cells := []string{taskDisplayID(code, r.alias), r.kind, r.target, r.state}
			if checked == "" {
				cells = append(cells, r.checked)
			}
			cells = append(cells, compactTaskDetail(r.detail, -1))
			table = append(table, cells)
		}
		writeTaskTable(out, table, color, screenWidth)
		start = end
	}
}

func resourceListDetail(r resourceView) string {
	if r.LastError != "" {
		return r.LastError
	}
	return r.Detail
}

func compactTaskDetail(detail string, limit int) string {
	detail = cleanText(strings.Join(strings.Fields(detail), " "))
	if detail == "" {
		return "-"
	}
	if limit < 0 || (readline.Runes{}).WidthAll([]rune(detail)) <= limit {
		return detail
	}
	if limit == 0 {
		return ""
	}
	width := 0
	for i, r := range detail {
		width += (readline.Runes{}).Width(r)
		if width > limit-1 {
			return detail[:i] + "…"
		}
	}
	return detail
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

func writeTaskTable(out io.Writer, table [][]string, color bool, screenWidth int) {
	widths := make([]int, len(table[0]))
	for _, row := range table {
		for j := range row {
			row[j] = cleanText(row[j])
			widths[j] = max(widths[j], (readline.Runes{}).WidthAll([]rune(row[j])))
		}
	}
	if screenWidth > 0 && table[0][0] == "ID" {
		aliasLimit := min(32, max(0, screenWidth/4-9))
		widths[0] = len("ID")
		for i := range table {
			if i > 0 {
				code, alias, ok := strings.Cut(table[i][0], " (")
				if ok {
					alias = strings.TrimSuffix(alias, ")")
					table[i][0] = code
					if aliasLimit > 0 {
						table[i][0] += " (" + compactTaskDetail(alias, aliasLimit) + ")"
					}
				}
			}
			widths[0] = max(widths[0], (readline.Runes{}).WidthAll([]rune(table[i][0])))
		}
	}
	// Task IDs and state stay intact; DETAIL uses the remaining terminal width.
	if screenWidth > 0 {
		last := len(widths) - 1
		remaining := screenWidth
		for _, width := range widths[:last] {
			remaining -= width + 2
		}
		for i := range table {
			if remaining <= 0 {
				table[i] = table[i][:last]
			} else {
				table[i][last] = compactTaskDetail(table[i][last], remaining)
			}
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
		id := ids[peerTaskIdentity(*p)]
		p.order, p.ShortID = id.order, id.code
	}
	return nil
}

func peerTaskIdentity(p peerView) string {
	if validTaskUID(p.UID) {
		return "peer-task:" + p.UID
	}
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
	if err := cacheRemoteShells(o.host, inv, o); err != nil {
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
		writeTaskInventory(os.Stderr, inv)
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

func shellTaskKind(temporary bool) string {
	if temporary {
		return "spawn"
	}
	return "launch"
}

func peerKind(name string) string {
	kind, _, _ := strings.Cut(name, "/")
	return kind
}

func peerName(name string) string {
	_, alias, _ := strings.Cut(name, "/")
	return alias
}

func taskDisplayID(code, alias string) string {
	code = cleanText(code)
	if code == "" {
		code = "-"
	}
	if alias == "" || alias == code {
		return code
	}
	return code + " (" + cleanText(alias) + ")"
}
