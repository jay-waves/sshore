package files

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
)

type wireTestStream struct {
	io.Reader
	io.Writer
}

func (wireTestStream) Close() error { return nil }

func TestSFTPWirePreservesDirectoryBudgetAcrossPackets(t *testing.T) {
	const id = 1
	var replies bytes.Reader
	wire := NewSFTPWire(wireTestStream{Reader: &replies, Writer: io.Discard})
	wire.maxEntries = 1
	request, payload, err := (&sshfx.ReadDirPacket{Handle: "directory"}).MarshalPacket(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The limiter counts NAME entries before the client parses the entries.
	reply := binary.BigEndian.AppendUint32(nil, 9)
	reply = append(reply, byte(sshfx.PacketTypeName))
	reply = binary.BigEndian.AppendUint32(reply, id)
	reply = binary.BigEndian.AppendUint32(reply, 1)
	for _, want := range []sshfx.PacketType{sshfx.PacketTypeName, sshfx.PacketTypeStatus} {
		if err := wire.WriteSFTPPacket(request, payload); err != nil {
			t.Fatal(err)
		}
		replies.Reset(reply)
		var response sshfx.RawPacket
		if err := response.ReadFrom(wire, nil, 1024); err != nil {
			t.Fatal(err)
		}
		if response.PacketType != want || response.RequestID != id {
			t.Fatalf("response type %v, request ID %d", response.PacketType, response.RequestID)
		}
	}
}

func TestSFTPWireWritesOriginalPayload(t *testing.T) {
	data := []byte("file data")
	writer := &payloadWriter{data: data}
	wire := NewSFTPWire(wireTestStream{Writer: writer})
	header, payload, err := (&sshfx.WritePacket{Handle: "file", Data: data}).MarshalPacket(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteSFTPPacket(header, payload); err != nil {
		t.Fatal(err)
	}
	if !writer.reused {
		t.Fatal("transport did not receive the original data slice")
	}
}

type payloadWriter struct {
	data   []byte
	reused bool
}

func (w *payloadWriter) Write(p []byte) (int, error) {
	if bytes.Equal(p, w.data) {
		w.reused = &p[0] == &w.data[0]
	}
	return len(p), nil
}
