package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func collectLocalResources(host string, withStatus bool) ([]resourceView, error) {
	daemon, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if host != "" {
		query.Set("host", host)
	}
	timeout := daemonRequestTimeout
	if withStatus {
		query.Set("status", "1")
		timeout = resourceStatusTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := daemonRequest(ctx, daemon, "/resources?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	response, err := daemonDo(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	dec := json.NewDecoder(response.Body)
	var reply resourceReply
	if err := dec.Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, fmt.Errorf("%s", reply.Error)
	}

	views := make([]resourceView, 0, len(reply.Records))
	pending := make(map[string]int)
	for _, record := range reply.Records {
		view := viewRecord(record)
		if withStatus && record.Kind != "review" {
			view.LastError = "local worker unavailable or status deadline exceeded"
			pending[record.UID] = len(views)
		}
		views = append(views, view)
	}
	for len(pending) > 0 {
		var row resourceStatusRow
		if err := dec.Decode(&row); err != nil {
			break
		}
		if index, ok := pending[row.UID]; ok {
			view := &views[index]
			if row.Status.Version != version {
				view.LastError = "incompatible or invalid worker status"
			} else {
				view.Worker, view.resourceStatus = "running", row.Status
			}
			delete(pending, row.UID)
		}
	}
	slices.SortFunc(views, func(a, b resourceView) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.ID, b.ID))
	})
	return views, nil
}

func runRenameResource(kind, old, name string, expected ...string) (int, error) {
	if kind != "mount" && kind != "forward" && kind != "review" {
		return 2, usage("rename expects mount/NAME, forward/NAME or review/NAME")
	}
	if err := validAlias(name); err != nil {
		return 2, err
	}
	change := resourceMutation{Kind: kind, Ref: old, NewName: name}
	if len(expected) > 0 {
		change.Identity = expected[0]
	}
	reply, err := daemonResourceRequest(context.Background(), "/resources/rename", change)
	if err != nil {
		return 1, err
	}
	if !reply.Unchanged {
		fmt.Printf("Renamed %s/%s to %s/%s.\n", kind, old, kind, name)
	}
	return 0, nil
}

func runKillResource(kind, name string) (int, error) {
	switch kind {
	case "review":
		if err := stopResource(kind, name); err != nil {
			return 1, err
		}
		termui.Log(os.Stderr, termui.Success, "[sshore] Request discarded\n")
		return 0, nil
	case "mount", "forward":
		return 0, stopResource(kind, name)
	default:
		return 2, usage("resource ID must be review/NAME, mount/NAME or forward/NAME")
	}
}

func stopResource(kind, name string, expected ...string) error {
	change := resourceMutation{Kind: kind, Ref: name}
	if len(expected) > 0 {
		change.Identity = expected[0]
	}
	reply, err := daemonResourceRequest(context.Background(), "/resources/kill", change)
	if err != nil {
		return err
	}
	if kind == "review" {
		if reply.Request == nil {
			return fmt.Errorf("invalid request cancellation response")
		}
		removeRejectedPeerTask(*reply.Request)
	}
	printStoppedResource(kind, name)
	return nil
}

func printStoppedResource(kind, name string) {
	switch kind {
	case "mount":
		fmt.Fprintf(os.Stdout, "Unmounted mount/%s.\n", name)
	case "forward":
		fmt.Fprintf(os.Stdout, "Closed forward/%s.\n", name)
	}
}

func stopConfirmedResources(ctx context.Context, views []resourceView) error {
	if len(views) == 0 {
		return nil
	}
	changes := make([]resourceMutation, len(views))
	for i, view := range views {
		kind, name, _ := strings.Cut(view.ID, "/")
		changes[i] = resourceMutation{Kind: kind, Ref: name, Identity: view.Identity}
	}
	reply, err := daemonResourceRequest(ctx, "/resources/stop-confirmed", changes)
	if err != nil {
		return err
	}
	if len(reply.Failures) != len(changes) {
		return fmt.Errorf("invalid resource cancellation response")
	}
	var failures []error
	for i, failure := range reply.Failures {
		change := changes[i]
		if failure != "" {
			failures = append(failures, fmt.Errorf("%s/%s: %s", change.Kind, change.Ref, failure))
		} else {
			printStoppedResource(change.Kind, change.Ref)
		}
	}
	return errors.Join(failures...)
}

// Kill-all uses administrative control, independent of normal release matching.
func runKillAll(o options) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snapshot daemonSnapshot
	var stop func() error
	var views []resourceView
	if o.host == "" {
		var err error
		snapshot, err = daemonAdmin(ctx, "snapshot", nil)
		if os.IsNotExist(err) {
			fmt.Println("No account daemon is running.")
			return 0, nil
		}
		if err != nil {
			return 1, err
		}
		stop = func() error { return requestDaemonShutdown(snapshot) }
	} else {
		var err error
		views, err = collectLocalResources(o.host, true)
		if err != nil {
			return 1, err
		}
		o.command, o.list = "daemon-admin", false
		process, err := transportio.Start(ctx, sshConnectionOptions(o), os.Stderr)
		if err != nil {
			return 1, err
		}
		defer process.CloseAndWait()
		decoder := json.NewDecoder(process)
		var reply daemonAdminReply
		if err := decoder.Decode(&reply); err != nil {
			return 1, fmt.Errorf("remote daemon administration unavailable: %w", err)
		}
		if reply.Error != "" {
			return 1, fmt.Errorf("%s", reply.Error)
		}
		snapshot = reply.Snapshot
		stop = func() error {
			if err := json.NewEncoder(process).Encode(snapshot); err != nil {
				return err
			}
			if err := decoder.Decode(&reply); err != nil {
				return err
			}
			if reply.Error != "" {
				return fmt.Errorf("%s", reply.Error)
			}
			err := process.Wait()
			return err
		}
	}
	if err := confirmDaemonShutdown(o.host, snapshot, views); err != nil {
		return 1, err
	}
	if err := stop(); err != nil {
		return 1, err
	}
	if err := stopConfirmedResources(ctx, views); err != nil {
		return 1, fmt.Errorf("remote daemon stopped; local resource cleanup: %w", err)
	}
	fmt.Println("Tasks ended; daemon stopped.")
	return 0, nil
}

func confirmDaemonShutdown(host string, snapshot daemonSnapshot, resources []resourceView) error {
	if len(snapshot.Tasks)+len(resources) == 0 {
		return nil
	}
	if host == "" {
		host = "local"
	}
	printDaemonSnapshot(os.Stdout, host, snapshot)
	if len(resources) > 0 {
		inv := inventory{Resources: slices.Clone(resources)}
		inv.setTaskReferences()
		printTaskInventory(inv)
	}
	fmt.Fprint(os.Stderr, "End the listed tasks and stop the account daemon? [Y/n] ")
	if !confirmKillAll(os.Stdin) {
		return fmt.Errorf("cancelled; no tasks ended")
	}
	return nil
}

func confirmKillAll(input io.Reader) bool {
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes", "kill":
		return true
	default:
		return false
	}
}

func printDaemonSnapshot(w io.Writer, host string, snapshot daemonSnapshot) {
	fmt.Fprintf(w, "HOST  %s · daemon %s\n", cleanText(host), cleanText(snapshot.Version))
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tNAME")
	tasks := slices.Clone(snapshot.Tasks)
	for i := range tasks {
		tasks[i].ID = strings.TrimPrefix(tasks[i].ID, "task:")
	}
	slices.SortStableFunc(tasks, func(a, b daemonTaskInfo) int { return cmp.Compare(a.ID, b.ID) })
	for i, task := range tasks {
		width := min(6, len(task.ID))
		for _, neighbor := range []int{i - 1, i + 1} {
			if neighbor < 0 || neighbor >= len(tasks) {
				continue
			}
			for width < len(task.ID) && strings.HasPrefix(tasks[neighbor].ID, task.ID[:width]) {
				width++
			}
		}
		fmt.Fprintf(table, "%s\t(%s)\n", cleanText(task.ID[:width]), cleanText(task.Name))
	}
	table.Flush()
}

func killResourceIdentity(id, identity string) error {
	kind, name, ok := strings.Cut(id, "/")
	if !ok || identity == "" {
		return fmt.Errorf("missing resource identity")
	}
	return stopResource(kind, name, identity)
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
func (s *resourceService) saveResult(code int, err error, name string) {
	state := "stopped"
	if code != 0 || err != nil {
		state = "failed"
	}
	s.setTransport(state, err)
	record := s.record
	// UID-backed receipts keep the final alias in the record itself. An old
	// name-keyed slot still needs its original name for lookup.
	if validTaskUID(record.UID) && validSession(name) == nil {
		record.Name = name
	}
	result := resourceResult{Record: record, Status: s.statusSnapshot(), Finished: time.Now().UTC()}
	if err := writeResourceResult(s.resultPath, result); err != nil {
		return
	}
	_, discarded := resourceResultFiles(filepath.Dir(s.resultPath), result.Finished.Add(-exitReceiptLifetime))
	for _, file := range discarded {
		_ = os.Remove(file.path)
	}
}

func writeResourceResult(path string, r resourceResult) error {
	r.Record.Token, r.Record.Address = "", ""
	return writeBoundedStateJSON(path, r, resourceResultMaxBytes)
}

type resourceResult struct {
	Record   resourceRecord
	Status   resourceStatus
	Finished time.Time
}

const resourceResultSuffix = ".json.result"
const resourceResultMaxBytes = 128 * 1024

type resourceResultFile struct {
	path, slot string
	at         time.Time
}

// The same age and count bounds apply whether reading history or pruning it.
// Use file metadata here so cleanup never needs to decode old receipts.
func resourceResultFiles(dir string, cutoff time.Time) (recent, discarded []resourceResultFile) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var files []resourceResultFile
	for _, entry := range entries {
		slot, ok := strings.CutSuffix(entry.Name(), resourceResultSuffix)
		if !ok || validSession(slot) != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, resourceResultFile{path: filepath.Join(dir, entry.Name()), slot: slot, at: info.ModTime()})
	}
	return partitionExitReceipts(files, cutoff, func(file resourceResultFile) time.Time { return file.at })
}

func (file resourceResultFile) read(kind string, cutoff time.Time) (resourceResult, bool) {
	var stored struct {
		resourceResult
		DisplayName string // Older receipts stored the displayed alias separately.
	}
	if err := readStateJSON(file.path, resourceResultMaxBytes, &stored); err != nil ||
		stored.Record.storageName() != file.slot || stored.Record.Kind != kind || stored.Finished.Before(cutoff) {
		return resourceResult{}, false
	}
	if validSession(stored.DisplayName) == nil {
		stored.Record.slot, stored.Record.Name = file.slot, stored.DisplayName
	}
	return stored.resourceResult, true
}

// A mount worker can record its exit even if it loses its parent. When the
// daemon owns the exit, it publishes the final label under the same UID.
func updateMountResultName(path, uid, name string) {
	file := resourceResultFile{path: path, slot: uid}
	result, ok := file.read("mount", time.Now().Add(-exitReceiptLifetime))
	if !ok || result.Record.UID != uid {
		return
	}
	result.Record.Name = name
	_ = writeResourceResult(path, result)
}

func collectResourceResults(active []resourceView) []resourceView {
	seen := make(map[string]bool, len(active)*2)
	for _, view := range active {
		seen[view.Identity] = true
		seen[view.ID] = true
	}
	cutoff := time.Now().Add(-exitReceiptLifetime)
	var rows []resourceView
	for _, kind := range []string{"mount", "forward"} {
		dir, err := resourceStateDir(kind)
		if err != nil {
			continue
		}
		recent, _ := resourceResultFiles(dir, cutoff)
		for _, file := range recent {
			result, ok := file.read(kind, cutoff)
			if !ok {
				continue
			}
			record := result.Record
			if validTaskUID(record.UID) {
				if seen[resourceIdentity(record)] {
					continue
				}
			} else if seen[kind+"/"+file.slot] || seen[kind+"/"+record.Name] {
				continue
			}
			view := viewRecord(record)
			view.Worker, view.resourceStatus = "exited", result.Status
			rows = append(rows, view)
		}
	}
	slices.SortFunc(rows, func(a, b resourceView) int { return cmp.Compare(a.ID, b.ID) })
	return rows
}
