package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParticipatingShellTransitions(t *testing.T) {
	activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	first := participatingShell{Host: "work", Instance: strings.Repeat("a", 32), Name: "build",
		State: "attached", PID: 1234, Lifetime: "heartbeat", Temporary: true, Attachment: strings.Repeat("b", 32)}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func() participatingShell {
		t.Helper()
		rows, err := readParticipatingShells("work")
		if err != nil || len(rows) != 1 {
			t.Fatalf("participation: %+v, %v", rows, err)
		}
		return rows[0]
	}
	must(observeParticipatingShell(first))
	next := first
	next.Attachment = strings.Repeat("c", 32)
	must(observeParticipatingShell(next))
	first.State = "unconfirmed"
	must(setParticipatingShellState(first))
	if row := read(); row.State != "attached" {
		t.Fatalf("superseded attachment changed state: %+v", row)
	}
	next.State = "detached"
	must(setParticipatingShellState(next))
	must(renameParticipatingShell(next.Host, next.Instance, "renamed"))
	activityFixture(t) // Read the persisted record through a new daemon instance.
	if row := read(); row.Name != "renamed" || row.State != "detached" || row.PID != next.PID || row.Lifetime != "heartbeat" {
		t.Fatalf("persisted participation: %+v", row)
	}
	must(forgetParticipatingShell(next.Host, next.Instance))
	rows, err := readParticipatingShells("work")
	if err != nil || len(rows) != 0 {
		t.Fatalf("exited shell remained in participation: %+v, %v", rows, err)
	}
}

func TestParticipationRefreshUpdatesOnlyKnownTasks(t *testing.T) {
	activity := activityFixture(t)
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	first := participatingShell{Host: "work", Instance: strings.Repeat("a", 32), Name: "known",
		State: "attached", PID: 1234, Lifetime: "persistent", Attachment: strings.Repeat("b", 32)}
	missing := first
	missing.Instance, missing.Name = strings.Repeat("c", 32), "missing"
	other := first
	other.Host = "other"
	for _, row := range []participatingShell{first, missing, other} {
		if err := observeParticipatingShell(row); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now()
	for i := range activity.metadata.participation.rows {
		activity.metadata.participation.rows[i].ChangedAt = since.Add(-time.Second)
	}
	late := first
	late.Instance, late.Name = strings.Repeat("d", 32), "late"
	if err := observeParticipatingShell(late); err != nil {
		t.Fatal(err)
	}
	observed := []shellView{
		{Instance: first.Instance, Name: "known-renamed", State: "detached", PID: first.PID, Lifetime: "persistent"},
		{Instance: strings.Repeat("e", 32), Name: "unrelated", State: "attached", PID: 4321, Lifetime: "persistent"},
	}
	if err := reconcileParticipatingShells("work", observed, since); err != nil {
		t.Fatal(err)
	}
	rows, err := readParticipatingShells("")
	if err != nil || len(rows) != 3 {
		t.Fatalf("participation: %+v, %v", rows, err)
	}
	if rows[0].Name != "known-renamed" || rows[0].State != "detached" || rows[1].Host != "other" || rows[2].Name != "late" {
		t.Fatalf("refresh changed unrelated observations: %+v", rows)
	}
}
