package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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

const inventoryStateHelp = `Task states
  R  Attached session or registered resource
  S  Waiting for terminal control
  T  Detached session (can keep running)
  ?  Unknown

Starting and exited tasks are omitted.
`

type resourceView struct {
	ShortID  string `json:"short_id,omitempty"`
	Identity string `json:"identity,omitempty"`
	ID       string `json:"id"`
	Host     string `json:"host"`
	Detail   string `json:"detail"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func (r resourceView) kindAndName() (string, string) {
	kind, name, _ := strings.Cut(r.ID, "/")
	return kind, name
}

type shellView struct {
	HostInfo    hostInfo  `json:"host_info,omitzero"`
	SourceHost  string    `json:"source_host,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	PinUntil    time.Time `json:"pin_until,omitzero"`
	Instance    string    `json:"instance,omitempty"`
	ShortID     string    `json:"short_id,omitempty"`
	Host        string    `json:"host,omitempty"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Observed    bool      `json:"observed,omitempty"`
	SeenAt      time.Time `json:"seen_at,omitzero"`
	PID         int       `json:"pid"`
	Shell       string    `json:"shell,omitempty"`
	Program     bool      `json:"program,omitempty"`
	ProgramLine string    `json:"program_line,omitempty"`
	Directory   string    `json:"directory"`
}
type peerView struct {
	UID    string `json:"uid"`
	ID     string `json:"id"`
	Detail string `json:"detail"`
}
type inventory struct {
	LocalHost   *hostInfo        `json:"local_host,omitempty"`
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
	v := resourceView{Identity: resourceIdentity(r), ID: r.Kind + "/" + r.Name, Host: r.Host, Detail: r.Detail}
	if r.Kind == "mount" {
		v.ReadOnly = r.ReadOnly
		v.Detail = sshio.FormatEndpoint(r.Host, r.Remote) + " -> " + r.Mountpoint
	}
	return v
}

// Read only registered tasks here. Historical and peer observations are
// collected separately and cannot become part of this snapshot.
func readTaskInventory(request taskInventoryRequest) (taskInventorySnapshot, error) {
	var snapshot taskInventorySnapshot
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err == nil {
		err = daemonJSON(context.Background(), record, "/tasks/snapshot", request, &snapshot)
	}
	return snapshot, err
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

func (a *daemonActivity) listTasks() inventory {
	query := taskInventoryRequest{Native: true, Hosted: true, Resources: true}
	snapshot := a.tasks.taskInventory(query)
	inv := emptyInventory()
	inv.LocalHost = a.hostInfo.Load()
	inv.Shells = append(inv.Shells, snapshot.Shells...)
	inv.LocalShells = snapshot.nativeShells()
	inv.Resources = snapshot.Resources
	inv.Peers = excludeLocalResourcePeers(peerViews(a.incomingPeers()), inv.Resources)
	reply := a.handleMetadata(metadataRequest{Action: "participation_get"})
	if reply.Error != "" {
		inv.Errors = append(inv.Errors, "remote shell history unavailable: "+cleanText(reply.Error))
	}
	for _, row := range reply.Participants {
		inv.Shells = append(inv.Shells, shellView{HostInfo: row.HostInfo, Host: row.Host, Owner: row.Owner, Instance: row.Instance, Name: row.Name, State: row.State, Observed: true, SeenAt: row.ChangedAt, Pinned: row.Pinned, PinUntil: row.PinUntil, PID: row.PID, Shell: row.Shell, Program: row.Program})
	}
	inv.setTaskReferences()
	return inv
}

func daemonListTasks() (inventory, error) {
	if activity := accountActivity.Load(); activity != nil {
		return activity.listTasks(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
	defer cancel()
	record, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return emptyInventory(), nil
	}
	if err != nil {
		return emptyInventory(), fmt.Errorf("open local task inventory: %w", err)
	}
	var inv inventory
	if err := daemonJSON(ctx, record, "/tasks/list", struct{}{}, &inv); err != nil {
		return emptyInventory(), fmt.Errorf("read local task inventory: %w", err)
	}
	return inv, nil
}

func collectInventory() (inventory, error) {
	inv, err := daemonListTasks()
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
}

func runInventoryCommand(parsed cliArguments) (int, error) {
	command := parsed.command
	current, color := shellIdentityFromEnvironment(), termui.ColorEnabled(os.Stdout)
	inv, err := collectInventory()
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	writeInventory(os.Stdout, command, inv, current, color, width)
	if err != nil {
		return 1, err
	}
	return 0, nil
}

const localInventoryHost = "\x00local"
const incomingInventoryHost = "\x00incoming"

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
	hostInfos := map[string]hostInfo{}
	for _, s := range inv.LocalShells {
		selected := current != "" && s.Identity == current && s.State == taskActive
		detail := shellListDetail(joinTaskDetails(cmp.Or(s.ProgramLine, s.Shell), s.Directory), s.SourceHost)
		detail = pinDetail(detail, s.Pinned, s.PinUntil)
		rows = append(rows, taskTableRow{taskUID(s.Identity), s.ShortID, s.Name, terminalTaskKind(s.Program), localInventoryHost, s.State, taskShortCode(s.Owner), detail, selected})
	}
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

func terminalTaskKind(program bool) string {
	if program {
		return "run"
	}
	return "shell"
}

func terminalTreeName(program bool, name, command string) string {
	if program && isShortTaskID(name) {
		return command
	}
	return name
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

func taskHiddenFromInventory(state string) bool {
	return state == taskStarting || state == taskExited
}

func taskStateCode(state string) string {
	switch state {
	case taskActive:
		return "R"
	case taskWaiting:
		return "S"
	case taskDetached:
		return "T"
	case taskUnknown:
		return "?"
	default:
		return state
	}
}

func peerTaskIdentity(p peerView) string {
	return "peer-task:" + p.UID
}

func peerKind(name string) string {
	kind, _, _ := strings.Cut(name, "/")
	return kind
}

func taskDisplayID(code, alias string, current bool) string {
	code = cleanText(code)
	if code == "" {
		code = "-"
	}
	suffix := ""
	if alias != "" && alias != code {
		suffix = " (" + cleanText(alias) + ")"
	}
	if current {
		code += "*"
	}
	return code + suffix
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
		host := cmp.Or(nodes[i].Host, localInventoryHost)
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
			host := cmp.Or(n.Host, localInventoryHost)
			if host != parentHost {
				row.Remote = hostDisplayLabel(host)
			}
			rows = append(rows, row)
			appendRows(children[i+1], len(rows), host)
		}
	}
	for _, host := range hosts {
		rows = append(rows, taskTreeRow{Code: hostDisplayLabel(host)})
		appendRows(byHost[host], len(rows), host)
	}
	return rows
}

func hostDisplayLabel(host string) string {
	if host == localInventoryHost {
		return "local"
	}
	if host == incomingInventoryHost {
		return "incoming"
	}
	return cleanText(host)
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

func compareInventoryHosts(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == localInventoryHost:
		return -1
	case b == localInventoryHost:
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
		if taskHiddenFromInventory(n.State) {
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
		add(shellTreeNode{Kind: terminalTaskKind(s.Program), Identity: s.Identity, ShortID: s.ShortID, Owner: s.Owner, Name: terminalTreeName(s.Program, s.Name, s.Shell), State: s.State})
	}
	for _, s := range inv.Shells {
		n := shellTreeNode{Kind: terminalTaskKind(s.Program), Identity: identityFromUID(s.Instance), ShortID: s.ShortID, Owner: s.Owner, Name: terminalTreeName(s.Program, s.Name, s.Shell), State: s.State}
		if s.Observed {
			n.Host, n.SeenAt = s.Host, s.SeenAt
		}
		add(n)
	}
	for _, r := range inv.Resources {
		kind, name := r.kindAndName()
		add(shellTreeNode{Kind: kind, Identity: r.Identity, ShortID: r.ShortID, Name: name, State: taskActive})
	}
	for _, p := range inv.Peers {
		kind, name, _ := strings.Cut(p.ID, "/")
		identity := peerTaskIdentity(p)
		add(shellTreeNode{Kind: kind, Identity: identity, ShortID: taskShortCode(identity), Host: incomingInventoryHost, Name: name, State: taskActive})
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
	writeBranch = func(i int, prefix, indent string) {
		row := rows[i]
		fmt.Fprintln(out, prefix+shellTreeLabel(row, current != "" && row.Identity == current, color))
		kids := children[i+1]
		for j, child := range kids {
			prefix, childIndent := termui.TreeBranch(color, indent, j == len(kids)-1)
			writeBranch(child, prefix, childIndent)
		}
	}
	for _, root := range children[0] {
		writeBranch(root, "", "")
	}
}

func shellTreeLabel(n taskTreeRow, current, color bool) string {
	label := taskDisplayID(n.Code, n.Name, current)
	if n.Kind != "" {
		label = cleanText(n.Kind) + "/" + label
	}
	if n.Remote != "" {
		label += " on " + cleanText(n.Remote)
	}

	state := cleanText(taskStateCode(n.State))
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
	if uid := os.Getenv("SSHORE_SOCKET"); uid != "" {
		return identityFromUID(uid)
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
