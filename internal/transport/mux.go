package transport

import (
	"time"

	"github.com/xtaci/smux"
)

const MaxFileStreams = 32

func MuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 * 1024
	c.MaxStreamBuffer = 256 * 1024
	// Keep room for terminal, files and control requests. Independent forwards
	// and mounts use separate SSH connections and do not consume this budget.
	// Stalled streams must not exhaust the shared receive budget and block input.
	c.MaxReceiveBuffer = (MaxFileStreams + 12) * c.MaxStreamBuffer
	c.KeepAliveInterval = 10 * time.Second
	c.KeepAliveTimeout = 60 * time.Second
	return c
}
