package main

import (
	"cmp"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

func runList(_ cliArguments) (int, error) {
	inv, err := collectInventory()
	writeList(os.Stdout, inv, shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout))
	if err != nil {
		return 1, err
	}
	return 0, nil
}

type taskTableRow struct {
	uid, code, alias, kind, owner, state, parent, detail string
	current                                              bool
}

func writeList(out io.Writer, inv inventory, current string, color bool) {
	screenWidth := 0
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		screenWidth, _, _ = term.GetSize(int(f.Fd()))
	}
	writeListWidth(out, inv, current, color, screenWidth)
}

func writeListWidth(out io.Writer, inv inventory, current string, color bool, screenWidth int) {
	rows := []taskTableRow{}
	hostInfos := map[string]hostInfo{}
	now := time.Now()
	for _, s := range inv.Shells {
		host := cmp.Or(s.Host, localInventoryHost)
		if s.HostInfo != (hostInfo{}) && !taskHiddenFromInventory(s.State) {
			if previous, ok := hostInfos[host]; ok && previous != s.HostInfo {
				// Different targets behind the same SSH name share only the name.
				hostInfos[host] = hostInfo{}
			} else {
				hostInfos[host] = s.HostInfo
			}
		}
		selected := !s.Observed && current != "" && identityFromUID(s.Instance) == current && s.State == taskActive
		detail := joinTaskDetails(cmp.Or(s.ProgramLine, s.Shell), s.Directory)
		if s.Observed {
			seen := "on " + hostDisplayLabel(host)
			if !s.SeenAt.IsZero() {
				seen += " · seen " + observationAge(s.SeenAt, now)
			}
			detail = joinTaskDetails(detail, seen)
		}
		detail = shellListDetail(detail, s.SourceHost)
		detail = pinDetail(detail, s.Pinned, s.PinUntil)
		rows = append(rows, taskTableRow{taskUID(identityFromUID(s.Instance)), s.ShortID, s.Name, terminalTaskKind(s.Program), host, s.State, taskShortCode(s.Owner), detail, selected})
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		rows = append(rows, taskTableRow{taskUID(r.Identity), r.ShortID, name, kind, localInventoryHost, taskActive, "", resourceListDetail(r), false})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(p.ID, "/")
		identity := peerTaskIdentity(p)
		rows = append(rows, taskTableRow{taskUID(identity), taskShortCode(identity), name, kind, incomingInventoryHost, taskActive, "", readOnlyDetail(p.Detail), false})
	}
	rows = slices.DeleteFunc(rows, func(row taskTableRow) bool {
		return taskHiddenFromInventory(row.state)
	})
	sort.SliceStable(rows, func(i, j int) bool {
		if order := compareInventoryHosts(rows[i].owner, rows[j].owner); order != 0 {
			return order < 0
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
		if len(inv.Errors) > 0 {
			return
		}
		fmt.Fprintln(out, "No tasks. Create a local shell with sshore shell.")
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
		info := hostInfos[host]
		if host == localInventoryHost && inv.LocalHost != nil {
			info = *inv.LocalHost
		}
		fmt.Fprint(out, termui.Render(color, termui.Accent, "HOST  "+inventoryHostTitle(host, info)))
		fmt.Fprintln(out)
		table := [][]termui.TableCell{{{Text: "ID"}, {Text: "TYPE"}, {Text: "STATE"}, {Text: "PARENT"}, {Text: "DETAIL"}}}
		currentRow := -1
		for _, r := range rows[start:end] {
			parent := cmp.Or(r.parent, "-")
			cells := []termui.TableCell{
				{Text: taskTableID(r, screenWidth)}, {Text: r.kind}, taskStateCell(r.state),
				{Text: parent}, {Text: compactTaskDetail(r.detail, -1)},
			}
			if r.current {
				currentRow = len(table)
			}
			table = append(table, cells)
		}
		termui.WriteTable(out, table, screenWidth, currentRow, color)
		start = end
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, termui.Render(color, termui.Muted, "R attached or registered  ·  T detached  ·  ? unknown"))
}

func joinTaskDetails(first, second string) string {
	if first == "" || second == "" {
		return first + second
	}
	return first + " · " + second
}

func shellListDetail(detail, source string) string {
	if source == "" {
		return detail
	}
	return joinTaskDetails(detail, "from "+source)
}

func readOnlyDetail(detail string) string {
	if detail == "" {
		return "read-only"
	}
	return "read-only · " + detail
}

func resourceListDetail(r resourceView) string {
	detail := r.Detail
	if r.ReadOnly {
		detail = readOnlyDetail(detail)
	}
	return detail
}

func observationAge(seen, now time.Time) string {
	const day = 24 * time.Hour
	age := now.Sub(seen)
	var unit time.Duration
	var name string
	switch {
	case age >= day:
		unit, name = day, "day"
	case age >= time.Hour:
		unit, name = time.Hour, "hour"
	case age >= time.Minute:
		unit, name = time.Minute, "minute"
	default:
		return "just now"
	}
	count := age / unit
	if count != 1 {
		name += "s"
	}
	return fmt.Sprintf("%d %s ago", count, name)
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

func taskTableID(row taskTableRow, screenWidth int) string {
	alias := row.alias
	if screenWidth > 0 && alias != "" && alias != row.code {
		limit := min(32, max(0, screenWidth/4-9))
		if limit == 0 {
			alias = ""
		} else {
			alias = compactTaskDetail(alias, limit)
		}
	}
	return taskDisplayID(row.code, alias, row.current)
}

func taskStateCell(state string) termui.TableCell {
	cell := termui.TableCell{Text: taskStateCode(state)}
	switch cell.Text {
	case "R":
		cell.Color = termui.Success
	case "?":
		cell.Color = termui.Warning
	}
	return cell
}

func inventoryHostTitle(route string, info hostInfo) string {
	title := hostDisplayLabel(route)
	if info.Address != "" {
		address := info.Address
		if info.Port != "" && info.Port != "22" {
			address = net.JoinHostPort(address, info.Port)
		} else if strings.Contains(address, ":") {
			address = "[" + address + "]"
		}
		if info.User != "" {
			address = info.User + "@" + address
		}
		if address != route {
			title += "  " + cleanText(address)
		}
	}
	var details []string
	for _, value := range []string{info.OS, info.Arch, info.Hostname} {
		if value = cleanText(value); value != "" {
			details = append(details, value)
		}
	}
	if len(details) > 0 {
		title += " (" + strings.Join(details, ", ") + ")"
	}
	return title
}

func pinDetail(detail string, pinned bool, deadline time.Time) string {
	if !pinned {
		return detail
	}
	pin := "pinned"
	if !deadline.IsZero() {
		if deadline.After(time.Now()) {
			pin += " until " + deadline.Local().Format(time.RFC3339)
		} else {
			pin = "pin expired"
		}
	}
	return strings.TrimSpace(strings.Join([]string{pin, detail}, " "))
}
