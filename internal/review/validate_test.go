package review

import (
	"encoding/json"
	"testing"
)

func TestApprovalValidation(t *testing.T) {
	for _, ops := range [][]Operation{
		nil, {{Kind: "exec", Path: "/bin/sh"}}, {{Kind: "download", Path: "/tmp/a", Name: "../a", Size: 1}},
		{{Kind: "download", Path: "relative", Name: "a", Size: 1}}, {{Kind: "download", Path: "/a", Name: "a", Size: -1}},
		{{Kind: "forward", Port: 80, Reverse: true, TargetPort: -1}},
		{{Kind: "upload", Path: "/tmp", Reverse: true}},
		{{Kind: "forward", Port: 0}}, {{Kind: "forward", Port: 80, Path: "/a"}},
		{{Kind: "upload", Path: "/tmp", Name: "secret"}},
	} {
		if ValidateOperations(ops) == nil {
			t.Fatal("accepted", ops)
		}
	}
	if err := ValidateOperations([]Operation{{Kind: "forward", Port: 7777, TargetPort: 6666}}); err != nil {
		t.Fatal("valid remote target rejected:", err)
	}
	// A malicious peer cannot deserialize a chosen local path.
	var op Operation
	if err := json.Unmarshal([]byte(`{"kind":"upload","path":"/tmp","LocalPath":"/etc/passwd"}`), &op); err != nil {
		t.Fatal(err)
	}
	if op.LocalPath != "" {
		t.Fatal("remote selected local path")
	}
}
