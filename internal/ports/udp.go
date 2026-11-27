package ports

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

const (
	udpIdle     = time.Minute
	maxDatagram = 65507 // IPv4 without IP options.
	maxUDPPeers = 256
)

func udpLoopback(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

// A stream belongs to one sender. Prefix each datagram with its length,
// including zero, so adjacent writes never merge into a different packet.
func writeDatagram(stream *smux.Stream, frame []byte) error {
	binary.BigEndian.PutUint16(frame, uint16(len(frame)-2))
	stream.SetWriteDeadline(time.Now().Add(udpIdle))
	_, err := stream.Write(frame)
	return err
}

func receiveDatagrams(stream *smux.Stream, deliver func([]byte) error) error {
	var header [2]byte
	packet := make([]byte, maxDatagram)
	for {
		if _, err := io.ReadFull(stream, header[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(header[:]))
		if n > len(packet) {
			return io.ErrShortBuffer
		}
		if _, err := io.ReadFull(stream, packet[:n]); err != nil {
			return err
		}
		if err := deliver(packet[:n]); err != nil {
			return err
		}
	}
}

// Each target socket has a distinct source port. Traffic in either direction
// extends its lifetime. Expiry closes the stream and releases the sender's slot.
func relayUDPTarget(stream *smux.Stream, port int) {
	socket, err := net.DialUDP("udp4", nil, udpLoopback(port))
	if err != nil {
		stream.Close()
		return
	}
	socket.SetReadDeadline(time.Now().Add(udpIdle))
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveDatagrams(stream, func(packet []byte) error {
			socket.SetReadDeadline(time.Now().Add(udpIdle))
			_, err := socket.Write(packet)
			return err
		})
		socket.Close()
	}()
	frame := make([]byte, maxDatagram+2)
	for {
		n, err := socket.Read(frame[2:])
		if err != nil {
			break
		}
		socket.SetReadDeadline(time.Now().Add(udpIdle))
		if writeDatagram(stream, frame[:2+n]) != nil {
			break
		}
	}
	socket.Close()
	stream.Close()
	<-done
}

func serveUDPListener(socket *net.UDPConn, mux *smux.Session, workers *sync.WaitGroup) {
	peers := make(map[netip.AddrPort]chan []byte)
	var mu sync.Mutex
	frame := make([]byte, maxDatagram+2)
	for {
		n, addr, err := socket.ReadFromUDPAddrPort(frame[2:])
		if err != nil {
			return
		}
		mu.Lock()
		queue := peers[addr]
		if queue == nil && len(peers) < maxUDPPeers {
			queue = make(chan []byte, 8)
			peers[addr] = queue
			workers.Go(func() {
				relayUDPSender(socket, addr, mux, queue)
				mu.Lock()
				delete(peers, addr)
				mu.Unlock()
			})
		}
		// A busy sender may drop datagrams without stalling the other senders.
		if queue != nil && len(queue) < cap(queue) {
			queue <- bytes.Clone(frame[:n+2])
		}
		mu.Unlock()
	}
}

func relayUDPSender(socket *net.UDPConn, addr netip.AddrPort, mux *smux.Session, queue <-chan []byte) {
	stream, err := mux.OpenStream()
	if err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveDatagrams(stream, func(packet []byte) error {
			_, err := socket.WriteToUDPAddrPort(packet, addr)
			return err
		})
		stream.Close()
	}()
	for {
		select {
		case <-done:
			return
		case frame := <-queue:
			if writeDatagram(stream, frame) != nil {
				stream.Close()
				<-done
				return
			}
		}
	}
}
