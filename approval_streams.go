package main

// A shell attachment accepts only client-initiated approved file streams.
// Forwarding has its own SSH transport; obsolete tunnel/control streams cannot
// create listeners or dial services through this connection.
import (
	"context"
	"github.com/xtaci/smux"
	"sync"
	"time"
)

const maxApprovedStreams = 32

func serveApprovedStreams(ctx context.Context, mux *smux.Session) {
	slots := make(chan struct{}, maxApprovedStreams)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		st, e := mux.AcceptStream()
		if e != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			st.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots; st.Close() }()
			abort := context.AfterFunc(ctx, func() { st.SetDeadline(time.Now()); st.Close() })
			defer abort()
			st.SetDeadline(time.Now().Add(requestTimeout))
			m, e := readMessage(st)
			if e == nil && m.Type == "approved_transfer" {
				approvedRemoteTransfer(st, m)
			}
		}()
	}
}
