package main

import (
	"strings"
	"testing"
)

func TestFTPOptions(t *testing.T) {
	o, e := parseFTPOptions([]string{"host", "/remote folder", "mount", "--read-only", "--", "-J", "jump"})
	if e != nil || !o.readOnly || o.remote != "/remote folder" {
		t.Fatalf("%+v %v", o, e)
	}
	for _, args := range [][]string{{}, {"host", "/", "mount", "-f"}, {"--", "-bad", "/", "mount"}} {
		if _, e := parseFTPOptions(args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestFTPStatsOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host", "--stats"})
	if err != nil || !o.stats {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestFTPNotifyOptions(t *testing.T) {
	o, err := parseFTPOptions([]string{"host", "--server", "/a path/sshore", "--", "-J", "jump"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(mountSSHArgs(o), " ")
	if o.noWatch || !strings.Contains(args, "mount-agent") || !strings.Contains(args, "-J jump") || strings.Contains(args, "SessionType=subsystem") {
		t.Fatalf("%+v %s", o, args)
	}
	o.noWatch = true
	args = strings.Join(mountSSHArgs(o), " ")
	if !strings.HasSuffix(args, "-s -- host sftp") || strings.Contains(args, "mount-agent") {
		t.Fatal(args)
	}
}

func TestFTPNotifyRecovery(t *testing.T) {
	t.Setenv("SSHORE_DISABLED", "1")
	if _, err := run([]string{"mount-agent"}); err == nil {
		t.Fatal("mount agent allowed in recovery")
	}
}

func TestRemoteMountRequest(t *testing.T) {
	for _, args := range [][]string{{".", "origin:"}, {"--read-only", "/tmp", "origin:/client/mount"}} {
		op, err := remoteMountRequest(args)
		if err != nil || op.Kind != "mount" || op.LocalPath != "" {
			t.Fatalf("%+v %v", op, err)
		}
	}
	for _, args := range [][]string{{"origin:", "."}, {".", "origin:", "--no-watch"}, {".", "origin:", "--foreground"}, {".", "origin:", "--", "-J", "jump"}, {"host:/tmp", "origin:"}, {"-", "origin:"}, {".", "origin:", "extra"}} {
		if _, err := remoteMountRequest(args); err == nil {
			t.Fatal(args)
		}
	}
}

func TestMountApprovalSelection(t *testing.T) {
	input := &stubSetupInput{answers: []string{""}}
	if path, err := readMountApprovalPath(input, ""); err != nil || path != "" {
		t.Fatalf("%q %v", path, err)
	}
	if _, err := readMountApprovalPath(&stubSetupInput{}, ""); err == nil {
		t.Fatal("cancel accepted")
	}
}
