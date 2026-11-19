package forward

import (
	"io"
	"net"

	"github.com/xtaci/smux"
)

// Take ownership of the connection immediately and of the listener once the
// control stream is established. The control stream always precedes data.
func Open(conn io.ReadWriteCloser, listener net.Listener, target int, initiator bool) (*Forwarder, io.ReadWriteCloser, error) {
	openMux := smux.Server
	if initiator {
		openMux = smux.Client
	}
	mx, err := openMux(conn, muxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	openControl := mx.AcceptStream
	if initiator {
		openControl = mx.OpenStream
	}
	control, err := openControl()
	if err != nil {
		_ = mx.Close()
		return nil, nil, err
	}
	return startMuxForward(mx, listener, target), control, nil
}
