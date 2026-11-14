package main

// Local resource IDs are type/name, stable across list calls. Shell identities
// remain HOST/NAME and retain the existing version/process-safe kill path. List
// without HOST is offline discovery: it never authenticates to guessed hosts.
import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

func collectLocalResources(host string) ([]resourceRecord, error) {
	var records []resourceRecord
	for _, kind := range []string{"mount", "forward"} {
		dir, e := resourceStateDir(kind)
		if e != nil {
			return nil, e
		}
		files, e := os.ReadDir(dir)
		if e != nil {
			return nil, e
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			r, e := readResourceRecord(kind, strings.TrimSuffix(f.Name(), ".json"))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return nil, e
			}
			if host == "" || r.Host == host {
				records = append(records, r)
			}
		}
	}
	dir, e := approvalDir()
	if e != nil {
		return nil, e
	}
	pending, e := pendingApprovals(dir)
	if e != nil {
		return nil, e
	}
	for _, r := range pending {
		if host == "" || r.Host == host {
			records = append(records, resourceRecord{Kind: "approval", Name: r.ID, Host: r.Host, Detail: r.Session + ": " + approvalSummary(r.Operations)})
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
func listLocalResources(host string) error {
	records, err := collectLocalResources(host)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stdout, "No local resources or pending approvals.")
		return nil
	}
	out := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer out.Flush()
	fmt.Fprintln(out, "ID\tHOST\tSTATE\tDETAILS")
	for _, v := range inspectResources(records) {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", cleanText(v.ID), cleanText(v.Host), v.Worker, cleanText(v.Detail))
	}

	return nil
}

// Explicit type prefixes route local cancellation; HOST NAME uses the existing
// version- and process-identity-checked remote session termination path.
func runCancel(args []string) (int, error) {
	if len(args) == 0 {
		return 2, usage("use sshore cancel approval/ID, mount/NAME, forward/NAME, or HOST NAME")
	}
	kind, name, typed := strings.Cut(args[0], "/")
	if typed {
		if len(args) != 1 {
			return 2, usage("local cancellation accepts exactly one resource ID")
		}
		switch kind {
		case "peer":
			return 2, usage("peer tasks are read-only here; cancel the resource on its managing computer")
		case "approval":
			if !validApprovalID(name) {
				return 2, usage("invalid approval ID")
			}
			dir, err := approvalDir()
			if err != nil {
				return 1, err
			}
			record, readErr := readApproval(dir, name)
			if err = consumeApproval(dir, name); err != nil {
				return 1, err
			}
			if readErr == nil {
				removeRejectedPeerTask(record)
			}
			logf(os.Stderr, logSuccess, "[sshore] Request cancelled\n")
			return 0, nil
		case "mount", "forward":
			return 0, stopResource(kind, name)
		default:
			return 2, usage("resource ID must be approval/ID, mount/NAME or forward/NAME")
		}
	}
	o, err := parseOptions(append([]string{"cancel"}, args...))
	if errors.Is(err, errHelp) {
		return 0, showHelp([]string{"cancel"}, os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	return runClient(o)
}

func listThisComputer() error {
	if e := listLocalResources(""); e != nil {
		return e
	}
	if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		fmt.Fprintln(os.Stdout, "\nShell sessions on this computer:")
		return serverCommand([]string{"sessions"})
	}
	return listRemoteTasks()
}
