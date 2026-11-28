package main

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/files/dirlist"
	"github.com/jay-waves/sshore/internal/release"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	bolt "go.etcd.io/bbolt"
)

const defaultMountIOTimeout = time.Minute

// Direct and locally approved mounts use the same worker configuration.
type ftpOptions struct {
	ApprovalID                          string
	ResumePID                           int
	Connection                          *recentConnection
	IOTimeoutNanos, StartupTimeoutNanos int64
	Host, Remote, Mountpoint            string
	SSH                                 []string
	ReadOnly, DirectIO, Stats           bool
	PickRemote, PickMountpoint          bool
	Name                                string
}

func validateMountConfig(o ftpOptions) error {
	if err := validSSHDestination(o.Host); err != nil {
		return err
	}
	if o.Host == "here" || o.Host == "origin" || (o.Remote == "" && !o.PickRemote) || strings.ContainsRune(o.Remote, 0) || strings.ContainsRune(o.Mountpoint, 0) {
		return fmt.Errorf("invalid mount endpoints")
	}
	if o.IOTimeoutNanos < 0 || o.StartupTimeoutNanos < 0 {
		return fmt.Errorf("invalid mount timeout")
	}
	if o.Connection != nil || o.ApprovalID != "" || o.ResumePID != 0 {
		if o.PickRemote || o.PickMountpoint {
			return fmt.Errorf("approved mounts already specify their paths")
		}
		if o.Connection == nil || !validTaskUID(o.Connection.Instance) || o.ResumePID <= 0 || !validRequestID(o.ApprovalID) || o.Connection.Host != o.Host {
			return fmt.Errorf("invalid mount approval")
		}
		return validateRecentConnection(*o.Connection)
	}
	if len(o.SSH) != 0 {
		_, _, err := parseSSHOptions(o.SSH)
		return err
	}
	return nil
}

func validateLocalMountpoint(path string) (string, error) {
	path, err := files.ResolveMountpoint(path)
	if err != nil {
		return "", err
	}
	return path, files.ValidateMountpoint(path)
}

func readMountApprovalPath(input reviewPromptReader, hint string) (string, error) {
	return readReviewPath(input, "Mountpoint (Enter on a blank line to auto-assign): ", hint, true, validateLocalMountpoint)
}

var mountHistoryBucket = []byte("mount-visits")

// Mount workers read history once and merge their in-memory visits on unmount.
func mountVisitHistory(account, root string, delta files.VisitHistory) files.VisitHistory {
	if !validMachineID(account) {
		return nil
	}
	state, err := statePath("")
	if err != nil {
		return nil
	}
	key := sha256.Sum256([]byte(account + "\x00" + root))
	const maxHistoryBytes = files.VisitHistoryLimit * (6*4096 + 128)
	var history files.VisitHistory
	err = historyTransaction(state, len(delta) > 0, func(tx *bolt.Tx) error {
		bucket := tx.Bucket(mountHistoryBucket)
		if bucket != nil {
			data := bucket.Get(key[:])
			if len(data) > 0 && len(data) <= maxHistoryBytes {
				if err := json.Unmarshal(data, &history); err != nil || len(history) > files.VisitHistoryLimit {
					history = nil
				}
			}
		}
		if history == nil {
			history = files.VisitHistory{}
		}
		history.Merge(delta, time.Now())
		if len(delta) == 0 {
			return nil
		}
		if len(history) == 0 {
			if bucket != nil {
				return bucket.Delete(key[:])
			}
			return nil
		}
		data, err := marshalBoundedStateJSON("mount visit history", history, maxHistoryBytes)
		if err != nil {
			return err
		}
		bucket, err = tx.CreateBucketIfNotExists(mountHistoryBucket)
		if err != nil {
			return err
		}
		return bucket.Put(key[:], data)
	})
	if err != nil {
		return nil
	}
	return history
}

func executeMount(o ftpOptions) (int, error) {
	if o.PickRemote || o.PickMountpoint {
		if err := requireFileSelectionTerminal(); err != nil {
			return 2, err
		}
	}
	if err := files.CheckMountPlatform(); err != nil {
		return 1, err
	}
	return startResourceBackground(resourceConfig{Mount: &o})
}

func runFTPMount(o ftpOptions, s *resourceService, lifecycle files.MountLifecycle, selector func(context.Context, files.DirectoryReader, string) (string, string, error)) (int, error) {
	if !release.Valid(version) {
		return 1, fmt.Errorf("mount requires a full sshore release, got %q", version)
	}
	c := files.MountConfig{
		Remote: o.Remote, Mountpoint: o.Mountpoint, Version: version, Name: o.Name,
		ReadOnly: o.ReadOnly, DirectIO: o.DirectIO, StartupTimeout: time.Duration(o.StartupTimeoutNanos), IOTimeout: time.Duration(o.IOTimeoutNanos),
		History: mountVisitHistory,
		NotificationsUnavailable: func() {
			fmt.Fprintln(os.Stderr, "File notifications unavailable; using short-lived metadata caches.")
		},
	}
	if o.Stats {
		c.ReportStats = func(stats files.MountStats) {
			_ = json.MarshalEncode(jsontext.NewEncoder(os.Stderr), struct {
				Type string
				files.MountStats
			}{Type: "mount_stats", MountStats: stats})
		}
	}
	c.PeerMetadata, _ = json.Marshal(s.peerTask("export " + s.record.Remote))
	var process *sshio.Process
	defer func() {
		if process != nil {
			_ = process.CloseAndWait()
		}
	}()
	if o.Connection != nil {
		c.Approval = &files.MountApproval{Session: o.Connection.Session, PID: o.ResumePID, ID: o.ApprovalID, Instance: o.Connection.Instance, Path: o.Remote, ReadOnly: o.ReadOnly}
		c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
			request := approvedResourceRequest(o.Connection.Session, o.ResumePID, o.ApprovalID, operation{Kind: "mount", Path: o.Remote, ReadOnly: o.ReadOnly})
			return openIndependentResource(ctx, *o.Connection, request, s)
		}
	} else {
		connection := sshConnectionOptions(options{host: o.Host, ssh: o.SSH})
		auth := s.auth
		connection.Environment = s.baseEnvironment()
		connection.Directory = s.directory
		connection.Confirm, connection.Secret = auth.callbacks()
		if o.PickRemote || o.PickMountpoint {
			c.SelectPaths = selector
		}
		connection = withSSHDiagnostics(connection, os.Stderr, auth)
		connection = remoteService(connection, remoteServiceRequest{Operation: "mount-agent"})
		c.Open = func(ctx context.Context) (io.ReadWriteCloser, error) {
			var err error
			process, err = sshio.Start(ctx, connection, os.Stderr)
			return process, err
		}
	}
	return files.RunMount(c, lifecycle)
}

func runMountWorker(config resourceConfig, read func(any) error, enc *jsontext.Encoder, publish func(resourceRecord) error) (int, error) {
	o := *config.Mount
	if err := validSession(o.Name); err != nil {
		return 1, err
	}
	if err := validateMountConfig(o); err != nil {
		return 1, err
	}
	record := resourceRecord{UID: config.UID, Kind: "mount", Name: o.Name, Host: o.Host, ReadOnly: o.ReadOnly, Direction: "<-"}
	s := &resourceService{record: record, auth: config.Auth}
	ownerDone := make(chan struct{})
	workerDone := make(chan struct{})
	defer close(workerDone)
	requests := make(chan mountPathRequest)
	unmount := make(chan func(error))
	go func() {
		defer close(ownerDone)
		if o.PickRemote || o.PickMountpoint {
			err := readMountSelection(read, func(request mountPathRequest) error {
				select {
				case requests <- request:
					return nil
				case <-workerDone:
					return io.ErrClosedPipe
				}
			})
			if err != nil {
				return
			}
		}
		for {
			var action string
			if err := read(&action); err != nil || action != "kill" {
				return
			}
			select {
			case unmount <- func(err error) {
				var reply resourceReply
				if err != nil {
					reply.Error = err.Error()
				}
				_ = json.MarshalEncode(enc, reply)
			}:
			case <-workerDone:
				return
			}
		}
	}()
	ready := func(root, mountpoint string) error {
		s.record.Remote, s.record.Mountpoint = root, mountpoint
		return publish(s.record)
	}
	lifecycle := files.MountLifecycle{OwnerDone: ownerDone, Ready: ready, Stop: unmount}
	selector := func(ctx context.Context, readDir files.DirectoryReader, home string) (string, string, error) {
		if o.PickRemote {
			go readDir(ctx, home)
		}
		if err := json.MarshalEncode(enc, resourceStartup{Browse: true, Home: home}); err != nil {
			return "", "", err
		}
		for {
			select {
			case request := <-requests:
				if request.Directory == nil {
					return request.Remote, request.Local, nil
				}
				listing := &mountPathListing{}
				listing.Entries, listing.Error = listingEntries(readDir(ctx, *request.Directory))
				if request.Parent != "" {
					listing.ParentEntries, listing.ParentError = listingEntries(readDir(ctx, request.Parent))
				}
				if err := json.MarshalEncode(enc, resourceStartup{Listing: listing}); err != nil {
					return "", "", err
				}
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-ownerDone:
				return "", "", fmt.Errorf("mount startup abandoned")
			}
		}
	}
	return runFTPMount(o, s, lifecycle, selector)
}

type mountPathRequest struct {
	Directory *string
	Parent    string
	Remote    string
	Local     string
}

type mountPathListing struct {
	Entries       []dirlist.Entry
	Error         string
	ParentEntries []dirlist.Entry
	ParentError   string
}

func readMountSelection(read func(any) error, selectPath func(mountPathRequest) error) error {
	for {
		var raw jsontext.Value
		if err := read(&raw); err != nil {
			return err
		}
		var request mountPathRequest
		if raw.Kind() != '{' || json.Unmarshal(raw, &request) != nil {
			return errors.New("invalid mount selection request")
		}
		if err := selectPath(request); err != nil {
			return err
		}
		if request.Directory == nil {
			return nil
		}
	}
}

func (o ftpOptions) selectMountPaths(ctx context.Context, console *termui.LocalConsole, home string, read directoryPairReader) (string, string, error) {
	remote, local := o.Remote, o.Mountpoint
	if o.PickRemote {
		var err error
		picker := pairedFileSelection(home, "Mount directory", o.Host, true, read)
		remote, err = console.PickLF(ctx, picker)
		if err != nil {
			return "", "", err
		}
	}
	if o.PickMountpoint {
		var err error
		local, err = pickLocalMountpoint(ctx, console, "")
		if err != nil {
			return "", "", err
		}
	}
	return remote, local, nil
}

func pickLocalMountpoint(ctx context.Context, console *termui.LocalConsole, dir string) (string, error) {
	picker := termui.LocalFileSelection(dir, "Mountpoint", true)
	picker.Manual = true
	picker.Validate = files.ValidateMountpoint
	point, err := console.PickLF(ctx, picker)
	if !errors.Is(err, termui.ErrLFManual) {
		return point, err
	}
	input, err := newLocalInput()
	if err != nil {
		return "", err
	}
	point, err = readReviewPath(input, "Mountpoint: ", "", false, validateLocalMountpoint)
	return point, errors.Join(err, input.Close())
}
