package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExternalSpawnParentDisplay(t *testing.T) {
	start, err := spawnProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	machine := strings.Repeat("a", 32)
	p := &spawnProcessView{PID: os.Getpid(), Start: start, MachineID: machine, Name: "stale-name"}
	input := []shellTreeNode{
		{Identity: "one", Name: "one", Lifetime: "parent", OwnerProcess: p},
		{Identity: "two", Name: "two", Lifetime: "heartbeat", OwnerProcess: p},
		{Identity: "persistent", Lifetime: "persistent", OwnerProcess: p},
	}
	nodes := externalSpawnParents(input, machine)
	if len(nodes) != 4 || nodes[0].Owner == "" || nodes[0].Owner != nodes[1].Owner || nodes[2].Owner != "" || input[0].Owner != "" {
		t.Fatalf("bad external ownership: %+v", nodes)
	}
	var out bytes.Buffer
	writeShellTree(&out, nodes, false)
	if !strings.Contains(out.String(), fmt.Sprintf("(pid %d)", os.Getpid())) || !strings.Contains(out.String(), "external ") || strings.Contains(out.String(), "stale-name") || strings.Contains(out.String(), " ?") {
		t.Fatal(out.String())
	}
	input = append(input, shellTreeNode{Identity: "managed", Lifetime: "persistent"})
	input[0].Owner = "managed"
	nodes = externalSpawnParents(input, machine)
	if nodes[0].Owner != "managed" {
		t.Fatal("replaced a visible managed owner")
	}
}

func TestExternalSpawnParentUnavailable(t *testing.T) {
	machine := strings.Repeat("a", 32)
	for _, foreign := range []bool{false, true} {
		p := &spawnProcessView{PID: os.Getpid(), Start: "reused-pid", MachineID: machine, Name: "wrong-process", Host: "other"}
		if foreign {
			p.MachineID = strings.Repeat("b", 32)
		}
		nodes := externalSpawnParents([]shellTreeNode{{Identity: "spawn", Lifetime: "parent", OwnerProcess: p}}, machine)
		if len(nodes) != 2 || nodes[1].State != "unavailable" {
			t.Fatalf("trusted unverified identity: %+v", nodes)
		}
		if foreign && nodes[1].Name != "wrong-process on other" {
			t.Fatal("resolved foreign PID locally")
		}
		if !foreign && nodes[1].Name != "unavailable" {
			t.Fatal("showed a reused PID's name")
		}
	}
}

func TestExternalSpawnOwnerSurvivesReattach(t *testing.T) {
	activityFixture(t)
	machine := strings.Repeat("a", 32)
	first := &spawnProcessView{PID: 123, Start: "first", MachineID: machine, Name: "bash"}
	second := &spawnProcessView{PID: 456, Start: "second", MachineID: machine, Name: "sh"}
	saveShellTreeWithProcess("external-child", "job", nil, "attached", first, "parent")
	saveShellTree("external-child", "job", nil, "detached")
	saveShellTreeWithProcess("external-child", "job", nil, "attached", second, "parent")
	chain := shellTreeFor("external-child")
	if len(chain) == 0 || chain[len(chain)-1].OwnerProcess == nil || chain[len(chain)-1].OwnerProcess.PID != first.PID {
		t.Fatalf("owner replaced: %+v", chain)
	}
}
