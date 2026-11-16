package files

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"github.com/jay-waves/sshore/internal/transport"
)

// SFTP v3 framing guard. pkg/sftp's ReadDir accumulates all NAME packets before
// returning; enforce a budget before its decoder allocates entries. Only READDIR
// replies count, keyed by request ID and remote handle. An oversized directory
// receives a normal failure status so the rest of the mount remains usable.
// Packet size and outstanding requests are bounded independently. This layer
// does not implement file operations or change successful server responses.

const (
	ftpMaxPacket         = 1024 * 1024
	ftpMaxDirectoryBytes = 32 * 1024 * 1024
	ftpMaxRequests       = 4096
)

type ftpRequest struct {
	started time.Time
	kind    byte
	handle  string
}
type ftpDirectoryBudget struct{ entries, bytes uint64 }
type ftpWire struct {
	onReply                       func()
	metrics                       bool
	totals                        [256]ftpRequestTotals
	peakRequests                  int
	reader                        io.Reader
	writer                        io.WriteCloser
	readBuffer, writeBuffer       []byte
	readOffset                    int
	mu                            sync.Mutex
	requests                      map[uint32]ftpRequest
	directories                   map[string]ftpDirectoryBudget
	maxEntries, maxDirectoryBytes uint64
}

func newFTPWire(r io.Reader, w io.WriteCloser) *ftpWire {
	return &ftpWire{reader: r, writer: w, requests: make(map[uint32]ftpRequest), directories: make(map[string]ftpDirectoryBudget), maxEntries: ftpMaxDirectoryEntries, maxDirectoryBytes: ftpMaxDirectoryBytes}
}
func NewSFTP(reader io.Reader, writer io.WriteCloser) (*sftp.Client, error) {
	wire := newFTPWire(reader, writer)
	return sftp.NewClientPipe(wire, wire)
}
func (w *ftpWire) Close() error { return w.writer.Close() }
func ftpWireString(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	n := uint64(binary.BigEndian.Uint32(b))
	if n > uint64(len(b)-4) {
		return "", false
	}
	return string(b[4 : 4+int(n)]), true
}
func (w *ftpWire) record(packet []byte) error {
	if len(packet) < 5 {
		return fmt.Errorf("short outgoing SFTP packet")
	}
	kind := packet[0]
	if kind == 1 {
		return nil
	} // INIT has a version instead of an ID.
	id := binary.BigEndian.Uint32(packet[1:5])
	req := ftpRequest{kind: kind, started: time.Now()}
	if kind == 12 || kind == 4 {
		var ok bool
		req.handle, ok = ftpWireString(packet[5:])
		if !ok {
			return fmt.Errorf("invalid SFTP handle")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) >= ftpMaxRequests {
		return fmt.Errorf("too many outstanding SFTP requests")
	}
	w.requests[id] = req
	if w.metrics {
		w.totals[kind].Started++
		w.totals[kind].SentBytes += uint64(len(packet) + 4)
		w.peakRequests = max(w.peakRequests, len(w.requests))
	}
	if kind == 4 {
		delete(w.directories, req.handle)
	}
	return nil
}
func (w *ftpWire) Write(p []byte) (int, error) {
	// pkg/sftp serializes writes, but can split a frame between header and data.
	accepted := 0
	for len(p) > 0 {
		need := 4 - len(w.writeBuffer)
		if need <= 0 {
			n := int(binary.BigEndian.Uint32(w.writeBuffer[:4]))
			if n < 5 || n > ftpMaxPacket {
				return accepted, fmt.Errorf("invalid SFTP request size %d", n)
			}
			need = 4 + n - len(w.writeBuffer)
		}
		take := min(need, len(p))
		w.writeBuffer = append(w.writeBuffer, p[:take]...)
		p = p[take:]
		accepted += take
		if len(w.writeBuffer) < 4 {
			continue
		}
		n := int(binary.BigEndian.Uint32(w.writeBuffer[:4]))
		if n < 5 || n > ftpMaxPacket {
			return accepted, fmt.Errorf("invalid SFTP request size %d", n)
		}
		if len(w.writeBuffer) != n+4 {
			continue
		}
		if e := w.record(w.writeBuffer[4:]); e != nil {
			return accepted, e
		}
		if e := transport.WriteFull(w.writer, w.writeBuffer); e != nil {
			return accepted, e
		}
		w.writeBuffer = w.writeBuffer[:0]
	}
	return accepted, nil
}
func ftpDirectoryFailure(id uint32) []byte {
	var b bytes.Buffer
	b.WriteByte(101) // SSH_FXP_STATUS
	_ = binary.Write(&b, binary.BigEndian, id)
	_ = binary.Write(&b, binary.BigEndian, uint32(4)) // SSH_FX_FAILURE
	msg := "directory exceeds sshore listing limit"
	_ = binary.Write(&b, binary.BigEndian, uint32(len(msg)))
	b.WriteString(msg)
	_ = binary.Write(&b, binary.BigEndian, uint32(0)) // language
	return b.Bytes()
}
func (w *ftpWire) filter(packet []byte) ([]byte, error) {
	if len(packet) < 5 {
		return nil, fmt.Errorf("short SFTP response")
	}
	if packet[0] == 2 {
		return packet, nil
	} // VERSION
	if w.onReply != nil {
		w.onReply()
	}
	id := binary.BigEndian.Uint32(packet[1:5])
	w.mu.Lock()
	defer w.mu.Unlock()
	req, ok := w.requests[id]
	delete(w.requests, id)
	if !ok {
		return packet, nil
	} // pkg/sftp diagnoses unsolicited IDs.
	if w.metrics {
		total := &w.totals[req.kind]
		elapsed := time.Since(req.started)
		total.Completed++
		total.TotalNS += int64(elapsed)
		total.MaxNS = max(total.MaxNS, int64(elapsed))
		total.ReceivedBytes += uint64(len(packet) + 4)
		if packet[0] == 101 && len(packet) >= 9 && binary.BigEndian.Uint32(packet[5:9]) > 1 {
			total.Errors++
		}
	}
	if req.kind == 12 && packet[0] == 104 { // READDIR -> NAME
		if len(packet) < 9 {
			return nil, fmt.Errorf("short SFTP directory response")
		}
		budget := w.directories[req.handle]
		budget.entries += uint64(binary.BigEndian.Uint32(packet[5:9]))
		budget.bytes += uint64(len(packet))
		w.directories[req.handle] = budget
		if budget.entries > w.maxEntries || budget.bytes > w.maxDirectoryBytes {
			return ftpDirectoryFailure(id), nil
		}
	}
	return packet, nil
}
func (w *ftpWire) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.readOffset == len(w.readBuffer) {
		var header [4]byte
		if _, e := io.ReadFull(w.reader, header[:]); e != nil {
			return 0, e
		}
		size := binary.BigEndian.Uint32(header[:])
		if size < 5 || size > ftpMaxPacket {
			return 0, fmt.Errorf("invalid SFTP response size %d", size)
		}
		needed := 4 + int(size)
		if cap(w.readBuffer) < needed {
			w.readBuffer = make([]byte, needed)
		} else {
			w.readBuffer = w.readBuffer[:needed]
		}
		if _, e := io.ReadFull(w.reader, w.readBuffer[4:]); e != nil {
			return 0, e
		}
		packet, e := w.filter(w.readBuffer[4:])
		if e != nil {
			return 0, e
		}
		needed = 4 + len(packet)
		if cap(w.readBuffer) < needed {
			w.readBuffer = make([]byte, needed)
		} else {
			w.readBuffer = w.readBuffer[:needed]
		}
		binary.BigEndian.PutUint32(w.readBuffer, uint32(len(packet)))
		copy(w.readBuffer[4:], packet)
		w.readOffset = 0
	}
	n := copy(p, w.readBuffer[w.readOffset:])
	w.readOffset += n
	return n, nil
}

// Each outstanding SFTP request has its own deadline. A responsive sibling or
// SSH keepalive cannot hide one stuck request. Timeout tears down the transport;
// writes are never replayed and their remote outcome can be unknown. Idle mounts
// have no request deadline. Authentication is covered only by an opt-in startup
// deadline, since the subsystem protocol cannot distinguish an SSH prompt.

type sftpWatch struct {
	mu      sync.Mutex
	failure error
	timer   *time.Timer
}

func (w *sftpWatch) fail(err error, abort func()) {
	w.mu.Lock()
	first := w.failure == nil
	if first {
		w.failure = err
	}
	w.mu.Unlock()
	if first {
		abort()
	}
}
func (w *sftpWatch) err() error { w.mu.Lock(); defer w.mu.Unlock(); return w.failure }
func (w *ftpWire) watchRequests(timeout time.Duration, watch *sftpWatch, abort func()) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if timeout <= 0 {
			<-stop
			return
		}
		tick := time.NewTicker(min(time.Second, max(time.Millisecond, timeout/4)))
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tick.C:
				w.mu.Lock()
				expired := false
				for _, r := range w.requests {
					if !r.started.IsZero() && now.Sub(r.started) >= timeout {
						expired = true
						break
					}
				}
				w.mu.Unlock()
				if expired {
					watch.fail(fmt.Errorf("SFTP request timed out after %s; connection closed, remote writes may be incomplete or have completed; no retry performed", timeout), abort)
					return
				}
			}
		}
	}()
	return func() { close(stop); <-done }
}
