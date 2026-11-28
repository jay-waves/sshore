package termui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckLFText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample")
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{name: "empty"},
		{name: "UTF-8", data: []byte("汉字\t\r\n")},
		{name: "UTF-16 LE", data: []byte{0xff, 0xfe, 'a', 0, '\n', 0}},
		{name: "UTF-16 BE", data: []byte{0xfe, 0xff, 0, 'a', 0, '\n'}},
		{name: "binary", data: []byte("abc\x00def"), want: errLFNonText},
		{name: "binary control", data: []byte{0x7f}, want: errLFNonText},
		{name: "prefix only", data: append(bytes.Repeat([]byte{'a'}, 4096), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkLFText(path); !errors.Is(err, tc.want) {
				t.Fatalf("checkLFText = %v, want %v", err, tc.want)
			}
		})
	}
}
