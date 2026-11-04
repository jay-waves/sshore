package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestFileRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, chunkSize*3 + 11} {
		t.Run(string(rune('a'+size%26)), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "中文 source.bin")
			dst := filepath.Join(dir, "target.bin")
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i)
			}
			if err := os.WriteFile(src, data, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := openSending(src)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			r, err := newReceiving(dst, s.size)
			if err != nil {
				t.Fatal(err)
			}
			defer r.close()
			for {
				m, err := s.next(1)
				if err != nil {
					t.Fatal(err)
				}
				if m.Type == "end" {
					if err = r.finish(m.Hash); err != nil {
						t.Fatal(err)
					}
					break
				}
				if err = r.put(m.Data); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(dst)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("round trip failed", err)
			}
		})
	}
}
func TestFileFailureAndNoClobber(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "target")
	r, err := newReceiving(dst, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.put([]byte("bad")); err == nil {
		t.Fatal("accepted excess data")
	}
	r.close()
	r, _ = newReceiving(dst, 2)
	_ = r.put([]byte("ok"))
	if r.finish("bad") == nil {
		t.Fatal("accepted wrong digest")
	}
	if _, err = os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("published corrupt file")
	}
	r, _ = newReceiving(dst, 0)
	if err = os.WriteFile(dst, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nil)
	if r.finish(hex.EncodeToString(sum[:])) == nil {
		t.Fatal("overwrote race-created file")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "keep" {
		t.Fatal("existing file changed")
	}
	if _, err = newReceiving(dst, 0); err == nil {
		t.Fatal("accepted existing file")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
	for _, name := range []string{"../x", "a/b", `a\b`, "CON", "LPT1.txt", "a:b", "a\n", "..", "file."} {
		if safeName(name) == nil {
			t.Fatal("unsafe filename", name)
		}
	}
}
