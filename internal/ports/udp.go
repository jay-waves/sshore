package ports

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jay-waves/sshore/internal/ssh"
)

const (
	udpIdle           = time.Minute
	maxDatagram       = 65507 // IPv4 without IP options.
	maxUDPPeers       = 256
	smallDatagramSize = 2048
)

type udpPacket struct {
	packet *ssh.DatagramPacket
	size   int
}

var udpPacketBuffers = sync.Pool{New: func() any { return ssh.NewDatagramPacket(smallDatagramSize) }}

func queueUDPPacket(data []byte) udpPacket {
	var packet *ssh.DatagramPacket
	if len(data) <= smallDatagramSize {
		packet = udpPacketBuffers.Get().(*ssh.DatagramPacket)
	} else {
		packet = ssh.NewDatagramPacket(len(data))
	}
	return udpPacket{packet, copy(packet.Payload(), data)}
}

func (p udpPacket) release() {
	if len(p.packet.Payload()) == smallDatagramSize {
		udpPacketBuffers.Put(p.packet)
	}
}

func udpLoopback(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

func receiveDatagrams(datagrams *ssh.Datagram, deliver func([]byte) error) error {
	packet := make([]byte, maxDatagram)
	for {
		n, err := datagrams.ReadPacket(packet)
		if err != nil {
			return err
		}
		if err := deliver(packet[:n]); err != nil {
			return err
		}
	}
}

// Each target socket has a distinct source port. Traffic in either direction
// extends its lifetime. Expiry closes the stream and releases the sender's slot.
func relayUDPTarget(stream ssh.Stream, port int) {
	socket, err := net.DialUDP("udp4", nil, udpLoopback(port))
	if err != nil {
		stream.Close()
		return
	}
	socket.SetReadDeadline(time.Now().Add(udpIdle))
	datagrams := ssh.NewDatagram(stream)
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveDatagrams(datagrams, func(packet []byte) error {
			socket.SetReadDeadline(time.Now().Add(udpIdle))
			_, err := socket.Write(packet)
			return err
		})
		socket.Close()
	}()
	packet := ssh.NewDatagramPacket(maxDatagram)
	for {
		n, err := socket.Read(packet.Payload())
		if err != nil {
			break
		}
		socket.SetReadDeadline(time.Now().Add(udpIdle))
		stream.SetWriteDeadline(time.Now().Add(udpIdle))
		if datagrams.WritePacket(packet, n) != nil {
			break
		}
	}
	socket.Close()
	stream.Close()
	<-done
}

func serveUDPListener(socket *net.UDPConn, streams *ssh.StreamMux, workers *sync.WaitGroup) {
	peers := make(map[netip.AddrPort]chan udpPacket)
	var mu sync.Mutex
	frame := make([]byte, maxDatagram)
	for {
		n, addr, err := socket.ReadFromUDPAddrPort(frame)
		if err != nil {
			return
		}
		mu.Lock()
		queue := peers[addr]
		if queue == nil && len(peers) < maxUDPPeers {
			queue = make(chan udpPacket, 8)
			peers[addr] = queue
			workers.Go(func() {
				relayUDPSender(socket, addr, streams, queue)
				mu.Lock()
				delete(peers, addr)
				close(queue)
				mu.Unlock()
				for frame := range queue {
					frame.release()
				}
			})
		}
		// A busy sender may drop datagrams without stalling the other senders.
		if queue != nil && len(queue) < cap(queue) {
			queue <- queueUDPPacket(frame[:n])
		}
		mu.Unlock()
	}
}

func relayUDPSender(socket *net.UDPConn, addr netip.AddrPort, streams *ssh.StreamMux, queue <-chan udpPacket) {
	stream, err := streams.OpenStream()
	if err != nil {
		return
	}
	datagrams := ssh.NewDatagram(stream)
	done := make(chan struct{})
	go func() {
		defer close(done)
		receiveDatagrams(datagrams, func(packet []byte) error {
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
			stream.SetWriteDeadline(time.Now().Add(udpIdle))
			err := datagrams.WritePacket(frame.packet, frame.size)
			if err != nil {
				// An interrupted write may still own the packet buffer.
				stream.Close()
				<-done
				return
			}
			frame.release()
		}
	}
}
