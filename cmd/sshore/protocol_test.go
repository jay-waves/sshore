package main

import (
	"bytes"
	json "encoding/json/v2"
	"strings"
	"testing"
)

func TestInlineControlPayloads(t *testing.T) {
	const frame = `{"type":"console_files","cols":80,"parent_directory":"/tmp","host_info":{"Hostname":"node-07"},"ssh_server_ip":"10.0.0.12"}`
	m, err := readMessage(strings.NewReader(frame + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Cols != 80 || m.ConsoleFiles == nil || m.ConsoleFiles.ParentDirectory != "/tmp" || m.Host == nil || m.Host.SSHServer != "10.0.0.12" || m.advertisedHost().Hostname != "node-07" {
		t.Fatalf("incorrect control payloads: %+v", m)
	}
	var out bytes.Buffer
	if err := writeMessage(&out, m); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 || fields["cols"] != float64(80) || fields["parent_directory"] != "/tmp" || fields["ssh_server_ip"] != "10.0.0.12" || fields["host_info"] == nil {
		t.Fatalf("expected flat control fields, got %s", out.Bytes())
	}
	out.Reset()
	if err := writeMessage(&out, message{Type: "bye"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "{\"type\":\"bye\"}\n" {
		t.Fatalf("empty payloads changed the frame: %s", got)
	}
}
