package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

type fragmentedReader struct{ io.Reader }

func (r fragmentedReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(b []byte) (int, error) {
	if len(b) > 2 {
		b = b[:2]
	}
	return w.Buffer.Write(b)
}
func TestProtocolBinaryAndFragmentation(t *testing.T) {
	data := make([]byte, chunkSize)
	for i := range data {
		data[i] = byte(i)
	}
	var b shortWriter
	for _, m := range []message{{Type: "attach", SoftwareVersion: version, Term: "xterm", Cols: 90, Rows: 30}, {Type: "input", Data: data}, {Type: "output", Data: []byte("\x00\xff\x1b[31m")}} {
		if err := writeMessage(&b, m); err != nil {
			t.Fatal(err)
		}
	}
	r := fragmentedReader{&b.Buffer}
	m, err := readMessage(r)
	if err != nil || m.Cols != 90 || m.SoftwareVersion != version {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = readMessage(r)
	if err != nil || m.Type != "input" || !bytes.Equal(m.Data, data) {
		t.Fatal("binary frame mismatch", err)
	}
	m, err = readMessage(r)
	if err != nil || !bytes.Equal(m.Data, []byte("\x00\xff\x1b[31m")) {
		t.Fatal("terminal frame mismatch", err)
	}
}
func TestProtocolRejectsMalformedFrames(t *testing.T) {
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], maxFrame+1)
	for _, b := range [][]byte{header, {4, 0, 0, 0, 0}, {3, 0, 0, 0, 1, 0}, {1, 0, 0, 0, 2, 0}, {0, 0, 0, 0, 0}} {
		if _, err := readMessage(bytes.NewReader(b)); err == nil {
			t.Fatalf("accepted %v", b)
		}
	}
}
func TestOptions(t *testing.T) {
	o, err := parseOptions([]string{"new", "myhost", "build", "--server", "/a b/sshore", "--", "-J", "jump"})
	if err != nil || o.session != "build" || o.host != "myhost" || len(o.ssh) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, name := range []string{"", "../bad", "-bad", "space name", "\u4e2d\u6587"} {
		if validSession(name) == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, args := range [][]string{{"new", "host", "-s"}, {"new", "-evil"}, {"new", "host", "name", "unexpected"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestProtocolUnicodeError(t *testing.T) {
	want := "无法打开 中文文件.txt；literal\\u4e2d.txt"
	var b bytes.Buffer
	if err := writeMessage(&b, message{Type: "error", Error: want}); err != nil {
		t.Fatal(err)
	}
	m, err := readMessage(&b)
	if err != nil || m.Error != want {
		t.Fatalf("got %q, %v; want %q", m.Error, err, want)
	}
}
