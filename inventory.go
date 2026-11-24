package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"charm.land/lipgloss/v2/tree"
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
	Log        string `json:"log,omitempty"`
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
	Tree        []taskTreeRow    `json:"tree,omitempty"`
	MachineID   string           `json:"machine_id,omitempty"`
	Hostname    string           `json:"hostname,omitempty"`
	LocalShells []localShellView `json:"local_shells,omitempty"`
	Version     string           `json:"version"`
	Host        string           `json:"host,omitempty"`
	Resources   []resourceView   `json:"resources"`
	Shells      []shellView      `json:"shells"`
	Peers       []peerView       `json:"peers"`
	Errors      []string         `json:"errors"`
}

func emptyInventory() inventory {
	return inventory{Version: version, Resources: []resourceView{}, Shells: []shellView{}, Peers: []peerView{}, Errors: []string{}}
}
func viewRecord(r resourceRecord) resourceView {
	v := resourceView{Identity: resourceIdentity(r), ID: r.Kind + "/" + r.Name, Host: r.Host, Kind: r.Kind, Direction: r.Direction, State: taskUnknown, Detail: r.Detail, resourceStatus: resourceStatus{Version: r.Version, Transport: "unknown", Target: "unchecked"}}
	if log, err := statePath(filepath.Join(r.Kind+"s", r.UID+".log")); err == nil {
		v.Log = log
	}
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

func managedInventory(ctx context.Context, tasks *daemonTasks, request taskInventoryRequest) inventory {
	inv := emptyInventory()
	if runtime.GOOS == "linux" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	snapshot, workers := tasks.taskInventory(request)
	inv.Shells = append(inv.Shells, snapshot.Shells...)
	if request.Native {
		inv.LocalShells = snapshot.nativeShells()
	}
	if request.Resources {
		statuses := make(map[string]resourceStatus)
		ctx, cancel := context.WithTimeout(ctx, resourceStatusTimeout)
		defer cancel()
		probeResourceStatus(ctx, workers, func(row resourceStatusRow) bool {
			statuses[row.UID] = row.Status
			return true
		})
		inv.Resources = resourceViews(snapshot.Resources, statuses, request.Status)
	}
	return inv
}

func excludeLocalResourcePeers(peers []peerView, resources []resourceView) []peerView {
	local := make(map[string]bool, len(resources))
	for _, r := range resources {
		local[canonicalTaskUID(r.Identity)] = true
	}
	return slices.DeleteFunc(peers, func(p peerView) bool { return local[p.UID] })
}

type taskListRequest struct {
	Native     bool
	Host       string
	Observed   bool
	ShellsOnly bool
}

func (a *daemonActivity) listTasks(ctx context.Context, request taskListRequest) inventory {
	query := taskInventoryRequest{Native: request.Native, Hosted: request.Host == "", Host: request.Host, Detail: "inventory"}
	if request.ShellsOnly {
		query.Detail = "health"
	} else {
		query.Resources, query.Status = true, true
	}
	inv := managedInventory(ctx, a.tasks, query)
	if request.Host != "" {
		inv.Host = request.Host
	} else {
		if request.Native {
			inv.Host = "local"
			for i := range inv.Shells {
				inv.Shells[i].Host = "local"
			}
		}
		if !request.ShellsOnly {
			inv.Peers = excludeLocalResourcePeers(peerViews(a.incomingPeers()), inv.Resources)
		}
	}
	if request.Observed || request.Host != "" {
		reply := a.handleMetadata(metadataRequest{Action: "participation_get", Host: request.Host})
		if reply.Error != "" {
			inv.Errors = append(inv.Errors, "remote shell history unavailable: "+cleanText(reply.Error))
		}
		for _, row := range reply.Participants {
			inv.Shells = append(inv.Shells, shellView{Host: row.Host, Owner: row.Owner, Instance: row.Instance, ID: "sh/" + row.Name, Name: row.Name, State: row.State, Observed: true, SeenAt: row.ChangedAt, Pinned: row.Pinned, PinUntil: row.PinUntil, PID: row.PID})
		}
	}
	inv.setTaskReferences()
	if request.Native && request.Observed && request.Host == "" {
		inv.Tree = normalizeTaskTree(localShellTreeNodes(inv))
	}
	return inv
}

func daemonListTasks(request taskListRequest) (inventory, error) {
	if activity := accountActivity.Load(); activity != nil {
		return activity.listTasks(context.Background(), request), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
	defer cancel()
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		root, pathErr := statePath("")
		if pathErr != nil {
			return emptyInventory(), pathErr
		}
		if _, statErr := os.Stat(root); os.IsNotExist(statErr) || !activityAllowed() {
			return emptyInventory(), nil
		}
		record, err = ensureLocalDaemonContext(ctx)
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

func collectInventory(shellsOnly bool) (inventory, error) {
	inv, err := daemonListTasks(taskListRequest{Native: true, Observed: true, ShellsOnly: shellsOnly})
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

func runInventoryCommand(command string, args []string) (int, error) {
	parsed, err := parseCLI(args, command)
	if err != nil {
		return 2, err
	}
	current, color := shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout)
	inv, err := collectInventory(command == "pstree")
	if parsed.has("--json") {
		if err := json.NewEncoder(os.Stdout).Encode(inv); err != nil {
			return 1, err
		}
	} else {
		width, _, _ := term.GetSize(int(os.Stdout.Fd()))
		writeInventory(os.Stdout, command, inv, current, color, width)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

type taskTableRow struct {
	uid, code, alias, kind, owner, state, parent, detail string
	current                                              bool
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
		selected := current != "" && s.Identity == current && s.State == taskActive
		detail := shellListDetail(s.Directory, s.SourceHost)
		detail = pinDetail(detail, s.Pinned, s.PinUntil)
		rows = append(rows, taskTableRow{taskUID(s.Identity), s.ShortID, s.Name, "sh", "local", s.State, taskShortCode(s.Owner), detail, selected})
	}
	now := time.Now()
	for _, s := range inv.Shells {
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		selected := !s.Observed && current != "" && identityFromUID(s.Instance) == current && s.State == taskActive
		detail := s.Directory
		if s.Observed {
			seen := "on " + host
			if !s.SeenAt.IsZero() {
				seen += " · seen " + observationAge(s.SeenAt, now)
			}
			detail = joinTaskDetails(seen, detail)
		}
		detail = shellListDetail(detail, s.SourceHost)
		detail = pinDetail(detail, s.Pinned, s.PinUntil)
		rows = append(rows, taskTableRow{taskUID(identityFromUID(s.Instance)), s.ShortID, s.Name, "sh", host, s.State, taskShortCode(s.Owner), detail, selected})
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
		if len(inv.Errors) > 0 {
			return
		}
		if inv.Host != "" && inv.Host != "local" {
			fmt.Fprintf(out, "No tasks for %s.\n", cleanText(inv.Host))
		} else {
			fmt.Fprintln(out, "No tasks. Create a local shell with sshore shell.")
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

func shellListDetail(directory, source string) string {
	if source == "" {
		return directory
	}
	return joinTaskDetails("from "+source, directory)
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
			widths[j] = max(widths[j], lipgloss.Width(cell))
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
				padding = widths[j] - lipgloss.Width(cell) + 2
			}
			if j == last {
				cell = compactTaskDetail(cell, remaining)
			}
			if color && i > 0 && i != currentRow && j == taskStateColumn {
				cell = taskStateStyle(lipgloss.NewStyle(), cell).Render(cell)
			}
			line.WriteString(cell)
			line.WriteString(strings.Repeat(" ", padding))
		}
		rendered := termui.Render(color && i == 0, termui.Heading, line.String())
		fmt.Fprintln(out, termui.Render(color && i == currentRow, termui.Accent, rendered))
	}
}

func taskStateStyle(style lipgloss.Style, state string) lipgloss.Style {
	switch state {
	case taskActive:
		return style.Foreground(ansi.Green)
	case taskUnknown, taskExited:
		return style.Foreground(ansi.Yellow)
	default:
		return style
	}
}

// writeStyledTaskTable keeps compact output for narrow terminals and renders a
// bordered table only when the task ID and state fit without truncation.
func writeStyledTaskTable(out io.Writer, rows [][]string, columns, remaining int, color bool, screenWidth, currentRow int) bool {
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
		if color && row+1 == currentRow {
			return cell.Foreground(ansi.Cyan)
		}
		if color && col == taskStateColumn {
			return taskStateStyle(cell, visible[row+1][col])
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
	return "peer-task:" + p.UID
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
	Identity     string    `json:"identity"`
	Code         string    `json:"short_id"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Directory    string    `json:"directory,omitempty"`
	MissingOwner string    `json:"last_known_parent,omitempty"`
	Remote       string    `json:"remote,omitempty"`
	Direction    string    `json:"direction,omitempty"`
	Owner        int       `json:"parent_row,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	PinUntil     time.Time `json:"pin_until,omitzero"`
	SeenAt       time.Time `json:"seen_at,omitzero"`
}

func writeInventory(out io.Writer, command string, inv inventory, current string, color bool, width int) {
	if command == "pstree" {
		if len(inv.Tree) != 0 || len(inv.Errors) == 0 {
			writeShellTree(out, inv.Tree, current, color)
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

	rows := make([]taskTreeRow, 0, len(nodes))
	var appendRows func([]int, int)
	appendRows = func(siblings []int, owner int) {
		for _, i := range siblings {
			n := nodes[i]
			row := taskTreeRow{Directory: n.Directory, SeenAt: n.SeenAt, Identity: n.Identity, Code: n.displayCode(), Name: n.Name, State: n.State, Remote: n.Remote, Owner: owner, Pinned: n.Pinned, PinUntil: n.PinUntil}
			if n.Remote != "" {
				row.Direction = n.Direction
			}
			if owner == 0 && n.Owner != "" {
				row.MissingOwner = taskShortCode(n.Owner)
			}
			rows = append(rows, row)
			appendRows(children[i+1], len(rows))
		}
	}
	appendRows(children[0], 0)
	return rows
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
func localShellTreeNodes(inv inventory) []shellTreeNode {
	nodes := make([]shellTreeNode, 0, len(inv.LocalShells)+len(inv.Shells))
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
		add(shellTreeNode{Identity: s.Identity, ShortID: s.ShortID, Owner: s.Owner, Name: s.Name, State: s.State, Directory: s.Directory,
			Remote: s.SourceHost, Direction: "<-", Pinned: s.Pinned, PinUntil: s.PinUntil})
	}
	for _, s := range inv.Shells {
		n := shellTreeNode{Identity: identityFromUID(s.Instance), ShortID: s.ShortID, Owner: s.Owner, Name: s.Name, State: s.State,
			Directory: s.Directory, Remote: s.SourceHost, Direction: "<-", Pinned: s.Pinned, PinUntil: s.PinUntil}
		if s.Observed {
			n.Remote, n.Direction, n.SeenAt = s.Host, "->", s.SeenAt
		}
		add(n)
	}
	return nodes
}

type shellTreeNode struct {
	Identity, ShortID, Owner, Name, State string
	Directory, Remote, Direction          string
	Pinned                                bool
	PinUntil, SeenAt                      time.Time
}

func writeShellTree(out io.Writer, rows []taskTreeRow, current string, color bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No shells.")
		return
	}
	branchStyle := lipgloss.NewStyle().PaddingRight(1)
	if color {
		branchStyle = branchStyle.Foreground(ansi.BrightBlack)
	}
	trees := make([]*tree.Tree, len(rows))
	var roots []*tree.Tree
	for i, row := range rows {
		selected := current != "" && row.Identity == current
		t := tree.Root(shellTreeLabel(row, selected, color)).Enumerator(tree.RoundedEnumerator).
			EnumeratorStyle(branchStyle).IndenterStyle(branchStyle)
		trees[i] = t
		if row.Owner > 0 && row.Owner <= i {
			trees[row.Owner-1].Child(t)
		} else {
			roots = append(roots, t)
		}
	}
	for _, root := range roots {
		fmt.Fprintln(out, root)
	}
}

func shellTreeLabel(n taskTreeRow, current, color bool) string {
	label := cleanText(n.Code)
	if n.Name != "" && n.Name != n.Code {
		label = cleanText(n.Name) + " (" + label + ")"
	}
	if n.Remote != "" && n.Direction == "->" {
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
