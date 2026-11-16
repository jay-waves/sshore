//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jay-waves/sshore/internal/files"
	transportio "github.com/jay-waves/sshore/internal/transport"
)

func serverCommand(args []string) error {
	switch args[0] {
	case "forward-watch":
		return runApprovedForwardWatch(args[1:])
	case "cp-agent":
		if len(args) != 1 {
			return usage("cp-agent takes no arguments")
		}
		return files.ServeCopy(&transportio.Pipe{Reader: os.Stdin, Writer: os.Stdout, Closers: []io.Closer{os.Stdin, os.Stdout}})
	case "mount-agent":
		if len(args) != 1 {
			return usage("mount-agent takes no arguments")
		}
		return files.ServeAgent(&transportio.Pipe{Reader: os.Stdin, Writer: os.Stdout, Closers: []io.Closer{os.Stdin, os.Stdout}}, version, func(a files.MountApproval) error { return checkApprovalSource(a.Session, a.PID, a.ID) })
	case "rename-session":
		if len(args) != 4 && len(args) != 5 {
			return usage("rename-session requires old name, new name and client version")
		}
		if err := checkPeerVersion(args[3], "client"); err != nil {
			return err
		}
		return renameSession(args[1], args[2], args[4:]...)
	case "kill-session":
		if len(args) != 3 && len(args) != 4 {
			return usage("kill-session requires a name and client version")
		}
		if err := checkPeerVersion(args[2], "client"); err != nil {
			return err
		}
		return killSession(args[1], args[3:]...)
	case "bootstrap":
		if len(args) != 4 {
			return usage("invalid bootstrap arguments")
		}
		return bootstrap(args[1], args[2], args[3])
	case "bridge", "resume", "daemon":
		if args[0] == "daemon" && len(args) == 1 {
			return runSharedRemoteDaemon()
		}
		if len(args) != 2 {
			return fmt.Errorf("internal command requires a shell name")
		}
		if args[0] == "bridge" || args[0] == "resume" {
			return bridge(args[1], args[0] == "resume")
		}
		return daemon(args[1])
	case "task-watch":
		if len(args) != 2 {
			return usage("invalid task watcher arguments")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		return watchRemoteTask()
	case "task-remove":
		if len(args) != 3 {
			return usage("invalid task removal arguments")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		return removeRemoteTask(args[2])
	case "inventory":
		if len(args) != 2 && (len(args) != 3 || args[2] != "--verbose") {
			return usage("inventory requires client version")
		}
		if e := checkPeerVersion(args[1], "client"); e != nil {
			return e
		}
		inv, e := localSessionInventory(len(args) == 3)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(inv)
	case "sessions":
		if len(args) != 1 {
			return usage("usage: sshore sessions")
		}
		return listSessions()
	case "mount":
		op, err := remoteMountRequest(args[1:])
		if err != nil {
			return err
		}
		op.Path, err = filepath.Abs(op.Path)
		if err != nil {
			return err
		}
		info, err := os.Stat(op.Path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("mount source must be a directory")
		}
		return requestOperations([]operation{op})
	case "forward":
		op, err := remoteForwardRequest(args[1:])
		if err != nil {
			return err
		}
		return requestOperations([]operation{op})
	case "detach":
		if len(args) != 1 {
			return usage("usage: sshore detach")
		}
		return detachHelper()
	case "cp":
		kind, paths, hint, err := remoteCopyRequest(args[1:])
		if err != nil {
			return err
		}
		return requestFilesWithHint(kind, paths, hint)
	}
	return fmt.Errorf("unknown server command")
}
