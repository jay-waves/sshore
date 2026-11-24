package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestFrontendMuxLargeStream(t *testing.T) {
	client, server := networkMuxPair(t)
	send, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	receive, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := send.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := receive.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("sshore-mux-data"), 32*1024)
	done := make(chan error, 1)
	go func() { _, err := send.Write(payload); done <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(receive, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("large mux stream changed data")
	}
}

func networkMuxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	a, err := smux.Client(client, relayMuxConfig())
	if err != nil {
		client.Close()
		server.Close()
		t.Fatal(err)
	}
	b, err := smux.Server(server, relayMuxConfig())
	if err != nil {
		a.Close()
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestConnectedHostedShellsUsesAuthenticatedChannel(t *testing.T) {
	id := strings.Repeat("a", 32)
	token := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request taskInventoryRequest
		if r.URL.Path != "/tasks/snapshot" || r.Header.Get("Authorization") != "Bearer "+token ||
			r.Header.Get("Sshore-Version") != version || json.NewDecoder(r.Body).Decode(&request) != nil ||
			request != (taskInventoryRequest{Hosted: true, Detail: "health"}) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(taskInventorySnapshot{Native: map[string]localShellView{}, Shells: []shellView{{Instance: id}}})
	}))
	defer server.Close()
	dialed := 0
	dial := func(ctx context.Context, address string) (net.Conn, error) {
		dialed++
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	rows, err := connectedHostedShells(context.Background(), localShellRecord{Address: server.Listener.Addr().String(), Token: token}, dial)
	if err != nil || dialed != 1 || len(rows) != 1 || rows[0].Instance != id {
		t.Fatalf("hosted task snapshot: %+v, %d channels, %v", rows, dialed, err)
	}
}
