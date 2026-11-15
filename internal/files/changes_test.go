package files

import (
	"fmt"
	"os"
	"testing"
)

func TestWatchChangesBounded(t *testing.T) {
	var changes watchChanges
	for i := 0; i < 100; i++ {
		changes.add("/same", false)
	}
	reset, dirs := changes.take()
	if reset || len(dirs) != 1 || dirs[0] != "/same" {
		t.Fatal(reset, dirs)
	}
	for i := 0; i <= ftpWatchLimit; i++ {
		changes.add(fmt.Sprintf("/dir%d", i), false)
	}
	reset, dirs = changes.take()
	if !reset || len(dirs) != 0 {
		t.Fatal("overflow lost reset", reset, dirs)
	}
	changes.add("/a", false)
	changes.add("", true)
	changes.add("/b", false)
	reset, dirs = changes.take()
	if !reset || len(dirs) != 0 {
		t.Fatal("reset did not dominate")
	}
	reset, dirs = changes.take()
	if reset || len(dirs) != 0 {
		t.Fatal("changes not drained")
	}
}
func TestPrefetchStatistics(t *testing.T) {
	var c ftpMetadataCache
	fetch := func() ([]os.FileInfo, error) { return []os.FileInfo{ftpCacheInfo{name: "file"}}, nil }
	_, _ = c.readDirMode("/used", true, fetch)
	_, _ = c.readDirMode("/unused", true, fetch)
	_, _ = c.readDir("/used", fetch)
	_, _ = c.readDir("/used", fetch)
	c.invalidateRemote()
	if c.counters.PrefetchStored != 2 || c.counters.PrefetchHits != 1 || c.counters.PrefetchDiscarded != 1 {
		t.Fatal(c.counters)
	}
}
