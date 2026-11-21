package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

type taskTableRow struct {
	uid, code, alias, kind, owner, target, state, detail string
}

func printTaskInventory(inv inventory) {
	writeTaskInventoryColor(os.Stdout, inv, shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout))
}

func writeTaskInventory(out io.Writer, inv inventory) {
	writeTaskInventoryColor(out, inv, "", false)
}

func writeTaskInventoryColor(out io.Writer, inv inventory, current string, color bool) {
	screenWidth := 0
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		screenWidth, _, _ = term.GetSize(int(f.Fd()))
	}
	writeTaskInventoryWidth(out, inv, current, color, screenWidth)
}

func writeTaskInventoryWidth(out io.Writer, inv inventory, current string, color bool, screenWidth int) {
	rows := []taskTableRow{}
	for _, s := range inv.LocalShells {
		state := s.State
		if current != "" && s.identity == current && state == "attached" {
			state += " (current)"
		}
		detail := s.Directory
		if s.ConnectedHost != "" {
			detail = strings.TrimSpace(detail + "  -> " + cleanText(s.ConnectedHost))
		}
		rows = append(rows, taskTableRow{taskUID(s.identity), s.ShortID, s.Name, shellTaskKind(s.Temporary), "local", "-", state, detail})
	}
	for _, s := range inv.Shells {
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		state := s.State
		if !inv.RemoteQueried && s.Instance != "" &&
			remoteTreeIdentity(s.Instance) == current && state == "attached" {
			state += " (current)"
		}
		rows = append(rows, taskTableRow{taskUID(s.identity(inv.Host)), s.ShortID, s.Name, shellTaskKind(s.Temporary), host, "-", state, s.Directory})
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		host := r.Host
		if host == "" {
			host = "-"
		}
		owner := "local"
		if inv.RemoteQueried && !r.local {
			owner = inv.Host
		}
		rows = append(rows, taskTableRow{taskUID(r.Identity), r.ShortID, name, kind, owner, host, r.Worker, resourceListDetail(r)})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		target := inv.Host
		if target == "" {
			target = "local"
		}
		rows = append(rows, taskTableRow{taskUID(peerTaskIdentity(p)), p.ShortID, name, kind, p.Manager, target, p.State + " (read-only)", p.Detail})
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
		if rows[i].uid == "" {
			return false
		}
		if rows[j].uid == "" {
			return true
		}
		return rows[i].uid < rows[j].uid
	})
	if len(rows) == 0 {
		if inv.Host != "" && inv.Host != "local" {
			fmt.Fprintf(out, "No tasks for %s.\n", cleanText(inv.Host))
		} else {
			fmt.Fprintln(out, "No tasks. Create a local shell with sshore launch.")
		}
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
		fmt.Fprint(out, termui.Render(color, termui.Accent, "HOST  "+cleanText(host)))
		fmt.Fprintln(out)
		table := [][]string{{"ID", "TYPE", "REMOTE", "STATE", "DETAIL"}}
		for _, r := range rows[start:end] {
			code := r.code
			if code == "" {
				code = "-"
			}
			cells := []string{taskDisplayID(code, r.alias), r.kind, r.target, r.state, r.detail}
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
	if limit < 0 {
		return detail
	}
	return ansi.Truncate(detail, limit, "…")
}

func writeTaskTable(out io.Writer, table [][]string, color bool, screenWidth int) {
	last := len(table[0]) - 1
	widths := make([]int, last)
	aliasLimit := -1
	if screenWidth > 0 && table[0][0] == "ID" {
		aliasLimit = min(32, max(0, screenWidth/4-9))
	}
	for i, row := range table {
		for j, cell := range row[:last] {
			cell = cleanText(cell)
			if j == 0 && i > 0 && aliasLimit >= 0 {
				if code, alias, ok := strings.Cut(cell, " ("); ok {
					cell = code
					if aliasLimit > 0 {
						cell += " (" + compactTaskDetail(strings.TrimSuffix(alias, ")"), aliasLimit) + ")"
					}
				}
			}
			row[j] = cell
			widths[j] = max(widths[j], lipgloss.Width(cell))
		}
	}
	const remote = 2
	if screenWidth > 0 && last > remote && table[0][remote] == "REMOTE" {
		// Keep the ID and state readable when a destination is wider than the panel.
		width := screenWidth
		for j, w := range widths {
			if j != remote {
				width -= w
			}
		}
		width -= 2 * (last - 1)
		widths[remote] = min(widths[remote], max(width, lipgloss.Width(table[0][remote])))
		for _, row := range table[1:] {
			row[remote] = ansi.Truncate(row[remote], widths[remote], "…")
		}
	}
	// Task IDs and state stay intact; DETAIL uses the remaining terminal width.
	columns, remaining := last+1, -1
	if screenWidth > 0 {
		remaining = screenWidth
		for _, width := range widths {
			remaining -= width + 2
		}
		if remaining <= 0 {
			columns = last
		}
	}
	if screenWidth >= 64 && writeStyledTaskTable(out, table, columns, remaining, color, screenWidth) {
		return
	}
	for i, row := range table {
		var line strings.Builder
		for j, cell := range row[:columns] {
			padding := 0
			if j < columns-1 {
				padding = widths[j] - lipgloss.Width(cell) + 2
			}
			if j == last {
				cell = compactTaskDetail(cell, remaining)
			}
			if i > 0 && j == 3 {
				switch {
				case strings.Contains(cell, "(current)"), cell == "running":
					cell = termui.Render(color, termui.Success, cell)
				case strings.Contains(cell, "unconfirmed"), strings.HasPrefix(cell, "unknown"), strings.HasPrefix(cell, "unavailable"), strings.HasPrefix(cell, "failed"), strings.HasPrefix(cell, "error"), cell == "exited":
					cell = termui.Render(color, termui.Warning, cell)
				}
			}
			line.WriteString(cell)
			line.WriteString(strings.Repeat(" ", padding))
		}
		fmt.Fprintln(out, termui.Render(color && i == 0, termui.Heading, line.String()))
	}
}

// writeStyledTaskTable keeps compact output for narrow terminals and renders a
// bordered table only when the task ID and state fit without truncation.
func writeStyledTaskTable(out io.Writer, rows [][]string, columns, remaining int, color bool, screenWidth int) bool {
	if columns != len(rows[0]) || remaining <= columns+3 {
		return false
	}
	visible := make([][]string, len(rows))
	for i, row := range rows {
		visible[i] = make([]string, columns)
		for j, cell := range row[:columns] {
			if j == columns-1 && i > 0 {
				cell = compactTaskDetail(cell, remaining-columns-3)
			}
			visible[i][j] = cleanText(cell)
		}
	}
	cell := lipgloss.NewStyle().Padding(0, 1)
	header, border := cell, lipgloss.NewStyle()
	if color {
		header = header.Bold(true).Foreground(ansi.Cyan)
		border = border.Foreground(ansi.BrightBlack)
	}
	t := table.New().Border(lipgloss.RoundedBorder()).BorderStyle(border).Headers(visible[0]...).Rows(visible[1:]...).Wrap(false).FitContent().StyleFunc(func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return header
		}
		if color && col == 3 {
			state := visible[row+1][col]
			switch {
			case strings.Contains(state, "(current)"), state == "running":
				return cell.Foreground(ansi.Green)
			case strings.Contains(state, "unconfirmed"), strings.HasPrefix(state, "unknown"), strings.HasPrefix(state, "unavailable"), strings.HasPrefix(state, "failed"), strings.HasPrefix(state, "error"), state == "exited":
				return cell.Foreground(ansi.Yellow)
			}
		}
		return cell
	})
	rendered := t.String()
	if lipgloss.Width(rendered) > screenWidth {
		return false
	}
	fmt.Fprintln(out, rendered)
	return true
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

func showConnectedTasks(o options) error {
	inv, err := daemonListTasks(taskListRequest{Host: o.host})
	if err != nil {
		return err
	}
	shells := inv.Shells[:0]
	for _, shell := range inv.Shells {
		if shell.Instance != o.instance {
			shells = append(shells, shell)
		}
	}
	inv.Shells = shells
	if len(inv.Shells)+len(inv.Resources)+len(inv.Peers) > 0 {
		fmt.Fprintln(os.Stderr, "Background tasks:")
		writeTaskInventory(os.Stderr, inv)
	}
	return inv.incompleteError()
}

func peerResourceID(id string) string {
	if rest, wrapped := strings.CutPrefix(id, "peer/"); wrapped {
		if _, resource, ok := strings.Cut(rest, "/"); ok {
			return resource
		}
	}
	return id
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
