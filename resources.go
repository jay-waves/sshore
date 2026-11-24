package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	transportio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func collectLocalResources(host string, withStatus bool) ([]resourceView, error) {
	snapshot, statuses, err := readTaskInventory(taskInventoryRequest{Resources: true, Status: withStatus, Host: host})
	if err != nil {
		return nil, err
	}
	return resourceViews(snapshot.Resources, statuses, withStatus), nil
}

func resourceViews(records []resourceRecord, statuses map[string]resourceStatus, withStatus bool) []resourceView {
	views := make([]resourceView, 0, len(records))
	for _, record := range records {
		view := viewRecord(record)
		if withStatus {
			switch status, ok := statuses[record.UID]; {
			case !ok:
				view.LastError = "local worker unavailable or status deadline exceeded"
			case status.Version != version:
				view.LastError = "incompatible or invalid worker status"
			default:
				view.State, view.resourceStatus = taskActive, status
			}
		}
		views = append(views, view)
	}
	slices.SortFunc(views, func(a, b resourceView) int { return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.ID, b.ID)) })
	return views
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
		o.command = "daemon-admin"
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
		writeTaskInventoryColor(os.Stdout, inv, "", termui.ColorEnabled(os.Stdout))
	}
	prompt := termui.StatusLine(termui.ColorEnabled(os.Stderr), termui.Warning, "End the listed tasks and stop the account daemon? [Y/n]")
	fmt.Fprint(os.Stderr, prompt+" ")
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
