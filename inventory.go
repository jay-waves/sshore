package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/jay-waves/sshore/internal/proc"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	"golang.org/x/term"
)

const (
	taskActive   = "active"
	taskDetached = "detached"
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
	MachineID   string    `json:"machine_id,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	Lifetime    string    `json:"lifetime"`
	Temporary   bool      `json:"temporary,omitempty"`
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
	MachineID     string           `json:"machine_id,omitempty"`
	Hostname      string           `json:"hostname,omitempty"`
	ShellTree     []shellTreeNode  `json:"shell_tree,omitempty"`
	LocalShells   []localShellView `json:"local_shells,omitempty"`
	Version       string           `json:"version"`
	Host          string           `json:"host,omitempty"`
	RemoteQueried bool             `json:"remote_queried"` // Legacy JSON key. Inventory reads the local daemon.
	Resources     []resourceView   `json:"resources"`
	Shells        []shellView      `json:"shells"`
	Peers         []peerView       `json:"peers"`
	Errors        []string         `json:"errors"`
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
		v.Detail = r.Mountpoint + " -> " + r.Remote
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
	rows := make([]localShellView, 0, len(snapshot.Native))
	for identity, view := range snapshot.Native {
		view.identity, view.ShortID = identity, taskShortCode(identity)
		rows = append(rows, view)
	}
	sortLocalShells(rows)
	return rows
}

func managedInventory(ctx context.Context, tasks *daemonTasks, detail string, includeNative bool, host string) inventory {
	inv := emptyInventory()
	if runtime.GOOS == "linux" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	request := taskInventoryRequest{Native: includeNative, Hosted: host == "", Resources: true, Status: true, Detail: detail, Host: host}
	snapshot, workers := tasks.taskInventory(request)
	statuses := make(map[string]resourceStatus)
	ctx, cancel := context.WithTimeout(ctx, resourceStatusTimeout)
	defer cancel()
	probeResourceStatus(ctx, workers, func(row resourceStatusRow) bool {
		statuses[row.UID] = row.Status
		return true
	})
	inv.Shells = append(inv.Shells, snapshot.Shells...)
	if includeNative {
		inv.LocalShells = snapshot.nativeShells()
	}
	inv.Resources = resourceViews(snapshot.Resources, statuses, true)
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
	Native   bool
	Host     string
	Observed bool
}

func (a *daemonActivity) listTasks(ctx context.Context, request taskListRequest) inventory {
	inv := managedInventory(ctx, a.tasks, "inventory", request.Native, request.Host)
	if request.Host != "" {
		inv.Host = request.Host
	} else {
		if request.Native {
			inv.Host = "local"
			for i := range inv.Shells {
				inv.Shells[i].Host = "local"
			}
		}
		inv.Peers = excludeLocalResourcePeers(peerViews(a.incomingPeers()), inv.Resources)
	}
	if request.Observed || request.Host != "" {
		reply := a.handleMetadata(metadataRequest{Action: "participation_get", Host: request.Host})
		if reply.Error != "" {
			inv.Errors = append(inv.Errors, cleanText(reply.Error))
		}
		for _, row := range reply.Participants {
			inv.Shells = append(inv.Shells, shellView{Host: row.Host, Instance: row.Instance, ID: shellTaskName(row.Name, row.Temporary), Name: row.Name, State: row.State, Observed: true, SeenAt: row.ChangedAt, Temporary: row.Temporary, Lifetime: row.Lifetime, PID: row.PID})
		}
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
		return emptyInventory(), err
	}
	var inv inventory
	err = daemonJSON(ctx, record, "/tasks/list", request, &inv)
	return inv, err
}

func collectInventory() (inventory, error) {
	inv, err := daemonListTasks(taskListRequest{Native: true, Observed: true})
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
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
	if command == "pstree" {
		rows, err := readTaskTree()
		writeShellTree(os.Stdout, rows, current, color)
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	inv, err := collectInventory()
	if parsed.has("--json") {
		if err := json.NewEncoder(os.Stdout).Encode(inv); err != nil {
			return 1, err
		}
	} else {
		writeTaskInventoryColor(os.Stdout, inv, current, color)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

type taskTableRow struct {
	uid, code, alias, kind, owner, target, state, detail string
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
		selected := current != "" && s.identity == current && s.State == taskActive
		detail := s.Directory
		if s.ConnectedHost != "" {
			detail = strings.TrimSpace(detail + "  -> " + cleanText(s.ConnectedHost))
		}
		rows = append(rows, taskTableRow{taskUID(s.identity), s.ShortID, s.Name, shellTaskKind(s.Temporary), "local", "-", s.State, detail, selected})
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
			detail = strings.TrimSpace(detail + "  " + seenAgo(s.SeenAt, now))
		}
		rows = append(rows, taskTableRow{taskUID(identityFromUID(s.Instance)), s.ShortID, s.Name, shellTaskKind(s.Temporary), host, "-", s.State, detail, selected})
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		host := r.Host
		if host == "" {
			host = "-"
		}
		rows = append(rows, taskTableRow{taskUID(r.Identity), r.ShortID, name, kind, "local", host, r.State, resourceListDetail(r), false})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		target := inv.Host
		if target == "" {
			target = "local"
		}
		rows = append(rows, taskTableRow{taskUID(peerTaskIdentity(p)), p.ShortID, name, kind, p.Manager, target, p.State, readOnlyDetail(p.Detail), false})
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
		currentRow := -1
		for _, r := range rows[start:end] {
			code := r.code
			if code == "" {
				code = "-"
			}
			cells := []string{taskDisplayID(code, r.alias), r.kind, r.target, r.state, r.detail}
			if r.current {
				currentRow = len(table)
			}
			table = append(table, cells)
		}
		writeTaskTable(out, table, color, screenWidth, currentRow)
		start = end
	}
}

func readOnlyDetail(detail string) string {
	if detail == "" {
		return "read-only"
	}
	return "read-only · " + detail
}

func resourceListDetail(r resourceView) string {
	detail := r.Detail
	if r.LastError != "" {
		detail = r.LastError
	}
	if r.ReadOnly {
		detail = readOnlyDetail(detail)
	}
	return detail
}

func seenAgo(seen, now time.Time) string {
	if seen.IsZero() {
		return ""
	}
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
		return "seen just now"
	}
	count := age / unit
	if count != 1 {
		name += "s"
	}
	return fmt.Sprintf("seen %d %s ago", count, name)
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
			if color && i > 0 && i != currentRow && j == 3 {
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
		if color && col == 3 {
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

// A snapshot lists parents before children. Owner is the parent's one-based
// row index, or zero for a root. It carries display fields only.
type taskTreeRow struct {
	Identity, Code, Name, Kind string
	State                      string
	Remote, Direction          string          `json:",omitempty"`
	PID, Owner                 int             `json:",omitempty"`
	Handoffs                   []handoffTarget `json:",omitempty"`
	SeenAt                     time.Time       `json:",omitzero"`
}

type handoffTarget struct {
	Host, Kind, Code string
}

func (a *daemonActivity) taskTreeSnapshot(ctx context.Context) ([]taskTreeRow, error) {
	// Copy each source independently before probing. Metadata supplies context
	// for registered rows, and leases supply the incoming rows.
	tree, err := a.metadata.knownShellTrees()
	participation := a.handleMetadata(metadataRequest{Action: "participation_get"})
	peers := peerViews(a.incomingPeers())
	inv := managedInventory(ctx, a.tasks, "health", true, "")
	if inv.Hostname == "" {
		inv.MachineID, inv.Hostname = localMachineIdentity()
	}
	inv.Host = inv.Hostname
	inv.ShellTree = tree
	if err != nil {
		inv.Errors = append(inv.Errors, cleanText(err.Error()))
	}
	if participation.Error != "" {
		inv.Errors = append(inv.Errors, cleanText(participation.Error))
	}
	inv.Peers = peers
	nodes := inventoryResourceTrees(localShellTreeNodes(inv), inv)
	rows := normalizeTaskTree(nodes, inv.MachineID)
	return addRemoteShells(rows, inv.LocalShells, participation.Participants), inv.incompleteError()
}

func addRemoteShells(rows []taskTreeRow, local []localShellView, remote []participatingShell) []taskTreeRow {
	attached := make(map[string]handoffTarget)
	var detached []participatingShell
	for _, shell := range remote {
		switch shell.State {
		case taskActive:
			attached[shell.Attachment] = handoffTarget{
				Host: shell.Host, Kind: shellTaskKind(shell.Temporary),
				Code: taskShortCode(identityFromUID(shell.Instance)),
			}
		case taskDetached:
			detached = append(detached, shell)
		}
	}
	byIdentity := make(map[string]*taskTreeRow, len(rows))
	for i := range rows {
		byIdentity[rows[i].Identity] = &rows[i]
	}
	for _, shell := range local {
		row := byIdentity[shell.identity]
		if row == nil {
			continue
		}
		for _, id := range shell.handoffAttachments {
			if target, ok := attached[id]; ok {
				row.Handoffs = append(row.Handoffs, target)
			}
		}
		slices.SortFunc(row.Handoffs, func(a, b handoffTarget) int {
			return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Code, b.Code))
		})
	}
	slices.SortStableFunc(detached, func(a, b participatingShell) int {
		return b.ChangedAt.Compare(a.ChangedAt)
	})
	for _, shell := range detached {
		rows = append(rows, taskTreeRow{
			Code: taskShortCode(identityFromUID(shell.Instance)), Name: shell.Name,
			Kind: shellTaskKind(shell.Temporary), State: shell.State, Remote: shell.Host, Direction: "->", SeenAt: shell.ChangedAt,
		})
	}
	return rows
}

func normalizeTaskTree(nodes []shellTreeNode, machine string) []taskTreeRow {
	nodes, owners := shellTreeOwners(nodes, machine)
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
			row := taskTreeRow{Identity: n.Identity, Code: n.displayCode(), Name: n.Name, Kind: n.Kind, State: n.State, Remote: n.Remote, Direction: n.Direction, Owner: owner}
			if row.Kind == "" {
				row.Kind = shellTaskKind(n.Lifetime == "parent" || n.Lifetime == "heartbeat")
			}
			if row.Kind == "external" && n.OwnerProcess != nil {
				row.PID = n.OwnerProcess.PID
			}
			rows = append(rows, row)
			appendRows(children[i+1], len(rows))
		}
	}
	appendRows(children[0], 0)
	return rows
}

// Build display edges directly. Synthetic parents share the lookup with
// registered tasks and never change a shell's saved owner.
func shellTreeOwners(nodes []shellTreeNode, machine string) ([]shellTreeNode, []int) {
	byID := make(map[string]int, len(nodes))
	for i, n := range nodes {
		if n.Identity != "" {
			byID[n.Identity] = i
		}
	}
	owners := make([]int, len(nodes))
	for i, n := range nodes {
		owner, found := byID[n.Owner]
		if !found {
			p := n.OwnerProcess
			spawn := n.Kind == "spawn" || n.Kind == "" && (n.Lifetime == "parent" || n.Lifetime == "heartbeat")
			if spawn && validSpawnProcessView(p) {
				id := fmt.Sprintf("external:%s:%d:%s", p.MachineID, p.PID, p.Start)
				owner, found = byID[id]
				if !found {
					owner, found = len(nodes), true
					byID[id] = owner
					nodes = append(nodes, externalSpawnNode(id, machine, p))
					owners = append(owners, -1)
				}
			}
		}
		if !found || owner == i {
			owner = -1
		}
		owners[i] = owner
	}
	return nodes, owners
}

func (n shellTreeNode) displayCode() string {
	if n.ShortID != "" {
		return n.ShortID
	}
	return taskShortCode(n.Identity)
}

// Only registered local shells become rows. Saved snapshots contribute
// relationship metadata and can supply the source of an incoming attachment.
func localShellTreeNodes(inv inventory) []shellTreeNode {
	saved := make(map[string]shellTreeNode, len(inv.ShellTree))
	for _, n := range inv.ShellTree {
		saved[n.Identity] = n
	}
	visible := make(map[string]int)
	count := len(inv.LocalShells) + len(inv.Shells)
	nodes := make([]shellTreeNode, 0, count)
	now := time.Now()
	add := func(n shellTreeNode) {
		if n.State == taskExited {
			return
		}
		old := saved[n.Identity]
		n.Cached = n.State == taskUnknown
		if n.Cached {
			if old.State == taskExited {
				return
			}
			n.CheckedAt = old.CheckedAt
		} else {
			n.CheckedAt = now
		}
		n.Owner, n.OwnerProcess = old.Owner, old.OwnerProcess
		if n.State == taskActive {
			n.Parent = old.Parent
			if old.SourceHost != "" && validMachineID(old.SourceMachineID) && old.SourceMachineID != inv.MachineID {
				n.Remote, n.Direction = old.SourceHost, "<-"
			}
		}
		if n.MachineID == "" && old.MachineID != "" {
			n.MachineID, n.Host = old.MachineID, old.Host
		}
		if i, ok := visible[n.Identity]; ok {
			if n.strongerEvidenceThan(nodes[i]) {
				nodes[i] = n
			}
			return
		}
		visible[n.Identity] = len(nodes)
		nodes = append(nodes, n)
	}
	for _, s := range inv.LocalShells {
		add(shellTreeNode{Identity: s.identity, ShortID: s.ShortID, Host: inv.Hostname, MachineID: inv.MachineID, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, false)})
	}
	for _, s := range inv.Shells {
		host := inv.Hostname
		if host == "" {
			host = s.Host
			if host == "" {
				host = inv.Host
			}
		}
		code := s.ShortID
		if code == "" {
			code = taskShortCode(identityFromUID(s.Instance))
		}
		add(shellTreeNode{Identity: identityFromUID(s.Instance), ShortID: code, Host: host, MachineID: inv.MachineID, Name: s.Name, State: s.State, Lifetime: shellLifetime(s.Temporary, true)})
	}
	return nodes
}

func inventoryResourceTrees(nodes []shellTreeNode, inv inventory) []shellTreeNode {
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		nodes = append(nodes, shellTreeNode{Identity: r.Identity, ShortID: r.ShortID, Kind: kind, State: r.State, Name: name, Remote: r.Host, Direction: r.Direction})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(peerResourceID(p.ID), "/")
		direction := "->"
		if p.Direction == "->" {
			direction = "<-"
		}
		nodes = append(nodes, shellTreeNode{Identity: peerTaskIdentity(p), ShortID: p.ShortID, Kind: kind, Name: name, Remote: p.Manager, Direction: direction})
	}

	return nodes
}

// Shell identities are opaque. Parent is the current attachment source; Owner
// is the original managed spawn context. Neither is OS process ancestry.
type shellTreeNode struct {
	OwnerProcess                                  *spawnProcessView `json:",omitempty"`
	Identity, Parent, Owner, Lifetime, Host, Name string
	State                                         string
	Cached                                        bool
	ShortID, Kind                                 string    `json:",omitempty"`
	SourceHost, SourceMachineID                   string    `json:",omitempty"`
	Remote, Direction                             string    `json:"-"`
	MachineID                                     string    `json:",omitempty"`
	CheckedAt                                     time.Time `json:",omitempty"`
}

// Display context only: this process is never registered or adopted as a task.
type spawnProcessView struct {
	PID                          int
	Start, Name, MachineID, Host string
}

func validSpawnProcessView(p *spawnProcessView) bool {
	return p != nil && p.PID > 1 && p.Start != "" && len(p.Start) <= 128 && len(p.Name) <= 256 && len(p.Host) <= 256 && validMachineID(p.MachineID)
}

func describeSpawnOwner(owner processIdentity) *spawnProcessView {
	if owner.PID <= 1 || owner.Start == "" {
		return nil
	}
	machine, host := localMachineIdentity()
	p := &spawnProcessView{PID: owner.PID, Start: owner.Start, MachineID: machine, Host: host}
	p.Name = proc.VerifiedName(p.PID, p.Start)
	return p
}

func externalSpawnNode(id, machine string, p *spawnProcessView) shellTreeNode {
	name, state := "unavailable", taskUnknown
	if p.MachineID == machine {
		if current := proc.VerifiedName(p.PID, p.Start); current != "" {
			name, state = current, taskActive
		}
	} else {
		// Never query a foreign PID in the local process namespace.
		if p.Name != "" {
			name = p.Name
		}
		name += " on " + p.Host
	}
	return shellTreeNode{Identity: id, Kind: "external", Name: name, State: state, OwnerProcess: p}
}

func (n shellTreeNode) clone() shellTreeNode {
	if n.OwnerProcess != nil {
		owner := *n.OwnerProcess
		n.OwnerProcess = &owner
	}
	return n
}

func cloneShellTree(chain []shellTreeNode) []shellTreeNode {
	cloned := slices.Clone(chain)
	for i := range cloned {
		cloned[i] = cloned[i].clone()
	}
	return cloned
}

func writeShellTree(out io.Writer, rows []taskTreeRow, current string, color bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No known tasks.")
		return
	}
	branchStyle := lipgloss.NewStyle().PaddingRight(1)
	if color {
		branchStyle = branchStyle.Foreground(ansi.BrightBlack)
	}
	trees := make([]*tree.Tree, len(rows))
	var roots []*tree.Tree
	now := time.Now()
	for i, row := range rows {
		selected := current != "" && row.Identity == current && row.Kind != "external"
		t := tree.Root(shellTreeLabel(row, selected, color, now)).Enumerator(func(children tree.Children, index int) string {
			return "│\n" + tree.RoundedEnumerator(children, index)
		}).
			EnumeratorStyle(branchStyle).IndenterStyle(branchStyle).
			ItemStyle(lipgloss.NewStyle().PaddingTop(1))
		trees[i] = t
		if row.Owner > 0 && row.Owner <= i {
			trees[row.Owner-1].Child(t)
		} else {
			roots = append(roots, t)
		}
	}
	for i, root := range roots {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, root)
	}
}

func shellTreeLabel(n taskTreeRow, current, color bool, now time.Time) string {
	label := cleanText(n.Kind) + "/" + taskDisplayID(n.Code, n.Name)
	if n.Kind == "external" && n.PID > 0 {
		label = fmt.Sprintf("%s (pid %d)", cleanText(n.Name), n.PID)
	}
	var out strings.Builder
	out.WriteString(label)
	dim := color && !current
	if n.State == taskUnknown {
		out.WriteString(termui.Render(dim, termui.Warning, " ?"))
	}
	if n.Remote != "" {
		detail := treeEndpointLabel(n.Direction, n.Remote, "")
		if n.State == taskDetached {
			detail += "  " + cleanText(n.State)
			if seen := seenAgo(n.SeenAt, now); seen != "" {
				detail += ", " + seen
			}
		}
		out.WriteString(termui.Render(dim, termui.Muted, "  "+detail))
	}
	for _, target := range n.Handoffs {
		link := treeEndpointLabel("->", target.Host, target.Kind+"/"+taskDisplayID(target.Code, ""))
		out.WriteString(termui.Render(dim, termui.Muted, "  "+link))
	}
	if current {
		out.WriteString("  *")
	}
	return termui.Render(color && current, termui.Accent, out.String())
}

func treeEndpointLabel(direction, host, task string) string {
	endpoint := host
	if task != "" {
		endpoint = sshio.FormatEndpoint(host, " "+task)
	}
	return cleanText(direction + " " + endpoint)
}

func readTaskTree() ([]taskTreeRow, error) {
	if activity := accountActivity.Load(); activity != nil {
		return activity.taskTreeSnapshot(context.Background())
	}
	reply, _, err := readTaskInventory(taskInventoryRequest{Detail: "tree"})
	if reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	return reply.Tree, err
}

func shellIdentityFromEnvironment() string {
	if id := os.Getenv("SSHORE_TREE_ID"); id != "" {
		return id
	}
	return os.Getenv("SSHORE_LOCAL_SESSION")
}

func currentShellTree() []shellTreeNode {
	id := os.Getenv("SSHORE_TREE_ID")
	if id == "" {
		return nil
	}
	return shellTreeFor(id)
}

func saveShellTree(id, name string, chain []shellTreeNode, state string, lifetime ...string) {
	saveShellTreeWithProcess(id, name, chain, state, nil, lifetime...)
}
func saveShellTreeWithProcess(id, name string, chain []shellTreeNode, state string, owner *spawnProcessView, lifetime ...string) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_save", Key: id, Name: name, Tree: chain, State: state, Lifetime: lifetime, OwnerProcess: owner})
}

func (m *daemonMetadata) saveShellTreeLocal(id, name string, chain []shellTreeNode, state string, ownerProcess *spawnProcessView, lifetime ...string) error {
	if id == "" || !validShellTreeState(state) || len(chain) >= maxShellTreeNodes {
		return fmt.Errorf("invalid shell tree")
	}
	previous := m.treeChain(id)
	machine, host := localMachineIdentity()
	self := shellTreeNode{Identity: id, ShortID: canonicalTaskCode(id), Host: host, MachineID: machine, CheckedAt: time.Now(), Name: name, State: state}
	if len(previous) > 0 {
		old := previous[len(previous)-1]
		self.Owner, self.Lifetime = old.Owner, old.Lifetime
		self.OwnerProcess = old.OwnerProcess
	}
	// A nil node reserves an identity already present in the outgoing chain.
	contextNodes := map[string]*shellTreeNode{id: nil}
	if state != taskActive {
		chain = nil
	}
	for _, n := range chain {
		if _, included := contextNodes[n.Identity]; included || !n.validAttachmentContext() {
			return fmt.Errorf("invalid shell tree context")
		}
		contextNodes[n.Identity] = nil
	}
	chain = cloneShellTree(chain)
	if len(chain) > 0 {
		self.Parent = chain[len(chain)-1].Identity
	}
	if state == taskActive && len(lifetime) >= 3 && len(lifetime[1]) <= 256 && validMachineID(lifetime[2]) {
		self.SourceHost, self.SourceMachineID = lifetime[1], lifetime[2]
	}

	// Only initial startup establishes lifetime ownership. Attachment metadata
	// remains display-only and can never transfer the actual process/SSH lease.
	if self.Lifetime == "" && len(lifetime) > 0 && validShellLifetime(lifetime[0]) {
		self.Lifetime = lifetime[0]
		if self.Lifetime == "parent" || self.Lifetime == "heartbeat" {
			self.Owner = self.Parent
			if len(previous) == 0 && validSpawnProcessView(ownerProcess) {
				self.OwnerProcess = ownerProcess
				self = self.clone()
			}
		}
	}
	// Keep the original owner's context across detach and attachment elsewhere.
	if _, included := contextNodes[self.Owner]; self.Owner != "" && !included && len(chain) < maxShellTreeNodes-1 {
		for i := len(previous) - 1; i >= 0; i-- {
			candidate := &previous[i]
			if _, included := contextNodes[candidate.Identity]; !included {
				contextNodes[candidate.Identity] = candidate
			}
		}
	}
	for owner := self.Owner; owner != "" && len(chain) < maxShellTreeNodes-1; {
		n := contextNodes[owner]
		if n == nil {
			break
		}
		chain = append(chain, *n)
		delete(contextNodes, owner)
		owner = n.Owner
	}
	for i := range chain {
		chain[i].Cached = true
	}
	chain = append(chain, self)
	return m.writeTreeLocal(chain)
}

func (n shellTreeNode) validAttachmentContext() bool {
	return n.Identity != "" && len(n.Identity) <= 256 && len(n.Parent) <= 256 &&
		len(n.Owner) <= 256 && len(n.Host) <= 256 && len(n.Name) <= 256 &&
		validShellTreeState(n.State) && len(n.SourceHost) <= 256 &&
		(n.MachineID == "" || validMachineID(n.MachineID)) &&
		(n.SourceMachineID == "" || validMachineID(n.SourceMachineID)) &&
		(n.ShortID == "" || isShortTaskID(n.ShortID)) && validShellLifetime(n.Lifetime)
}

func (n shellTreeNode) strongerEvidenceThan(old shellTreeNode) bool {
	ended, oldEnded := n.State == taskExited, old.State == taskExited
	if ended != oldEnded {
		return ended
	}
	if n.Cached != old.Cached {
		return !n.Cached
	}
	return n.CheckedAt.After(old.CheckedAt)
}

func (m *daemonMetadata) knownShellTrees() ([]shellTreeNode, error) {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	if err := m.loadTrees(); err != nil {
		return nil, err
	}
	byID := make(map[string]int, len(m.trees))
	result := make([]shellTreeNode, 0, len(m.trees))
	// Visit files in a fixed order so otherwise equal evidence has a stable tie.
	for _, name := range slices.Sorted(maps.Keys(m.trees)) {
		chain := m.trees[name]
		for _, n := range chain {
			if i, ok := byID[n.Identity]; ok {
				if n.strongerEvidenceThan(result[i]) {
					result[i] = n
				}
				continue
			}
			byID[n.Identity] = len(result)
			result = append(result, n)
		}
	}
	for i := range result {
		result[i] = result[i].clone()
		result[i].Cached = true
	}
	return result, nil
}

func shellTreeFor(id string) []shellTreeNode {
	r, _ := metadataRPC(metadataRequest{Action: "tree_get", Key: id})
	return r.Tree
}
func rememberShellTree(chain []shellTreeNode) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_remember", Tree: chain})
}
func (m *daemonMetadata) rememberShellTreeLocal(chain []shellTreeNode) error {
	if !validSavedShellTree(chain) {
		return fmt.Errorf("invalid shell tree")
	}
	// A local daemon owns its record; the client must not replace it with a cache.
	if own, found := m.treeNode(chain[len(chain)-1].Identity); found && (!own.Cached || own.State == taskExited) {
		return nil
	}
	chain = cloneShellTree(chain)
	for i := range chain {
		chain[i].Cached = true
	}
	return m.writeTreeLocal(chain)
}

func renameShellTree(id, name string) {
	_, _ = metadataRPC(metadataRequest{Action: "tree_rename", Key: id, Name: name})
}
func (m *daemonMetadata) renameShellTreeLocal(id, name string) {
	chain := slices.Clone(m.treeChain(id))
	if len(chain) == 0 {
		return
	}
	chain[len(chain)-1].Name = name
	_ = m.writeTreeLocal(chain)
}

func rememberShellExit(o options) error {
	detachErr := removeRecentDetach(o)
	if o.local || !validTaskUID(o.instance) {
		return detachErr
	}
	return errors.Join(detachErr, recordRemoteShellExit(o.host, o.session, o.instance), forgetParticipatingShell(o.host, o.instance))
}

func recordRemoteShellExit(host, name, instance string) error {
	if !validTaskUID(instance) {
		return fmt.Errorf("invalid remote shell instance")
	}
	_, err := metadataRPC(metadataRequest{Action: "tree_remote_exit", Host: host, Name: name, Instance: instance})
	return err
}

func (m *daemonMetadata) recordRemoteShellTreeExitLocal(host, name, instance string) error {
	if !validTaskUID(instance) {
		return fmt.Errorf("invalid remote shell instance")
	}
	id := identityFromUID(instance)
	self, found := m.treeNode(id)
	if !found {
		if name == "" {
			name = taskShortCode(id)
		}
		self = shellTreeNode{Identity: id, Host: host, Name: name, Cached: true}
	}
	self.State, self.Parent = taskExited, ""
	self.CheckedAt = time.Now()
	return m.writeTreeLocal([]shellTreeNode{self})
}
