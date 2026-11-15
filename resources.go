package main

// Local resource IDs are type/name, stable across list calls. Shell identities
// remain HOST/NAME and retain the existing version/process-safe kill path. List
// without HOST is offline discovery: it never authenticates to guessed hosts.
import (
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/jay-waves/sshore/internal/termui"
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
			records = append(records, resourceRecord{Token: r.ID, Kind: "review", Name: r.DisplayName(), Host: r.Host, Detail: r.Session + ": " + approvalSummary(r.Operations)})
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

// Explicit type prefixes route local cancellation; HOST NAME uses the existing
// version- and process-identity-checked remote session termination path.
func runCancel(args []string) (int, error) {
	if o, all, err := parseCancelAll(args); all {
		if errors.Is(err, errHelp) {
			return 0, showHelp([]string{"cancel"}, os.Stdout)
		}
		if err != nil {
			return 2, err
		}
		return runCancelAll(o)
	}
	if len(args) == 0 {
		return 2, usage("use sshore cancel review/NAME, mount/NAME, forward/NAME, or HOST NAME")
	}
	kind, name, typed := strings.Cut(args[0], "/")
	if typed {
		if len(args) != 1 {
			return 2, usage("local cancellation accepts exactly one resource ID")
		}
		switch kind {
		case "peer":
			return 2, usage("peer tasks are read-only here; cancel the resource on its managing computer")
		case "review":
			if !validApprovalID(name) && validSession(name) != nil {
				return 2, usage("invalid approval ID")
			}
			dir, err := approvalDir()
			if err != nil {
				return 1, err
			}
			record, readErr := findApproval(dir, name)
			if readErr != nil {
				return 1, readErr
			}
			if err = consumeApproval(dir, record.ID); err != nil {
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
	return runShellCommand(append([]string{"cancel"}, args...))
}
