package main

// Local resource IDs are type/name, stable across list calls. Shell identities
// remain HOST/NAME and retain the existing version/process-safe kill path. List
// without HOST is offline discovery: it never authenticates to guessed hosts.
import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

func listLocalResources(host, kindFilter string) error {
	var records []resourceRecord
	for _, kind := range []string{"mount", "forward"} {
		if kindFilter != "" && kind != kindFilter {
			continue
		}
		dir, e := resourceStateDir(kind)
		if e != nil {
			return e
		}
		files, e := os.ReadDir(dir)
		if e != nil {
			return e
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
				return e
			}
			if host == "" || r.Host == host {
				records = append(records, r)
			}
		}
	}
	if kindFilter == "" || kindFilter == "approval" {
		dir, e := approvalDir()
		if e != nil {
			return e
		}
		files, e := os.ReadDir(dir)
		if e != nil {
			return e
		}
		for _, f := range files {
			id := strings.TrimSuffix(f.Name(), ".json")
			if !validApprovalID(id) {
				continue
			}
			r, e := readApproval(dir, id)
			if e != nil {
				continue
			}
			if host != "" && r.Host != host {
				continue
			}
			detail := fmt.Sprintf("%s: %d requested operation(s)", r.Session, len(r.Operations))
			if len(r.Operations) == 1 {
				op := r.Operations[0]
				if op.Kind == "forward" {
					detail = r.Session + ": " + describeForward(op.Port, op.RemoteHost, op.RemotePort, op.Reverse)
				} else {
					detail = r.Session + ": " + op.Kind + " " + op.Path
				}
			}
			records = append(records, resourceRecord{Kind: "approval", Name: id, Host: r.Host, Detail: detail})
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
	if len(records) == 0 {
		fmt.Fprintln(os.Stdout, "No local resources or pending approvals.")
		return nil
	}
	out := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer out.Flush()
	fmt.Fprintln(out, "ID\tHOST\tSTATE\tDETAILS")
	for _, r := range records {
		state := "pending"
		if r.Kind != "approval" {
			state = "offline"
			resp, e := resourceHTTP(r.Address, r.Token, "GET", "/status", nil)
			if e == nil {
				if resp.StatusCode == 200 {
					state = "active"
				}
				resp.Body.Close()
			}
		}
		detail := r.Detail
		if r.Kind == "mount" {
			detail = r.Mountpoint + " -> " + r.Remote
		}
		fmt.Fprintf(out, "%s/%s\t%s\t%s\t%s\n", r.Kind, cleanText(r.Name), cleanText(r.Host), state, cleanText(detail))
	}
	return nil
}
func runStop(args []string) (int, error) {
	if len(args) != 1 {
		return 2, usage("use sshore stop mount/NAME or forward/NAME; shells use sshore kill HOST NAME")
	}
	kind, name, ok := strings.Cut(args[0], "/")
	if kind == "peer" {
		return 2, usage("peer tasks are read-only here; stop the resource on its managing computer")
	}
	if kind == "approval" {
		return 2, usage("use sshore approve approval/ID --reject to reject a pending request")
	}
	if !ok || (kind != "mount" && kind != "forward") {
		return 2, usage("resource ID must be mount/NAME or forward/NAME; shells use sshore kill HOST NAME")
	}
	return 0, stopResource(kind, name)
}

func listThisComputer() error {
	if e := listLocalResources("", ""); e != nil {
		return e
	}
	if os.Getenv("SSHORE_SOCKET") != "" || os.Getenv("SSH_CONNECTION") != "" {
		fmt.Fprintln(os.Stdout, "\nShell sessions on this computer:")
		return serverCommand([]string{"sessions"})
	}
	return listRemoteTasks()
}
