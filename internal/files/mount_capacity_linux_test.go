//go:build linux

package files

import (
	"context"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestFTPCapacitySFTP(t *testing.T) {
	f := testFTPBackend(t)
	s, err := ftpRemoteCapacity(f.client, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Blocks == 0 {
		t.Fatal("empty backing filesystem")
	}
	var out fuse.StatfsOut
	n := &ftpNode{backend: f}
	if errno := n.Statfs(context.Background(), &out); errno != 0 {
		t.Fatal(errno)
	}
	if out.Blocks != s.Blocks || uint64(out.Frsize) != s.Frsize || uint64(out.Bsize) != s.Bsize {
		t.Fatalf("capacity mapping: %+v, source %+v", out, s)
	}
}
