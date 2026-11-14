package main

// Local in-process baseline only; not a WAN or FUSE benchmark. Compare request
// concurrency without changing production defaults or creating extra runtimes.
import (
	"fmt"
	"github.com/pkg/sftp"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkSFTPReadConcurrency(b *testing.B) {
	for _, concurrency := range []int{8, 64} {
		b.Run(fmt.Sprint(concurrency), func(b *testing.B) {
			root := b.TempDir()
			path := filepath.Join(root, "file")
			data := make([]byte, 1024*1024)
			if e := os.WriteFile(path, data, 0600); e != nil {
				b.Fatal(e)
			}
			left, right := net.Pipe()
			server, e := sftp.NewServer(left)
			if e != nil {
				b.Fatal(e)
			}
			go func() { server.Serve(); server.Close() }()
			defer server.Close()
			wire := newFTPWire(right, right)
			client, e := sftp.NewClientPipe(wire, wire, sftp.MaxConcurrentRequestsPerFile(concurrency))
			if e != nil {
				b.Fatal(e)
			}
			defer client.Close()
			file, e := client.Open(filepath.ToSlash(path))
			if e != nil {
				b.Fatal(e)
			}
			defer file.Close()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, e := file.ReadAt(data, 0); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
