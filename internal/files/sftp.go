package files

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	sftp "github.com/pkg/sftp/v2"
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
	sftpCopyConcurrency  = 64
	ftpHandshakeRequest  = uint64(1) << 32 // INIT and VERSION have no request ID.
)

type ftpRequest struct {
	started time.Time
	kind    byte
	budget  *ftpDirectoryBudget
}
type ftpDirectoryBudget struct{ entries, bytes uint64 }
type ftpWire struct {
	onReply                       func()
	totals                        *[256]ftpRequestTotals
	peakRequests                  int
	reader                        io.Reader
	writer                        io.WriteCloser
	readBuffer, writeBuffer       []byte
	readOffset                    int
	mu                            sync.Mutex
	requests                      map[uint64]ftpRequest
	requestWake                   chan struct{} // idle/busy transition
	directories                   map[string]*ftpDirectoryBudget
	maxEntries, maxDirectoryBytes uint64
}

func newFTPWire(r io.Reader, w io.WriteCloser) *ftpWire {
	return &ftpWire{reader: r, writer: w, requests: make(map[uint64]ftpRequest), maxEntries: ftpMaxDirectoryEntries, maxDirectoryBytes: ftpMaxDirectoryBytes}
}

// Enable counters before passing the wire to the SFTP client.
func (w *ftpWire) enableMetrics() { w.totals = new([256]ftpRequestTotals) }

func NewCopyClient(ctx context.Context, stream io.ReadWriteCloser) (*sftp.Client, error) {
	wire := newFTPWire(stream, stream)
	return sftp.NewClientPipe(ctx, wire, wire, sftp.WithMaxInflight(sftpCopyConcurrency))
}
func (w *ftpWire) Close() error { return w.writer.Close() }
func ftpWireString(b []byte) ([]byte, bool) {
	if len(b) < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32(b))
	if n > uint64(len(b)-4) {
		return nil, false
	}
	return b[4 : 4+int(n)], true
}
func (w *ftpWire) record(packet []byte) error {
	if len(packet) < 5 {
		return fmt.Errorf("short outgoing SFTP packet")
	}
	kind := packet[0]
	id := binary.BigEndian.Uint32(packet[1:5])
	key := uint64(id)
	if kind == 1 {
		key = ftpHandshakeRequest
	}
	var handle []byte
	if kind == 12 || kind == 4 {
		var ok bool
		handle, ok = ftpWireString(packet[5:])
		if !ok {
			return fmt.Errorf("invalid SFTP handle")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) >= ftpMaxRequests {
		return fmt.Errorf("too many outstanding SFTP requests")
	}
	if _, exists := w.requests[key]; exists {
		return fmt.Errorf("duplicate outstanding SFTP request ID %d", id)
	}
	req := ftpRequest{kind: kind, started: time.Now()}
	switch kind {
	case 12: // Bind the reply to this enumeration even if CLOSE races with it.
		req.budget = w.directories[string(handle)]
		if req.budget == nil {
			req.budget = &ftpDirectoryBudget{}
			if w.directories == nil {
				w.directories = make(map[string]*ftpDirectoryBudget)
			}
			w.directories[string(handle)] = req.budget
		}
	case 4:
		delete(w.directories, string(handle))
	}
	empty := len(w.requests) == 0
	w.requests[key] = req
	if empty {
		w.wakeRequestWatchLocked()
	}
	if w.totals != nil && kind != 1 {
		w.totals[kind].Started++
		w.totals[kind].SentBytes += uint64(len(packet) + 4)
		w.peakRequests = max(w.peakRequests, len(w.requests))
	}
	return nil
}
func (w *ftpWire) wakeRequestWatchLocked() {
	select {
	case w.requestWake <- struct{}{}:
	default:
	}
}

func (w *ftpWire) Write(p []byte) (int, error) {
	// pkg/sftp serializes writes, but can split a frame between header and data.
	accepted := 0
	for len(p) > 0 {
		if len(w.writeBuffer) < 4 {
			take := min(4-len(w.writeBuffer), len(p))
			w.writeBuffer = append(w.writeBuffer, p[:take]...)
			p = p[take:]
			accepted += take
			if len(w.writeBuffer) < 4 {
				break
			}
		}
		n := binary.BigEndian.Uint32(w.writeBuffer[:4])
		if n < 5 || n > ftpMaxPacket {
			return accepted, fmt.Errorf("invalid SFTP request size %d", n)
		}
		frameSize := 4 + int(n)
		take := min(frameSize-len(w.writeBuffer), len(p))
		w.writeBuffer = append(w.writeBuffer, p[:take]...)
		p = p[take:]
		accepted += take
		if len(w.writeBuffer) < frameSize {
			break
		}
		if e := w.record(w.writeBuffer[4:]); e != nil {
			return accepted, e
		}
		if e := writeFull(w.writer, w.writeBuffer); e != nil {
			return accepted, e
		}
		w.writeBuffer = w.writeBuffer[:0]
	}
	return accepted, nil
}
func ftpDirectoryFailure(id uint32) []byte {
	const msg = "directory exceeds sshore listing limit"
	b := make([]byte, 1, 17+len(msg))
	b[0] = 101 // SSH_FXP_STATUS
	b = binary.BigEndian.AppendUint32(b, id)
	b = binary.BigEndian.AppendUint32(b, 4) // SSH_FX_FAILURE
	b = binary.BigEndian.AppendUint32(b, uint32(len(msg)))
	b = append(b, msg...)
	return binary.BigEndian.AppendUint32(b, 0) // language
}

// A non-nil result replaces the response sent to pkg/sftp.
func (w *ftpWire) filter(packet []byte) ([]byte, error) {
	if len(packet) < 5 {
		return nil, fmt.Errorf("short SFTP response")
	}
	id := binary.BigEndian.Uint32(packet[1:5])
	key := uint64(id)
	if packet[0] == 2 {
		key = ftpHandshakeRequest
	} else if w.onReply != nil {
		w.onReply()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	req, ok := w.requests[key]
	if !ok {
		return nil, nil
	} // pkg/sftp diagnoses unsolicited IDs.
	delete(w.requests, key)
	if len(w.requests) == 0 {
		w.wakeRequestWatchLocked()
	}
	overBudget := false
	if req.budget != nil && packet[0] == 104 { // READDIR -> NAME
		if len(packet) < 9 {
			return nil, fmt.Errorf("short SFTP directory response")
		}
		budget := req.budget
		budget.entries += uint64(binary.BigEndian.Uint32(packet[5:9]))
		budget.bytes += uint64(len(packet))
		overBudget = budget.entries > w.maxEntries || budget.bytes > w.maxDirectoryBytes
	}
	if w.totals != nil && key != ftpHandshakeRequest {
		total := &w.totals[req.kind]
		elapsed := time.Since(req.started)
		total.Completed++
		total.TotalNS += int64(elapsed)
		total.MaxNS = max(total.MaxNS, int64(elapsed))
		total.ReceivedBytes += uint64(len(packet) + 4)
		if overBudget || packet[0] == 101 && len(packet) >= 9 && binary.BigEndian.Uint32(packet[5:9]) > 1 {
			total.Errors++
		}
	}
	if overBudget {
		return ftpDirectoryFailure(id), nil
	}
	return nil, nil
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
		replacement, e := w.filter(w.readBuffer[4:])
		if e != nil {
			return 0, e
		}
		if replacement != nil {
			w.readBuffer = binary.BigEndian.AppendUint32(w.readBuffer[:0], uint32(len(replacement)))
			w.readBuffer = append(w.readBuffer, replacement...)
		} else {
			copy(w.readBuffer, header[:])
		}
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

func (w *ftpWire) watchRequests(ctx context.Context, timeout time.Duration) error {
	wake := make(chan struct{}, 1)
	w.mu.Lock()
	w.requestWake = wake
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.requestWake = nil
		w.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		w.mu.Lock()
		var oldest ftpRequest
		for _, request := range w.requests {
			if oldest.started.IsZero() || request.started.Before(oldest.started) {
				oldest = request
			}
		}
		expires := oldest.started.Add(timeout)
		expired := !oldest.started.IsZero() && !time.Now().Before(expires)
		w.mu.Unlock()

		if expired {
			if oldest.kind == 1 {
				return fmt.Errorf("SFTP handshake timed out after %s", timeout)
			}
			return fmt.Errorf("SFTP request timed out after %s; connection closed, remote writes may be incomplete or have completed; no retry performed", timeout)
		}
		// New requests cannot make an existing deadline sooner.
		var deadline <-chan time.Time
		if !oldest.started.IsZero() {
			timer.Reset(time.Until(expires))
			deadline = timer.C
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			timer.Stop()
		case <-deadline:
		}
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
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
