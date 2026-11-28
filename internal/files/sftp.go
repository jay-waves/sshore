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
	totals                        *[256]ftpRequestTotals
	peakRequests                  int
	reader                        io.Reader
	writer                        io.WriteCloser
	mu                            sync.Mutex
	requests                      map[uint64]ftpRequest
	requestWake                   chan struct{} // idle/busy transition
	directories                   map[string]*ftpDirectoryBudget
	maxEntries, maxDirectoryBytes uint64
}

// NewSFTPWire tracks SFTP requests and enforces directory response budgets.
func NewSFTPWire(stream io.ReadWriteCloser) *ftpWire {
	return &ftpWire{reader: stream, writer: stream, requests: make(map[uint64]ftpRequest), maxEntries: ftpMaxDirectoryEntries, maxDirectoryBytes: ftpMaxDirectoryBytes}
}

// Enable counters before passing the wire to the SFTP client.
func (w *ftpWire) enableMetrics() { w.totals = new([256]ftpRequestTotals) }

func NewCopyClient(ctx context.Context, stream io.ReadWriteCloser) (*sftp.Client, error) {
	wire := NewSFTPWire(stream)
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
func (w *ftpWire) record(packet []byte, size uint32) error {
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
		w.totals[kind].SentBytes += uint64(size) + 4
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

// Write handles the complete INIT frame. The client uses WriteSFTPPacket
// for requests whose header and file payload are separate slices.
func (w *ftpWire) Write(p []byte) (int, error) {
	if err := w.WriteSFTPPacket(p, nil); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *ftpWire) WriteSFTPPacket(header, payload []byte) error {
	if len(header) < 9 {
		return fmt.Errorf("short SFTP request header")
	}
	size := binary.BigEndian.Uint32(header[:4])
	if size > ftpMaxPacket || uint64(size) != uint64(len(header)-4)+uint64(len(payload)) {
		return fmt.Errorf("invalid SFTP request size %d", size)
	}
	if err := w.record(header[4:], size); err != nil {
		return err
	}
	if err := writeFull(w.writer, header); err != nil {
		return err
	}
	return writeFull(w.writer, payload)
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

// FilterSFTPPacket inspects the decoder buffer and substitutes a status only
// when a directory listing exceeds its wire budget.
func (w *ftpWire) FilterSFTPPacket(packet []byte) ([]byte, error) {
	if len(packet) < 5 {
		return nil, fmt.Errorf("short SFTP response")
	}
	id := binary.BigEndian.Uint32(packet[1:5])
	key := uint64(id)
	if packet[0] == 2 {
		key = ftpHandshakeRequest
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	req, ok := w.requests[key]
	if !ok {
		return packet, nil
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
	return packet, nil
}
func (w *ftpWire) Read(p []byte) (int, error) { return w.reader.Read(p) }

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
