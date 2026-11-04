package main

import (
	"bytes"
	"io"
	"testing"
)

func TestConsoleInputPreservesControlKeysAndUnicode(t *testing.T) {
	batches := [][]uint16{{'A', 0x1a, 3, 0x1d, 0x4e2d, 0xd83d}, {0xde80, 'Z'}}
	r := &consoleInput{readUnits: func(b []uint16) (int, error) {
		if len(batches) == 0 {
			return 0, io.EOF
		}
		n := copy(b, batches[0])
		batches = batches[1:]
		return n, nil
	}}
	var got bytes.Buffer
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		got.Write(one[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := "A\x1a\x03\x1d中🚀Z"; got.String() != want {
		t.Fatalf("got %q want %q", got.String(), want)
	}
}
func TestConsoleInputTruncatedSurrogate(t *testing.T) {
	once := false
	r := &consoleInput{readUnits: func(b []uint16) (int, error) {
		if once {
			return 0, io.EOF
		}
		once = true
		b[0] = 0xd83d
		return 1, nil
	}}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "�" {
		t.Fatalf("%q %v", got, err)
	}
}
