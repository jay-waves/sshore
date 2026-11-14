package main

import (
	"strings"
	"testing"
)

func TestLocalForwardDirections(t *testing.T) {
	for _, tc := range []struct{ direction, flag, spec string }{
		{"to", "-L", "127.0.0.1:8080:127.0.0.1:3000"},
		{"from", "-R", "127.0.0.1:3000:127.0.0.1:8080"},
	} {
		args, e := localForwardArgs([]string{"work", "8080", tc.direction, "3000"})
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(strings.Join(args, " "), tc.flag+" "+tc.spec) {
			t.Fatal(args)
		}
	}
	for _, args := range [][]string{{"work", "3000"}, {"work", "8080:3000"}, {"work", "8080", "to", "3000", "extra"}} {
		if _, e := localForwardArgs(args); e == nil {
			t.Fatal("accepted", args)
		}
	}
}
