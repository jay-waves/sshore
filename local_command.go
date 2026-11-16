package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type localShellView struct {
	Temporary bool   `json:"temporary,omitempty"`
	ID        uint64 `json:"id"`
	identity  string
	Name      string `json:"name"`
	State     string `json:"state"`
	PID       int    `json:"pid,omitempty"`
	Version   string `json:"version"`
	Directory string `json:"directory,omitempty"`
}

func localRequest(name string, request message, expected ...string) (message, error) {
	r, err := readLocalSession(name)
	if err != nil {
		return message{}, err
	}
	if len(expected) > 0 && expected[0] != "" && r.identity() != expected[0] {
		return message{}, fmt.Errorf("local task changed; refresh with sshore list")
	}
	c, err := dialLocalSession(r)
	if err != nil {
		return message{}, err
	}
	defer c.Close()
	if err = writeMessage(c, request); err != nil {
		return message{}, err
	}
	return readMessage(c)
}

func localShells() ([]localShellView, error) {
	dir, err := stateDir("local-shells")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	records := []localShellRecord{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		r, err := readLocalSession(name)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	rows := make([]localShellView, len(records))
	deadline := time.Now().Add(2 * time.Second)
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(8, len(records)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				rows[i] = inspectLocalShell(records[i], deadline)
			}
		}()
	}
	for i := range records {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	keys := make([]string, len(rows))
	for i := range rows {
		keys[i] = rows[i].identity
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].ID = ids[rows[i].identity]
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func inspectLocalShell(r localShellRecord, deadline time.Time) localShellView {
	row := localShellView{Temporary: r.Owner.PID != 0, identity: r.identity(), Name: r.Name, State: "unavailable", Version: r.Version, Directory: r.Directory}
	c, err := dialLocalSessionUntil(r, deadline)
	if err != nil {
		return row
	}
	defer c.Close()
	if err := writeMessage(c, message{Type: "status"}); err != nil {
		return row
	}
	m, err := readMessage(c)
	if err != nil || m.Type != "session" {
		return row
	}
	row.PID = m.PID
	row.State = "detached"
	if m.Attached {
		row.State = "attached"
	} else if m.PID == 0 {
		row.State = "starting"
	}
	return row
}

func runLocalCommand(o options) (int, error) {
	switch o.command {
	case "launch", "spawn":
		return runClient(o)
	case "attach":
		if o.session == "" {
			rows, err := localShells()
			if err != nil {
				return 1, err
			}
			for _, r := range rows {
				if r.State != "detached" {
					continue
				}
				if o.session != "" {
					return 1, fmt.Errorf("multiple local shells; use sshore list --local, then sshore attach --local NAME")
				}
				o.session = r.Name
			}
			if o.session == "" {
				return 1, fmt.Errorf("no detached local shells; use sshore launch")
			}
		}
		return runClient(o)
	case "list":
		rows, err := localShells()
		if err != nil {
			return 1, err
		}
		if o.listJSON {
			return 0, json.NewEncoder(os.Stdout).Encode(rows)
		}
		printTaskInventory(inventory{LocalShells: rows}, o.listDetails)
		return 0, nil
	case "cancel":
		m, err := localRequest(o.session, message{Type: "cancel"}, o.taskIdentity)
		if err == nil && m.Type != "cancelled" {
			err = fmt.Errorf("local cancellation unconfirmed")
		}
		return 0, err
	case "rename":
		m, err := localRequest(o.session, message{Type: "rename", Name: o.newName}, o.taskIdentity)
		if err == nil && m.Type != "renamed" {
			err = fmt.Errorf("rename local shell: %s", cleanText(m.Error))
		}
		if err == nil {
			fmt.Fprintf(os.Stdout, "Renamed local shell %s to %s\n", o.session, o.newName)
		}
		return 0, err
	default:
		return 2, usage("local %s is not available yet", o.command)
	}
}
