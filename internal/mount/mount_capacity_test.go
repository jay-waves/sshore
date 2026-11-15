package mount

import (
	"errors"
	"math"
	"syscall"
	"testing"

	"github.com/pkg/sftp"
)

type ftpCapacityStub struct {
	version string
	stat    *sftp.StatVFS
	err     error
	path    string
}

func (s *ftpCapacityStub) HasExtension(string) (string, bool)      { return s.version, s.version != "" }
func (s *ftpCapacityStub) StatVFS(p string) (*sftp.StatVFS, error) { s.path = p; return s.stat, s.err }

func TestFTPCapacityNegotiation(t *testing.T) {
	for _, version := range []string{"", "1", "3"} {
		c := &ftpCapacityStub{version: version}
		if _, err := ftpRemoteCapacity(c, "/data"); !errors.Is(err, syscall.ENOTSUP) || c.path != "" {
			t.Fatalf("version %q: err=%v path=%q", version, err, c.path)
		}
	}
	c := &ftpCapacityStub{version: "2", stat: &sftp.StatVFS{Bsize: 4096, Frsize: 512, Blocks: 100, Bfree: 40, Bavail: 30}}
	s, err := ftpRemoteCapacity(c, "/data")
	if err != nil || s != c.stat || c.path != "/data" {
		t.Fatalf("stat=%v err=%v path=%q", s, err, c.path)
	}
	c.err = syscall.EACCES
	if _, err := ftpRemoteCapacity(c, "/data"); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
}

func TestFTPCapacityInvalidReplies(t *testing.T) {
	for _, s := range []*sftp.StatVFS{
		nil,
		{},
		{Bsize: 4096, Frsize: 4096, Blocks: math.MaxUint64},
		{Bsize: 4096, Frsize: 4096, Blocks: 10, Bfree: 11},
		{Bsize: 4096, Frsize: 4096, Blocks: 10, Bfree: 5, Bavail: 6},
		{Bsize: math.MaxUint64, Frsize: 4096},
		{Bsize: 4096, Frsize: 4096, Namemax: math.MaxUint64},
	} {
		if err := validateFTPCapacity(s); !errors.Is(err, syscall.EIO) {
			t.Fatalf("%+v: %v", s, err)
		}
	}
}
