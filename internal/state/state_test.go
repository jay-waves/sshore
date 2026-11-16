package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataPublication(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := Dir("records")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "record.json")
	if err := WriteJSON(path, map[string]int{"value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := Private(path, false); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(path, make(chan int)); err == nil {
		t.Fatal("accepted non-JSON metadata")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"value":1}` {
		t.Fatalf("failed encoding damaged record: %q %v", data, err)
	}
	if err := WriteJSON(path, map[string]int{"value": 2}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != `{"value":2}` {
		t.Fatalf("replacement: %q %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
	if err := WriteFile(filepath.Join(dir, "missing", "record"), []byte("x")); err == nil {
		t.Fatal("accepted missing parent")
	}
}
