package main

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/jay-waves/sshore/internal/termui"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func collectRunningResources(host string) ([]resourceRecord, error) {
	reply, err := daemonResourceRequest(message{Type: "task_list_resources"})
	if err != nil {
		return nil, err
	}
	var records []resourceRecord
	for _, r := range reply.Records {
		if host == "" || r.Host == host {
			records = append(records, r)
		}
	}
	return records, nil
}

func collectLocalResources(host string) ([]resourceRecord, error) {
	records, e := collectRunningResources(host)
	if e != nil {
		return nil, e
	}
	pending, e := pendingRequests()
	if e != nil {
		return nil, e
	}
	for _, r := range pending {
		if host == "" || r.Connection.Host == host {
			records = append(records, resourceRecord{UID: r.ID, Token: r.ID, Kind: "review", Name: r.DisplayName(), Host: r.Connection.Host, Detail: r.Connection.Session + ": " + requestSummary(r.Operations)})
		}
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return records, nil
}

func findRunningResource(kind, name string) (resourceRecord, error) {
	if err := validSession(name); err != nil {
		return resourceRecord{}, err
	}
	rows, err := collectRunningResources("")
	if err != nil {
		return resourceRecord{}, err
	}
	for _, row := range rows {
		if row.Kind == kind && (row.Name == name || row.UID == name) {
			return row, nil
		}
	}
	return resourceRecord{}, fmt.Errorf("%s/%s no longer exists; refresh with sshore list", kind, name)
}

func runRenameResource(kind, old, name string, expected ...string) (int, error) {
	if kind != "mount" && kind != "forward" && kind != "review" {
		return 2, usage("rename expects mount/NAME, forward/NAME or review/NAME")
	}
	if err := validAlias(name); err != nil {
		return 2, err
	}
	if kind == "review" {
		record, err := findRequest(old)
		if err != nil {
			return 1, err
		}
		if len(expected) > 0 && resourceIdentity(resourceRecord{UID: record.ID, Kind: "review", Token: record.ID}) != expected[0] {
			return 1, fmt.Errorf("request changed; refresh with sshore list")
		}
		if record.DisplayName() == name {
			return 0, nil
		}
		if err := renameRequest(record.ID, name); err != nil {
			return 1, err
		}
	} else {
		row, err := findRunningResource(kind, old)
		if err != nil {
			return 1, err
		}
		if len(expected) > 0 && resourceIdentity(row) != expected[0] {
			return 1, fmt.Errorf("task changed; refresh with sshore list")
		}
		if _, err := daemonResourceRequest(message{Type: "task_rename_resource", Instance: row.UID, Name: name}); err != nil {
			return 1, err
		}

	}
	fmt.Printf("Renamed %s/%s to %s/%s.\n", kind, old, kind, name)
	return 0, nil
}

// Resource references resolve through the account daemon before cancellation.
func runCancelResource(kind, name string) (int, error) {
	switch kind {
	case "review":
		record, readErr := findRequest(name)
		if readErr != nil {
			return 1, readErr
		}
		if err := consumeRequest(record.ID); err != nil {
			return 1, err
		}
		removeRejectedPeerTask(record)
		termui.Log(os.Stderr, termui.Success, "[sshore] Request cancelled\n")
		return 0, nil
	case "mount", "forward":
		return 0, stopResource(kind, name)
	default:
		return 2, usage("resource ID must be review/NAME, mount/NAME or forward/NAME")
	}
}

func runCancelAll(o options) (int, error) {
	inv, err := collectInventory(o)
	if err != nil {
		return 1, err
	}
	// Offline shell history and tasks owned by other computers are not targets.
	inv.Peers = nil
	if o.host == "" {
		live := inv.Shells[:0]
		for _, s := range inv.Shells {
			if !s.Historical {
				live = append(live, s)
			}
		}
		inv.Shells = live
	}
	remoteResources := append([]resourceView(nil), inv.Resources...)
	records, err := collectLocalResources(o.host)
	if err != nil {
		return 1, err
	}
	if o.host != "" {
		inv.Resources = append(inv.Resources, inspectResources(records)...)
		if err = numberInventoryResources(&inv); err != nil {
			return 1, err
		}
	}
	if len(inv.LocalShells)+len(inv.Shells)+len(inv.Resources) == 0 {
		if err := stopIdleLocalDaemon(); err != nil {
			return 1, err
		}
		fmt.Println("No tasks to cancel.")
		return 0, nil
	}
	printTaskInventory(inv)
	fmt.Fprint(os.Stderr, "End these shells and tasks, unmount mounts, and stop forwards? Type cancel: ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(answer) != "cancel" {
		return 1, fmt.Errorf("cancelled; no tasks ended")
	}
	var failures []error
	for _, s := range inv.LocalShells {
		_, err := runLocalCommand(options{local: true, host: "local", command: "cancel", session: s.Name, taskIdentity: s.identity})
		if err != nil {
			failures = append(failures, err)
		}
	}
	for _, s := range inv.Shells {
		if s.Instance == "" {
			failures = append(failures, fmt.Errorf("shell %s has no confirmed identity", s.Name))
			continue
		}
		target := o
		target.list, target.command, target.session, target.instance = false, "cancel", s.Name, s.Instance
		cancel := runClient
		if o.host == "" {
			target.local, target.hosted = true, true
			cancel = runLocalCommand
		}
		if _, err := cancel(target); err != nil {
			failures = append(failures, err)
		}
	}
	if o.host != "" {
		for _, r := range remoteResources {
			target := o
			target.list, target.command, target.session, target.taskIdentity = false, "cancel-resource", r.ID, r.Identity
			if _, err := runClient(target); err != nil {
				failures = append(failures, err)
			}
		}
	}

	for _, r := range records {
		if err := cancelResourceIdentity(r.Kind+"/"+r.Name, resourceIdentity(r)); err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return 1, err
	}
	if err := stopIdleLocalDaemon(); err != nil {
		return 1, err
	}
	fmt.Println("Tasks cancelled.")
	return 0, nil
}

func cancelResourceIdentity(id, identity string) error {
	kind, name, ok := strings.Cut(id, "/")
	if !ok || identity == "" {
		return fmt.Errorf("missing resource identity")
	}
	if kind != "review" {
		return stopResource(kind, name, identity)
	}
	record, err := findRequest(name)
	if err != nil {
		return err
	}
	if resourceIdentity(resourceRecord{UID: record.ID, Kind: "review", Token: record.ID}) != identity {
		return fmt.Errorf("request changed; refresh with sshore list")
	}
	if err = consumeRequest(record.ID); err == nil {
		removeRejectedPeerTask(record)
	}
	return err
}

// Status separates local worker liveness from transport state and target health.
// Target services are never probed by listing. Exported views deliberately omit
// control capabilities, addresses, SSH options, credentials and terminal output.

type resourceStatus struct {
	Version       string    `json:"version"`
	Transport     string    `json:"transport"`
	Target        string    `json:"target"`
	Started       time.Time `json:"started,omitempty"`
	LastOperation time.Time `json:"last_operation,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

func (s *resourceService) statusSnapshot() resourceStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	snapshot := s.status
	snapshot.Version, snapshot.Target = version, "unchecked"
	return snapshot
}
func (s *resourceService) setTransport(state string, err error) {
	if s == nil {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status.Transport = state
	if err != nil {
		s.status.LastError = cleanText(err.Error())
	}
	if state == "connected" && s.status.Started.IsZero() {
		s.status.Started = time.Now().UTC()
	}
}
func (s *resourceService) operationCompleted() {
	if s == nil {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status.LastOperation = time.Now().UTC()
}
func (s *resourceService) saveResult(code int, err error) {
	state := "stopped"
	if code != 0 || err != nil {
		state = "failed"
	}
	s.setTransport(state, err)
	// UID-backed resources retain one receipt per instance; legacy slots reuse it.
	r := resourceResult{Record: s.record, DisplayName: s.record.Name, Status: s.statusSnapshot(), Finished: time.Now().UTC()}
	r.Record.Token, r.Record.Address = "", ""
	if writeStateJSON(s.recordPath+".result", r) == nil {
		pruneResourceResults(filepath.Dir(s.recordPath))
	}
}

type resourceResult struct {
	DisplayName string `json:",omitempty"`
	Record      resourceRecord
	Status      resourceStatus
	Finished    time.Time
}

// Retain at most 100 receipts per resource kind for seven days. Active records
// are separate files and are never removed by history cleanup.
func pruneResourceResults(dir string) {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return
	}
	type resultFile struct {
		name string
		at   time.Time
	}
	var files []resultFile
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json.result") {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			continue
		}
		files = append(files, resultFile{entry.Name(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	for i, f := range files {
		if i >= 100 || time.Since(f.at) > 7*24*time.Hour {
			_ = os.Remove(filepath.Join(dir, f.name))
		}
	}
}
