package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestFTPRequestStats(t *testing.T) {
	wire := newFTPWire(nil, nil)
	wire.metrics = true
	for _, id := range []uint32{1, 2} {
		packet := make([]byte, 5)
		packet[0] = 17
		binary.BigEndian.PutUint32(packet[1:], id)
		if err := wire.record(packet); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uint32{2, 1} {
		packet := make([]byte, 9)
		packet[0] = 101
		binary.BigEndian.PutUint32(packet[1:], id)
		if id == 2 {
			binary.BigEndian.PutUint32(packet[5:], 3)
		}
		if _, err := wire.filter(packet); err != nil {
			t.Fatal(err)
		}
	}
	totals := wire.totals[17]
	if totals.Started != 2 || totals.Completed != 2 || totals.Errors != 1 || totals.SentBytes != 18 || totals.ReceivedBytes != 26 || totals.TotalNS < totals.MaxNS || wire.peakRequests != 2 {
		t.Fatalf("%+v peak=%d", totals, wire.peakRequests)
	}
	backend := &ftpFS{metrics: &ftpCallbackMetrics{}}
	backend.metrics.observe("Open")()
	var output bytes.Buffer
	writeFTPStats(wire, backend, &output)
	var report struct {
		Type      string
		Requests  map[string]ftpRequestTotals
		Callbacks map[string]ftpCallbackTotals
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Type != "mount_stats" || report.Requests["STAT"].Completed != 2 || report.Callbacks["Open"].Calls != 1 {
		t.Fatalf("%s", output.Bytes())
	}
}
func TestFTPStatsOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host", "--stats"})
	if err != nil || !o.stats {
		t.Fatalf("%+v %v", o, err)
	}
	var output bytes.Buffer
	stop := startFTPStats(newFTPWire(nil, nil), &ftpFS{}, &output)
	stop()
	if output.Len() == 0 {
		t.Fatal("missing final statistics")
	}
}
