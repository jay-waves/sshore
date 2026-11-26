package main

import "io"

// Either direction ending closes the entire task or resource route.
func relayRoute(a, b io.ReadWriteCloser) {
	copied := make(chan struct{}, 2)
	copyStream := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(dst, src)
		copied <- struct{}{}
	}
	go copyStream(a, b)
	go copyStream(b, a)
	<-copied

	// Start both closes independently. A protocol close can block on I/O.
	closed := make(chan struct{})
	go func() {
		_ = b.Close()
		close(closed)
	}()
	_ = a.Close()
	<-closed
	<-copied
}
