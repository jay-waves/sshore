package main

import (
	"encoding/json"
	"testing"
)

func TestApprovalValidation(t *testing.T) {
	for _, ops := range [][]operation{
		nil, {{Kind: "mount", Path: "relative"}}, {{Kind: "mount", Path: "/tmp", Port: 80}},
		{{Kind: "mount", Path: "/tmp"}, {Kind: "upload", Path: "/tmp"}},
		{{Kind: "upload", Path: "/tmp", ReadOnly: true}}, {{Kind: "exec", Path: "/bin/sh"}}, {{Kind: "download", Path: "/tmp/a", Name: "../a", Size: 1}},
		{{Kind: "download", Path: "relative", Name: "a", Size: 1}}, {{Kind: "download", Path: "/a", Name: "a", Size: -1}},
		{{Kind: "forward", Port: 80, Reverse: true, TargetPort: -1}},
		{{Kind: "upload", Path: "/tmp", Reverse: true}},
		{{Kind: "forward", Port: 0}}, {{Kind: "forward", Port: 80, Path: "/a"}},
		{{Kind: "upload", Path: "/tmp", Name: "secret"}},
	} {
		if validateRequestOperations(ops) == nil {
			t.Fatal("accepted", ops)
		}
	}
	if err := validateRequestOperations([]operation{{Kind: "forward", Port: 7777, TargetPort: 6666}}); err != nil {
		t.Fatal("valid remote target rejected:", err)
	}
	if err := validateRequestOperations([]operation{{Kind: "mount", Path: "/tmp", ReadOnly: true}}); err != nil {
		t.Fatal(err)
	}
	// A malicious peer cannot deserialize a chosen local path.
	var op operation
	if err := json.Unmarshal([]byte(`{"kind":"upload","path":"/tmp","LocalPath":"/etc/passwd"}`), &op); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(op)
	if err != nil || string(data) != `{"kind":"upload","path":"/tmp"}` {
		t.Fatalf("unexpected approval payload %s: %v", data, err)
	}
}

type approvalReturnInput struct{ lines []string }

func (i *approvalReturnInput) ReadPrefilled(_ string, _ bool, _ string) (string, error) {
	line := i.lines[0]
	i.lines = i.lines[1:]
	return line, nil
}

func TestApprovalReturnChoice(t *testing.T) {
	for _, tc := range []struct {
		lines []string
		want  bool
	}{
		{[]string{""}, true}, {[]string{"Y"}, true}, {[]string{"n"}, false}, {[]string{"invalid", "N"}, false},
	} {
		got, err := chooseApprovalReturn(&approvalReturnInput{lines: tc.lines})
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %v, %v", tc.lines, got, err)
		}
	}
}
