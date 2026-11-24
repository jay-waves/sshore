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
		State: taskActive, PID: 1234, Attachment: strings.Repeat("b", 32)}
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
	first.State = taskUnknown
	must(setParticipatingShellState(first))
	if row := read(); row.State != taskActive {
		t.Fatalf("superseded attachment changed state: %+v", row)
	}
	next.State = taskDetached
	must(setParticipatingShellState(next))
	must(renameParticipatingShell(next.Host, next.Instance, "renamed"))
	activityFixture(t) // Read the persisted record through a new daemon instance.
	if row := read(); row.Name != "renamed" || row.State != taskDetached || row.PID != next.PID {
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
		State: taskActive, PID: 1234, Attachment: strings.Repeat("b", 32)}
	missing := first
	missing.Instance, missing.Name = strings.Repeat("c", 32), "missing"
	other := first
	other.Host = "other"
	unchanged := first
	unchanged.Instance, unchanged.Name = strings.Repeat("f", 32), "unchanged"
	for _, row := range []participatingShell{first, missing, other, unchanged} {
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
		{Instance: first.Instance, Name: "known-renamed", State: taskDetached, PID: first.PID},
		{Instance: unchanged.Instance, Name: unchanged.Name, State: unchanged.State, PID: unchanged.PID},
		{Instance: strings.Repeat("e", 32), Name: "unrelated", State: taskActive, PID: 4321},
	}
	if err := reconcileParticipatingShells("work", observed, since); err != nil {
		t.Fatal(err)
	}
	rows, err := readParticipatingShells("")
	if err != nil || len(rows) != 4 {
		t.Fatalf("participation: %+v, %v", rows, err)
	}
	if rows[0].Name != "known-renamed" || rows[0].State != taskDetached || rows[1].Host != "other" || rows[2].Name != "unchanged" || rows[3].Name != "late" {
		t.Fatalf("refresh changed unrelated observations: %+v", rows)
	}
	if rows[2].ChangedAt.Before(since) {
		t.Fatalf("refresh did not advance the observed time for an unchanged shell: %+v", rows[2])
	}
}
