package main

import (
	"path/filepath"
	"testing"
)

func TestShellConnectionConfirmation(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	o := options{host: "work", session: "eevee"}
	c, err := trackShellConnection(o)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readShellSnapshots()
	if err != nil || len(rows) != 1 || !rows[0].Active {
		t.Fatalf("%+v %v", rows, err)
	}
	c.confirm("eevee", "detached")
	c.close()
	rows, err = readShellSnapshots()
	if err != nil || rows[0].Active || rows[0].State != "detached" || rows[0].Unconfirmed {
		t.Fatalf("%+v %v", rows, err)
	}
	v := shellView{State: rows[0].State, ConfirmedAt: rows[0].ConfirmedAt}
	if snapshotState(v) != "-" || snapshotState(shellView{Active: true}) != "local active" || snapshotState(shellView{State: "attached"}) != "unconfirmed" {
		t.Fatal("incorrect cached connection state")
	}
	c, err = trackShellConnection(o)
	if err != nil {
		t.Fatal(err)
	}
	c.close()
	rows, err = readShellSnapshots()
	if err != nil || !rows[0].Unconfirmed || rows[0].Active {
		t.Fatalf("%+v %v", rows, err)
	}
	inv := emptyInventory()
	inv.Errors = []string{"unreachable shell"}
	if err = cacheRemoteShells("work", inv); err != nil {
		t.Fatal(err)
	}
	rows, _ = readShellSnapshots()
	if rows[0].State == "not-listed" {
		t.Fatal("incomplete inventory erased shell")
	}
	inv.Errors = nil
	if err = cacheRemoteShells("work", inv); err != nil {
		t.Fatal(err)
	}
	rows, _ = readShellSnapshots()
	if rows[0].State != "not-listed" || rows[0].Unconfirmed {
		t.Fatal(rows)
	}
}
