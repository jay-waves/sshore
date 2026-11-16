//go:build linux

package files

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/sftp"
)

// The ordinary pkg/sftp server fixture synthesizes atime from mtime. This peer
// exposes distinct timestamps and applies the actual SETSTAT request instead.
func testTimestampBackend(t *testing.T, atime, mtime uint32) *ftpFS {
	t.Helper()
	server, conn := net.Pipe()
	_ = server.SetDeadline(time.Now().Add(5 * time.Second))
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() {
		for {
			kind, body, err := readTimestampPacket(server)
			if err != nil {
				done <- err
				return
			}
			if kind == 1 {
				err = writeTimestampPacket(server, 2, []byte{0, 0, 0, 3})
				if err != nil {
					done <- err
					return
				}
				continue
			}
			if len(body) < 4 {
				done <- fmt.Errorf("short metadata request")
				return
			}
			id := binary.BigEndian.Uint32(body)
			var reply []byte
			put := func(n uint32) { reply = binary.BigEndian.AppendUint32(reply, n) }
			put(id)
			switch kind {
			case 7, 17: // LSTAT / STAT
				put(4 | 8)
				put(syscall.S_IFREG | 0600)
				put(atime)
				put(mtime)
				err = writeTimestampPacket(server, 105, reply)
			case 9: // SETSTAT: request ID, path string, flags, attributes
				if len(body) < 8 {
					done <- fmt.Errorf("short SETSTAT")
					return
				}
				offset := 8 + int(binary.BigEndian.Uint32(body[4:]))
				if len(body) != offset+12 || binary.BigEndian.Uint32(body[offset:]) != 8 {
					done <- fmt.Errorf("unexpected SETSTAT attributes")
					return
				}
				atime = binary.BigEndian.Uint32(body[offset+4:])
				mtime = binary.BigEndian.Uint32(body[offset+8:])
				put(0)
				put(0)
				put(0)
				err = writeTimestampPacket(server, 101, reply)
			default:
				done <- fmt.Errorf("unexpected metadata request %d", kind)
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	client, err := sftp.NewClientPipe(conn, conn)
	if err != nil {
		conn.Close()
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); conn.Close(); server.Close(); <-done })
	return &ftpFS{client: client, root: t.TempDir()}
}

func TestLinuxSetattrMtimePreservesAtime(t *testing.T) {
	atime, mtime := uint32(1700000000), uint32(1700001000)
	backend := testTimestampBackend(t, atime, mtime)
	root := &ftpNode{backend: backend}
	fs.NewNodeFS(root, nil)
	child := &ftpNode{backend: backend}
	root.AddChild("file", root.NewPersistentInode(context.Background(), child, fs.StableAttr{Mode: syscall.S_IFREG}), false)
	wanted := time.Unix(1700002000, 0)
	var in fuse.SetAttrIn
	in.Valid = fuse.FATTR_MTIME
	in.Mtime = uint64(wanted.Unix())
	var out fuse.AttrOut
	if errno := child.Setattr(context.Background(), nil, &in, &out); errno != 0 {
		t.Fatal(errno)
	}
	info, err := backend.client.Stat(backend.root + "/file")
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := info.Sys().(*sftp.FileStat)
	if !ok {
		t.Fatalf("unexpected stat representation %T", info.Sys())
	}
	if raw.Atime != atime {
		t.Fatalf("atime changed: got %d want %d", raw.Atime, atime)
	}
	if !info.ModTime().Equal(wanted) {
		t.Fatalf("mtime=%v want %v", info.ModTime(), wanted)
	}
}

func readTimestampPacket(r io.Reader) (byte, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size < 1 || size > 4096 {
		return 0, nil, fmt.Errorf("invalid metadata packet size %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return body[0], body[1:], nil
}
func writeTimestampPacket(w io.Writer, kind byte, body []byte) error {
	data := binary.BigEndian.AppendUint32(nil, uint32(len(body)+1))
	data = append(data, kind)
	data = append(data, body...)
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func TestLinuxSetattrModeUsesOpenHandle(t *testing.T) {
	backend := testFTPBackend(t)
	file, err := backend.open("file", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	root := &ftpNode{backend: backend}
	fs.NewNodeFS(root, nil)
	child := &ftpNode{backend: backend}
	root.AddChild("file", root.NewPersistentInode(context.Background(), child, fs.StableAttr{Mode: syscall.S_IFREG}), false)
	original := filepath.Join(backend.root, "original")
	replacement := filepath.Join(backend.root, "file")
	if err := os.Rename(replacement, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0644); err != nil {
		t.Fatal(err)
	}
	var in fuse.SetAttrIn
	in.Valid, in.Mode = fuse.FATTR_MODE, 0600
	var out fuse.AttrOut
	if errno := child.Setattr(context.Background(), &ftpLinuxFile{file: file}, &in, &out); errno != 0 {
		t.Fatal(errno)
	}
	for name, want := range map[string]os.FileMode{original: 0600, replacement: 0644} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", name, info.Mode().Perm(), want)
		}
	}
}

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
