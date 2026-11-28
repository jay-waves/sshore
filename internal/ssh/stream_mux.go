package ssh

import (
	"encoding/binary"
	"io"
	"net"

	"github.com/xtaci/smux"
)

// Stream is a reliable, ordered child stream. CloseWrite sends EOF while
// allowing the peer to continue sending. WriteTo relays received buffers directly.
type Stream interface {
	net.Conn
	io.WriterTo
	CloseWrite() error
}

type StreamWindow uint8

const (
	SharedStreamWindow StreamWindow = iota
	PerStreamWindow
)

// StreamMux owns a reliable transport and all its child streams.
type StreamMux struct{ session *smux.Session }

func NewStreamMux(transport io.ReadWriteCloser, initiator bool, window StreamWindow) (*StreamMux, error) {
	config := smux.DefaultConfig()
	config.Version = 1
	if window == PerStreamWindow {
		config.Version = 2
	}
	open := smux.Server
	if initiator {
		open = smux.Client
	}
	session, err := open(transport, config)
	if err != nil {
		return nil, err
	}
	return &StreamMux{session}, nil
}

func (m *StreamMux) OpenStream() (Stream, error) {
	stream, err := m.session.OpenStream()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (m *StreamMux) AcceptStream() (Stream, error) {
	stream, err := m.session.AcceptStream()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (m *StreamMux) Close() error { return m.session.Close() }

// Datagram preserves message boundaries over a reliable, ordered stream.
// Use one reader and one writer. The caller owns the stream and its deadlines.
type Datagram struct {
	stream Stream
	header [2]byte
}

func NewDatagram(stream Stream) *Datagram { return &Datagram{stream: stream} }

func (d *Datagram) ReadPacket(payload []byte) (int, error) {
	if _, err := io.ReadFull(d.stream, d.header[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(d.header[:]))
	if n > len(payload) {
		return 0, io.ErrShortBuffer
	}
	return io.ReadFull(d.stream, payload[:n])
}

// DatagramPacket reserves framing space so WritePacket can send one contiguous
// slice. A failed write may have left the packet in the mux's send queue.
type DatagramPacket struct{ frame []byte }

func NewDatagramPacket(size int) *DatagramPacket {
	return &DatagramPacket{frame: make([]byte, size+2)}
}

func (p *DatagramPacket) Payload() []byte { return p.frame[2:] }

func (d *Datagram) WritePacket(packet *DatagramPacket, size int) error {
	if size < 0 || size > len(packet.Payload()) || size > 65535 {
		return io.ErrShortBuffer
	}
	frame := packet.frame[:size+2]
	binary.BigEndian.PutUint16(frame, uint16(size))
	n, err := d.stream.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}
