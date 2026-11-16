package transport

import (
	"reflect"
	"strings"
	"testing"
)

func TestSSHTransportOwnership(t *testing.T) {
	input := []string{"-p", "2222", "-L", "9999:localhost:9", "-o", "RemoteForward=8888 localhost:8", "-D", "7777", "-J", "jump"}
	original := append([]string(nil), input...)
	filtered := WithoutForwarding(input)
	if !reflect.DeepEqual(filtered, []string{"-p", "2222", "-J", "jump"}) || !reflect.DeepEqual(input, original) {
		t.Fatal(filtered, input)
	}
	args := SFTPArgs("work", filtered)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ControlMaster=no") || !strings.Contains(joined, "ClearAllForwardings=yes") || !strings.HasSuffix(joined, "-s -- work sftp") {
		t.Fatal(args)
	}
	session := SessionArgs("work", filtered, "opaque command")
	if session[len(session)-1] != "opaque command" || !strings.Contains(strings.Join(session, " "), "SessionType=default") {
		t.Fatal(session)
	}
}
