package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestShortTaskShellSelectors(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	local := localShellRecord{Name: "work", Token: strings.Repeat("a", 64), Address: "127.0.0.1:1234", Version: version}
	path, err := localSessionPath(local.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeStateJSON(path, local); err != nil {
		t.Fatal(err)
	}
	remote := shellSnapshot{Host: "host", Name: "build", Instance: "original", State: "detached"}
	if err = saveShellSnapshot(remote); err != nil {
		t.Fatal(err)
	}
	remoteKey := remoteShellIdentity(remote.Host, remote.Instance)
	ids, err := assignTaskIDs([]string{local.identity(), remoteKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		command    string
		args, want []string
		identity   string
	}{
		{"attach", []string{ids[local.identity()].code}, []string{"work"}, local.identity()},
		{"cancel", []string{"--local", ids[local.identity()].code}, []string{"work"}, local.identity()},
		{"rename", []string{ids[local.identity()].code, "new"}, []string{"work", "new"}, local.identity()},
		{"attach", []string{ids[remoteKey].code}, []string{"host", "build"}, remoteKey},
		{"cancel", []string{"host", ids[remoteKey].code}, []string{"host", "build"}, remoteKey},
		{"rename", []string{ids[remoteKey].code, "new"}, []string{"host", "build", "new"}, remoteKey},
		{"rename", []string{"host", ids[remoteKey].code, "new"}, []string{"host", "build", "new"}, remoteKey},
	} {
		parsed, err := parseCLI(tc.args, tc.command)
		if err != nil {
			t.Fatal(err)
		}
		target, ok, err := resolveShortTask(tc.command, &parsed)
		if err != nil || !ok || target.identity != tc.identity || !reflect.DeepEqual(parsed.operands, tc.want) {
			t.Fatalf("%s %v: %+v %v %v", tc.command, tc.args, target, parsed, err)
		}
		o, err := shellOptions(tc.command, parsed)
		if err != nil || o.session == "" {
			t.Fatalf("%+v %v", o, err)
		}
	}
	for _, args := range [][]string{{"other", ids[remoteKey].code}, {"--local", ids[remoteKey].code}} {
		parsed, err := parseCLI(args, "cancel")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = resolveShortTask("cancel", &parsed); err == nil {
			t.Fatal("accepted wrong scope", args)
		}
	}
	// A reused name must never make the old code act on its replacement.
	local.Token = strings.Repeat("b", 64)
	if err = writeStateJSON(path, local); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", ids[localShellRecord{Token: strings.Repeat("a", 64)}.identity()].code}); err == nil {
		t.Fatal("accepted replaced local shell")
	}
	if err = renameShellSnapshot("host", "build", "renamed"); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseCLI([]string{ids[remoteKey].code}, "attach")
	if err != nil {
		t.Fatal(err)
	}
	target, ok, err := resolveShortTask("attach", &parsed)
	if err != nil || !ok || target.name != "renamed" {
		t.Fatalf("%+v %v", target, err)
	}
	remote.Name = "renamed"
	remote.Instance = "replacement"
	if err = saveShellSnapshot(remote); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", ids[remoteKey].code}); err == nil {
		t.Fatal("accepted replaced remote shell")
	}
}

func TestShortTaskReviewLifecycle(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "no-ssh"))
	id, err := writeApproval(options{host: "fixture", server: "sshore", session: "work"}, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	key := resourceIdentity(resourceRecord{UID: id, Kind: "review", Token: id})
	ids, err := assignTaskIDs([]string{key})
	if err != nil {
		t.Fatal(err)
	}
	code := ids[key].code
	parsed, err := parseCLI([]string{code}, "review")
	if err != nil {
		t.Fatal(err)
	}
	target, ok, err := resolveShortTask("review", &parsed)
	if err != nil || !ok || target.requestID != id {
		t.Fatalf("%+v %v", target, err)
	}
	if _, err = run([]string{"attach", code}); err == nil {
		t.Fatal("attached review")
	}
	if _, err = run([]string{"rename", code, "renamed"}); err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", code}); err != nil {
		t.Fatal(err)
	}
	dir, err := approvalDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Fatal("request not consumed", err)
	}
	if _, err = run([]string{"cancel", code}); err == nil {
		t.Fatal("reused ended request")
	}
}

func TestShortTaskResourceRenameAndReplacement(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	for _, kind := range []string{"mount", "forward"} {
		dir, err := resourceStateDir(kind)
		if err != nil {
			t.Fatal(err)
		}
		r := resourceRecord{Kind: kind, Name: kind + "job", Token: strings.Repeat("c", 64), Version: version, Host: "host"}
		path := filepath.Join(dir, r.Name+".json")
		if err = writeStateJSON(path, r); err != nil {
			t.Fatal(err)
		}
		key := resourceIdentity(r)
		ids, err := assignTaskIDs([]string{key})
		if err != nil {
			t.Fatal(err)
		}
		code := ids[key].code
		if _, err = run([]string{"rename", code, kind + "new"}); err != nil {
			t.Fatal(err)
		}
		parsed, err := parseCLI([]string{kind + "/" + code}, "cancel")
		if err != nil {
			t.Fatal(err)
		}
		target, ok, err := resolveShortTask("cancel", &parsed)
		if err != nil || !ok || target.name != kind+"new" {
			t.Fatalf("%+v %v", target, err)
		}
		// Resolve, then replace the storage slot before execution.
		r.Token = strings.Repeat("d", 64)
		if err = writeStateJSON(path, r); err != nil {
			t.Fatal(err)
		}
		if err = cancelResourceIdentity(kind+"/"+r.Name, key); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatal("accepted replacement", err)
		}
		if _, err = run([]string{"rename", code, "other"}); err == nil {
			t.Fatal("renamed replacement by old ID")
		}
	}
}

func TestShortTaskUnknownAndScopedNames(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("SSHORE_SSH", filepath.Join(t.TempDir(), "must-not-connect"))
	for _, args := range [][]string{{"attach", "abcdef"}, {"cancel", "123456"}, {"rename", "ABCDEF", "new"}, {"review", "abcdef"}} {
		if code, err := run(args); code != 2 || err == nil || !strings.Contains(err.Error(), "unknown task ID") {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, args := range [][]string{{"--local", "abcdef"}, {"host", "abcdef"}, {"review/abcdef"}} {
		parsed, err := parseCLI(args, "cancel")
		if err != nil {
			t.Fatal(err)
		}
		if _, selected, err := resolveShortTask("cancel", &parsed); selected || err != nil {
			t.Fatalf("hex name rejected: %v %v", args, err)
		}
	}
	id, err := writeApproval(options{host: "fixture", server: "sshore", session: "work"}, []operation{{Kind: "upload", Path: "/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"rename", "review/" + id, "abcdef"}); err == nil {
		t.Fatal("hex alias accepted")
	}

}

func TestShortTaskLocalCommandRouting(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	r := localShellRecord{Name: "work", Address: listener.Addr().String(), Token: strings.Repeat("e", 64), Version: version}
	path, err := localSessionPath(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeStateJSON(path, r); err != nil {
		t.Fatal(err)
	}
	_, err = assignTaskIDs([]string{r.identity()})
	if err != nil {
		t.Fatal(err)
	}
	// Numeric-only six-character codes are IDs too, not list row numbers.
	indexPath, err := statePath("tasks/ids.json")
	if err != nil {
		t.Fatal(err)
	}
	index, err := readTaskIDIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	index.Codes[r.identity()] = "123456"
	if err = writeStateJSON(indexPath, index); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cancel", "rename"} {
		done := make(chan error, 1)
		go func() {
			c, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			auth, err := readMessage(c)
			if err == nil && (auth.Type != "local_auth" || auth.Token != r.Token) {
				err = fmt.Errorf("wrong authentication: %+v", auth)
			}
			if err == nil {
				err = writeMessage(c, message{Type: "local_ready", SoftwareVersion: version})
			}
			if err == nil {
				var request message
				request, err = readMessage(c)
				if err == nil && (request.Type != command || command == "rename" && request.Name != "new") {
					err = fmt.Errorf("wrong request: %+v", request)
				}
			}
			if err == nil {
				reply := "cancelled"
				if command == "rename" {
					reply = "renamed"
				}
				err = writeMessage(c, message{Type: reply})
			}
			done <- err
		}()
		args := []string{command, "123456"}
		if command == "rename" {
			args = append(args, "new")
		}
		code, err := run(args)
		if code != 0 || err != nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestShortTaskRemoteHistoryPrefersCurrentName(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	now := time.Now()
	for _, s := range []shellSnapshot{
		{Host: "work", Name: "old", Instance: "instance", State: "detached", ConfirmedAt: now},
		{Host: "work", Name: "older", Instance: "instance", State: "detached", ConfirmedAt: now.Add(-time.Hour)},
	} {
		if err := saveShellSnapshot(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := cacheRemoteShells("work", inventory{Shells: []shellView{{Name: "new", Instance: "instance", State: "detached"}}}); err != nil {
		t.Fatal(err)
	}
	target, err := findTaskIdentity(remoteShellIdentity("work", "instance"))
	if err != nil || target.name != "new" {
		t.Fatalf("%+v %v", target, err)
	}
}

func TestShortTaskRejectsInvalidIndex(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir, err := stateDir("tasks")
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []taskIDIndex{
		{Next: 1, IDs: map[string]uint64{}, Codes: map[string]string{"peer:one": "abcdef"}},
		{Next: 3, IDs: map[string]uint64{"peer:one": 1, "peer:two": 2}, Codes: map[string]string{"peer:one": "abcdef", "peer:two": "abcdef"}},
	} {
		if err = writeStateJSON(filepath.Join(dir, "ids.json"), index); err != nil {
			t.Fatal(err)
		}
		if _, err = run([]string{"cancel", "abcdef"}); err == nil || !strings.Contains(err.Error(), "invalid task short ID index") {
			t.Fatal("invalid index accepted", err)
		}
	}
}

func TestShortTaskRejectsInvalidConnection(t *testing.T) {
	t.Setenv("SSHORE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	snapshot := shellSnapshot{Host: "work", Name: "job", Instance: "instance", State: "detached", Connection: &recentConnection{Host: "other", Session: "job", Server: "sshore"}}
	if err := saveShellSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	ids, err := assignTaskIDs([]string{remoteShellIdentity("work", "instance")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run([]string{"cancel", ids[remoteShellIdentity("work", "instance")].code}); err == nil || !strings.Contains(err.Error(), "invalid shell connection host") {
		t.Fatal("mismatched route accepted", err)
	}
}
