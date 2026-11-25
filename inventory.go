package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

const (
	taskActive   = "active"
	taskDetached = "detached"
	taskWaiting  = "waiting"
	taskStarting = "starting"
	taskUnknown  = "unknown"
	taskExited   = "exited"
)

type resourceView struct {
	ShortID    string `json:"short_id,omitempty"`
	Identity   string `json:"identity,omitempty"`
	ID         string `json:"id"`
	Host       string `json:"host"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Detail     string `json:"detail"`
	Protocol   string `json:"protocol,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Direction  string `json:"direction,omitempty"`
	resourceStatus
}

func (r resourceView) kindAndName() (string, string) {
	kind := r.Kind
	if kind == "" {
		kind, _, _ = strings.Cut(r.ID, "/")
	}
	return kind, strings.TrimPrefix(r.ID, kind+"/")
}

type shellView struct {
	SourceHost  string    `json:"source_host,omitempty"`
	MachineID   string    `json:"machine_id,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	PinUntil    time.Time `json:"pin_until,omitzero"`
	Instance    string    `json:"instance,omitempty"`
	ShortID     string    `json:"short_id,omitempty"`
	Host        string    `json:"host,omitempty"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Observed    bool      `json:"observed,omitempty"`
	SeenAt      time.Time `json:"seen_at,omitzero"`
	PID         int       `json:"pid"`
	Age         *int64    `json:"age_seconds"`
	Foreground  string    `json:"foreground"`
	Shell       string    `json:"shell,omitempty"`
	Directory   string    `json:"directory"`
	ProcessTree string    `json:"process_tree,omitempty"`
}
type peerView struct {
	Direction string `json:"direction,omitempty"`
	UID       string `json:"uid,omitempty"`
	Identity  string `json:"identity,omitempty"`
	ShortID   string `json:"short_id,omitempty"`
	ID        string `json:"id"`
	Manager   string `json:"manager"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}
type inventory struct {
	LocalShells []localShellView `json:"local_shells,omitempty"`
	Resources   []resourceView   `json:"resources"`
	Shells      []shellView      `json:"shells"`
	Peers       []peerView       `json:"peers"`
	Errors      []string         `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Resources: []resourceView{}, Shells: []shellView{}, Peers: []peerView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{Identity: resourceIdentity(r), ID: r.Kind + "/" + r.Name, Host: r.Host, Kind: r.Kind, Direction: r.Direction, State: taskUnknown, Detail: r.Detail, resourceStatus: resourceStatus{Version: r.Version, Transport: "unknown", Target: "unchecked"}}
	v.Protocol = "SSH/TCP"
	if r.Kind == "mount" {
		v.Protocol = "SFTP over SSH"
		v.Direction = "<-"
		v.Mountpoint = r.Mountpoint
		v.ReadOnly = r.ReadOnly
		v.Detail = r.Mountpoint + " -> " + sshio.FormatEndpoint(r.Host, r.Remote)
	}
	return v
}

// Read only registered tasks here. Historical and peer observations are
// collected separately and cannot become part of this snapshot.
func readTaskInventory(request taskInventoryRequest) (taskInventorySnapshot, map[string]resourceStatus, error) {
	timeout := daemonRequestTimeout
	if request.Status && !request.Native && !request.Hosted {
		timeout = resourceStatusTimeout
	}
	if request.Detail == "health" {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	statuses := make(map[string]resourceStatus)
	var snapshot taskInventorySnapshot
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return snapshot, nil, nil
	}
	if err != nil {
		return snapshot, nil, err
	}
	req, err := daemonRequest(ctx, record, "/tasks/snapshot", request)
	if err != nil {
		return snapshot, nil, err
	}
	resp, err := daemonDo(req)
	if err != nil {
		return snapshot, nil, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&snapshot); err != nil {
		return snapshot, nil, err
	}
	if request.Status {
		for range snapshot.Resources {
			var row resourceStatusRow
			if dec.Decode(&row) != nil {
				break
			}
			statuses[row.UID] = row.Status
		}
	}
	return snapshot, statuses, nil
}

func (snapshot taskInventorySnapshot) nativeShells() []localShellView {
	rows := snapshot.Native
	for i := range rows {
		rows[i].ShortID = taskShortCode(rows[i].Identity)
	}
	sortLocalShells(rows)
	return rows
}

func excludeLocalResourcePeers(peers []peerView, resources []resourceView) []peerView {
	local := make(map[string]bool, len(resources))
	for _, r := range resources {
		local[canonicalTaskUID(r.Identity)] = true
	}
	return slices.DeleteFunc(peers, func(p peerView) bool { return local[p.UID] })
}

type taskListRequest struct {
	Tree bool
}

func (a *daemonActivity) listTasks(ctx context.Context, request taskListRequest) inventory {
	query := taskInventoryRequest{Native: true, Hosted: true, Resources: true, Status: !request.Tree, Detail: "inventory"}
	if request.Tree {
		query.Detail = "health"
	}
	snapshot, workers := a.tasks.taskInventory(query)
	inv := emptyInventory()
	inv.Shells = append(inv.Shells, snapshot.Shells...)
	inv.LocalShells = snapshot.nativeShells()
	statuses := make(map[string]resourceStatus)
	if query.Status {
		probeCtx, cancel := context.WithTimeout(ctx, resourceStatusTimeout)
		defer cancel()
		probeResourceStatus(probeCtx, workers, func(row resourceStatusRow) bool {
			statuses[row.UID] = row.Status
			return true
		})
	}
	inv.Resources = resourceViews(snapshot.Resources, statuses, query.Status)
	inv.Peers = excludeLocalResourcePeers(peerViews(a.incomingPeers()), inv.Resources)
	reply := a.handleMetadata(metadataRequest{Action: "participation_get"})
	if reply.Error != "" {
		inv.Errors = append(inv.Errors, "remote shell history unavailable: "+cleanText(reply.Error))
	}
	for _, row := range reply.Participants {
		inv.Shells = append(inv.Shells, shellView{Host: row.Host, Owner: row.Owner, Instance: row.Instance, ID: "sh/" + row.Name, Name: row.Name, State: row.State, Observed: true, SeenAt: row.ChangedAt, Pinned: row.Pinned, PinUntil: row.PinUntil, PID: row.PID, Shell: row.Shell})
	}
	inv.setTaskReferences()
	return inv
}

func daemonListTasks(request taskListRequest) (inventory, error) {
	if activity := accountActivity.Load(); activity != nil {
		return activity.listTasks(context.Background(), request), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
	defer cancel()
	record, err := activityDaemon(ctx)
	if os.IsNotExist(err) && !activityAllowed() {
		return emptyInventory(), nil
	}
	if err != nil {
		return emptyInventory(), fmt.Errorf("open local task inventory: %w", err)
	}
	var inv inventory
	if err := daemonJSON(ctx, record, "/tasks/list", request, &inv); err != nil {
		return emptyInventory(), fmt.Errorf("read local task inventory: %w", err)
	}
	return inv, nil
}

func collectInventory(tree bool) (inventory, error) {
	inv, err := daemonListTasks(taskListRequest{Tree: tree})
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
		return inv, err
	}
	return inv, inv.incompleteError()
}

func (inv inventory) incompleteError() error {
	if len(inv.Errors) > 0 {
		return fmt.Errorf("inventory incomplete: %s", strings.Join(inv.Errors, "; "))
	}
	return nil
}

func (inv *inventory) setTaskReferences() {
	for i := range inv.Shells {
		shell := &inv.Shells[i]
		shell.ShortID = taskShortCode(identityFromUID(shell.Instance))
	}
	for i := range inv.Resources {
		resource := &inv.Resources[i]
		resource.ShortID = taskShortCode(resource.Identity)
	}
	slices.SortStableFunc(inv.Resources, func(a, b resourceView) int {
		if order := cmp.Compare(a.ShortID, b.ShortID); order != 0 {
			return order
		}
		return cmp.Compare(taskUID(a.Identity), taskUID(b.Identity))
	})
	for i := range inv.Peers {
		peer := &inv.Peers[i]
		peer.ShortID = taskShortCode(peerTaskIdentity(*peer))
	}
}

func runInventoryCommand(parsed cliArguments) (int, error) {
	command := parsed.command
	current, color := shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout)
	inv, err := collectInventory(command == "pstree")
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	writeInventory(os.Stdout, command, inv, current, color, width)
	if err != nil {
		return 1, err
	}
	return 0, nil
}

type taskTableRow struct {
	uid, code, alias, kind, owner, state, parent, detail string
	current                                              bool
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
		selected := current != "" && s.Identity == current && s.State == taskActive
		detail := shellListDetail(shellDirectoryDetail(s.Shell, s.Directory), s.SourceHost)
		detail = pinDetail(detail, s.Pinned, s.PinUntil)
		rows = append(rows, taskTableRow{taskUID(s.Identity), s.ShortID, s.Name, "shell", "local", s.State, taskShortCode(s.Owner), detail, selected})
	}
	now := time.Now()
	for _, s := range inv.Shells {
		host := cmp.Or(s.Host, "local")
		selected := !s.Observed && current != "" && identityFromUID(s.Instance) == current && s.State == taskActive
		detail := shellDirectoryDetail(s.Shell, s.Directory)
		if s.Observed {
			seen := "on " + host
			if !s.SeenAt.IsZero() {
				seen += " · seen " + observationAge(s.SeenAt, now)
			}
			detail = joinTaskDetails(seen, detail)
		}
		detail = shellListDetail(detail, s.SourceHost)
		detail = pinDetail(detail, s.Pinned, s.PinUntil)
		rows = append(rows, taskTableRow{taskUID(identityFromUID(s.Instance)), s.ShortID, s.Name, "shell", host, s.State, taskShortCode(s.Owner), detail, selected})
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		rows = append(rows, taskTableRow{taskUID(r.Identity), r.ShortID, name, kind, "local", r.State, "", resourceListDetail(r), false})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		rows = append(rows, taskTableRow{taskUID(peerTaskIdentity(p)), p.ShortID, name, kind, p.Manager, p.State, "", readOnlyDetail(p.Detail), false})
	}
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
		fmt.Fprint(out, termui.Render(color, termui.Accent, "HOST  "+cleanText(host)))
		fmt.Fprintln(out)
		table := [][]string{{"ID", "TYPE", "STATE", "PARENT", "DETAIL"}}
		currentRow := -1
		for _, r := range rows[start:end] {
			code := r.code
			if code == "" {
				code = "-"
			}
			parent := cmp.Or(r.parent, "-")
			cells := []string{taskDisplayID(code, r.alias), r.kind, r.state, parent, r.detail}
			if r.current {
				currentRow = len(table)
			}
			table = append(table, cells)
		}
		writeTaskTable(out, table, color, screenWidth, currentRow)
		start = end
	}
}

func joinTaskDetails(first, second string) string {
	if first == "" || second == "" {
		return first + second
	}
	return first + " · " + second
}

func shellDirectoryDetail(shell, directory string) string {
	if shell == "" || directory == "" {
		return shell + directory
	}
	return shell + " " + directory
}

func shellListDetail(detail, source string) string {
	if source == "" {
		return detail
	}
	return joinTaskDetails("from "+source, detail)
}

func readOnlyDetail(detail string) string {
	if detail == "" {
		return "read-only"
	}
	return "read-only · " + detail
}

func resourceListDetail(r resourceView) string {
	detail := joinTaskDetails(r.LastError, r.Detail)
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

const taskStateColumn = 2

func writeTaskTable(out io.Writer, table [][]string, color bool, screenWidth, currentRow int) {
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
			if i == currentRow && j == 0 {
				cell = "* " + cell
			}
			row[j] = cell
			widths[j] = max(widths[j], ansi.StringWidth(cell))
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
	if screenWidth >= 64 && writeStyledTaskTable(out, table, columns, remaining, color, screenWidth, currentRow) {
		return
	}
	for i, row := range table {
		var line strings.Builder
		for j, cell := range row[:columns] {
			padding := 0
			if j < columns-1 {
				padding = widths[j] - ansi.StringWidth(cell) + 2
			}
			if j == last {
				cell = compactTaskDetail(cell, remaining)
			}
			if color && i > 0 && i != currentRow && j == taskStateColumn {
				cell = renderTaskState(cell)
			}
			line.WriteString(cell)
			line.WriteString(strings.Repeat(" ", padding))
		}
		rendered := termui.Render(color && i == 0, termui.Heading, line.String())
		fmt.Fprintln(out, termui.Render(color && i == currentRow, termui.Accent, rendered))
	}
}

func renderTaskState(state string) string {
	switch state {
	case taskActive:
		return termui.Render(true, termui.Success, state)
	case taskUnknown, taskExited:
		return termui.Render(true, termui.Warning, state)
	default:
		return state
	}
}

// writeStyledTaskTable keeps compact output for narrow terminals and renders a
// bordered table only when the task ID and state fit without truncation.
func writeStyledTaskTable(out io.Writer, rows [][]string, columns, remaining int, color bool, screenWidth, currentRow int) bool {
	if columns != len(rows[0]) || remaining <= columns+3 {
		return false
	}
	visible := make([][]string, len(rows))
	widths := make([]int, columns)
	for i, row := range rows {
		visible[i] = make([]string, columns)
		for j, cell := range row[:columns] {
			if j == columns-1 && i > 0 {
				cell = compactTaskDetail(cell, remaining-columns-3)
			}
			cell = cleanText(cell)
			visible[i][j] = cell
			widths[j] = max(widths[j], ansi.StringWidth(cell))
		}
	}
	total := 1
	borders := make([]string, columns)
	for j, width := range widths {
		total += width + 3
		borders[j] = strings.Repeat("─", width+2)
	}
	if total > screenWidth {
		return false
	}
	border := func(text string) string { return termui.Render(color, termui.Muted, text) }
	divider := border("│")
	fmt.Fprintln(out, border("╭"+strings.Join(borders, "┬")+"╮"))
	for i, row := range visible {
		fmt.Fprint(out, divider)
		for j, cell := range row {
			padding := widths[j] - ansi.StringWidth(cell)
			if color {
				switch {
				case i == 0:
					cell = termui.Render(true, termui.Accent, cell)
				case i == currentRow:
					cell = termui.Render(true, termui.Info, cell)
				case j == taskStateColumn:
					cell = renderTaskState(cell)
				}
			}
			fmt.Fprint(out, " ", cell, strings.Repeat(" ", padding+1), divider)
		}
		fmt.Fprintln(out)
		if i == 0 {
			fmt.Fprintln(out, border("├"+strings.Join(borders, "┼")+"┤"))
		}
	}
	fmt.Fprintln(out, border("╰"+strings.Join(borders, "┴")+"╯"))
	return true
}

func peerTaskIdentity(p peerView) string {
	return "peer-task:" + p.UID
}

func peerResourceID(id string) string {
	if rest, wrapped := strings.CutPrefix(id, "peer/"); wrapped {
		if _, resource, ok := strings.Cut(rest, "/"); ok {
			return resource
		}
	}
	return id
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

// A snapshot lists parents before children. Owner is the parent's one-based
// row index, or zero for a root. It carries display fields only.
type taskTreeRow struct {
	Identity, Kind, Code, Name, State, Remote string
	Owner                                     int
}

func writeInventory(out io.Writer, command string, inv inventory, current string, color bool, width int) {
	if command == "pstree" {
		rows := normalizeTaskTree(taskTreeNodes(inv))
		if len(rows) != 0 || len(inv.Errors) == 0 {
			writeShellTree(out, rows, current, color)
		}
		return
	}
	writeTaskInventoryWidth(out, inv, current, color, width)
}

func normalizeTaskTree(nodes []shellTreeNode) []taskTreeRow {
	owners := shellTreeOwners(nodes)
	// Each node has at most one owner. Cut a display edge at each cycle so
	// every component has a root, even when cached relationships disagree.
	marks := make([]int, len(nodes))
	for i := range nodes {
		walk := i + 1
		for p := i; p >= 0; p = owners[p] {
			if marks[p] != 0 {
				if marks[p] == walk {
					// Give the same cycle a root regardless of inventory order.
					root := p
					for member := owners[p]; member != p; member = owners[member] {
						if nodes[member].Identity < nodes[root].Identity {
							root = member
						}
					}
					owners[root] = -1
				}
				break
			}
			marks[p] = walk
		}
	}
	// Bucket zero holds roots; the rest hold each owner's children.
	children := make([][]int, len(nodes)+1)
	for i, owner := range owners {
		bucket := owner + 1
		children[bucket] = append(children[bucket], i)
	}
	compare := func(a, b int) int {
		return cmp.Or(cmp.Compare(nodes[a].displayCode(), nodes[b].displayCode()),
			cmp.Compare(nodes[a].Identity, nodes[b].Identity))
	}
	for _, siblings := range children {
		slices.SortStableFunc(siblings, compare)
	}

	byHost := make(map[string][]int)
	for _, i := range children[0] {
		host := cmp.Or(nodes[i].Host, "local")
		byHost[host] = append(byHost[host], i)
	}
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	slices.SortFunc(hosts, compareInventoryHosts)

	rows := make([]taskTreeRow, 0, len(nodes)+len(hosts))
	var appendRows func([]int, int, string)
	appendRows = func(siblings []int, owner int, parentHost string) {
		for _, i := range siblings {
			n := nodes[i]
			row := taskTreeRow{Identity: n.Identity, Kind: n.Kind, Code: n.displayCode(), Name: n.Name, State: n.State, Owner: owner}
			host := cmp.Or(n.Host, "local")
			if host != parentHost {
				row.Remote = host
			}
			rows = append(rows, row)
			appendRows(children[i+1], len(rows), host)
		}
	}
	for _, host := range hosts {
		rows = append(rows, taskTreeRow{Code: host})
		appendRows(byHost[host], len(rows), host)
	}
	return rows
}

func compareInventoryHosts(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "local":
		return -1
	case b == "local":
		return 1
	default:
		return cmp.Compare(a, b)
	}
}

func shellTreeOwners(nodes []shellTreeNode) []int {
	byID := make(map[string]int, len(nodes))
	for i, n := range nodes {
		if n.Identity != "" {
			byID[n.Identity] = i
		}
	}
	owners := make([]int, len(nodes))
	for i, n := range nodes {
		owner, found := byID[n.Owner]
		if !found || owner == i {
			owner = -1
		}
		owners[i] = owner
	}
	return owners
}

func (n shellTreeNode) displayCode() string {
	if n.ShortID != "" {
		return n.ShortID
	}
	return taskShortCode(n.Identity)
}

// The local registry is authoritative. For other shells, show the newest
// authenticated observation of each UID, even when two SSH aliases reach it.
func taskTreeNodes(inv inventory) []shellTreeNode {
	nodes := make([]shellTreeNode, 0, len(inv.LocalShells)+len(inv.Shells)+len(inv.Resources)+len(inv.Peers))
	visible := make(map[string]int)
	add := func(n shellTreeNode) {
		if n.State == taskExited {
			return
		}
		if i, found := visible[n.Identity]; found {
			if !nodes[i].SeenAt.IsZero() && (n.SeenAt.IsZero() || n.SeenAt.After(nodes[i].SeenAt)) {
				nodes[i] = n
			}
			return
		}
		visible[n.Identity] = len(nodes)
		nodes = append(nodes, n)
	}
	for _, s := range inv.LocalShells {
		add(shellTreeNode{Kind: "shell", Identity: s.Identity, ShortID: s.ShortID, Owner: s.Owner, Name: s.Name, State: s.State})
	}
	for _, s := range inv.Shells {
		n := shellTreeNode{Kind: "shell", Identity: identityFromUID(s.Instance), ShortID: s.ShortID, Owner: s.Owner, Name: s.Name, State: s.State}
		if s.Observed {
			n.Host, n.SeenAt = s.Host, s.SeenAt
		}
		add(n)
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		add(shellTreeNode{Kind: kind, Identity: r.Identity, ShortID: r.ShortID, Name: name})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		add(shellTreeNode{Kind: kind, Identity: peerTaskIdentity(p), ShortID: p.ShortID, Host: cmp.Or(p.Manager, "unknown"), Name: name, State: p.State})
	}
	return nodes
}

type shellTreeNode struct {
	Kind, Host, Identity, ShortID, Owner, Name, State string
	SeenAt                                            time.Time
}

func writeShellTree(out io.Writer, rows []taskTreeRow, current string, color bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No tasks.")
		return
	}
	children := make([][]int, len(rows)+1)
	for i, row := range rows {
		owner := row.Owner
		if owner < 0 || owner > i {
			owner = 0
		}
		children[owner] = append(children[owner], i)
	}
	var writeBranch func(int, string, string)
	writeBranch = func(i int, indent, branch string) {
		row := rows[i]
		fmt.Fprintln(out, termui.Render(color, termui.Muted, indent+branch)+shellTreeLabel(row, current != "" && row.Identity == current, color))
		if branch == "├── " {
			indent += "│   "
		} else if branch != "" {
			indent += "    "
		}
		kids := children[i+1]
		for j, child := range kids {
			branch := "├── "
			if j == len(kids)-1 {
				branch = "╰── "
			}
			writeBranch(child, indent, branch)
		}
	}
	for _, root := range children[0] {
		writeBranch(root, "", "")
	}
}

func shellTreeLabel(n taskTreeRow, current, color bool) string {
	label := cleanText(n.Code)
	if n.Kind != "" {
		label = cleanText(n.Kind) + "/" + label
	}
	if n.Name != "" && n.Name != n.Code {
		label += " (" + cleanText(n.Name) + ")"
	}
	if n.Remote != "" {
		label += " on " + cleanText(n.Remote)
	}
	if current {
		label = "* " + label
	}

	state := cleanText(n.State)
	if state == "" {
		return termui.Render(color && current, termui.Accent, label)
	}
	style := termui.Muted
	if n.State == taskUnknown {
		style = termui.Warning
	}
	return termui.Render(color && current, termui.Accent, label) + "  " +
		termui.Render(color, style, state)
}

func shellIdentityFromEnvironment() string {
	if id := os.Getenv("SSHORE_TREE_ID"); id != "" {
		return id
	}
	return os.Getenv("SSHORE_LOCAL_SESSION")
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

func rememberShellExit(o options) error {
	detachErr := removeRecentDetach(o)
	if o.local || !validTaskUID(o.instance) {
		return detachErr
	}
	return errors.Join(detachErr, forgetParticipatingShell(o.host, o.instance))
}
