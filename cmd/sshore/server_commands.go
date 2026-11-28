package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/ports"
	"github.com/jay-waves/sshore/internal/release"
	deployment "github.com/jay-waves/sshore/internal/release/remote"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

const remoteServiceCommand = "service"
const remoteServiceReady = byte(0)

type remoteServiceRequest struct {
	PeerMetadata jsontext.Value `json:"peer_metadata,omitempty"`
	Forward      *ports.Rule    `json:"forward,omitempty"`
	Release      string         `json:"release"`
	Operation    string         `json:"operation"`
	Mode         string         `json:"mode,omitempty"`
	Name         string         `json:"name,omitempty"`
	Directory    string         `json:"directory,omitempty"`
	Instance     string         `json:"instance,omitempty"`
}

func startHostService(ctx context.Context, host string, ssh []string, op string) (*sshio.Process, error) {
	conn := remoteService(sshConnectionOptions(options{host: host, ssh: ssh}), remoteServiceRequest{Operation: op})
	return sshio.Start(ctx, withSSHDiagnostics(conn, os.Stderr, nil), os.Stderr)
}

func remoteService(connection sshio.Options, req remoteServiceRequest) sshio.Options {
	req.Release = version
	connection.OpenCommand = func(ctx context.Context, client *sshio.Client, sibling string, diagnostic io.Writer) (*sshio.Process, error) {
		if sibling != "" {
			return startRemoteService(ctx, client, sibling, req, diagnostic)
		}
		endpoint, err := deploymentEndpointFor(connection, client.Destination())
		if err != nil {
			return nil, err
		}
		if req.Operation == "daemon-admin" {
			if saved := os.Getenv(shutdownDestinationEnv); saved != "" {
				var expected deploymentDestination
				if err := json.Unmarshal([]byte(saved), &expected); err != nil || expected != endpoint.Destination {
					return nil, fmt.Errorf("SSH destination for %s changed since deployment", cleanText(endpoint.Host))
				}
			}
		}
		return endpoint.openService(ctx, client, req, diagnostic)
	}
	return connection
}

func (e deploymentEndpoint) openService(ctx context.Context, client *sshio.Client, req remoteServiceRequest, diagnostic io.Writer) (*sshio.Process, error) {
	id, err := deploymentClientID()
	if err != nil {
		return nil, err
	}
	if !release.Valid(version) && version != "dev" {
		return nil, fmt.Errorf("invalid deployment version %q", version)
	}
	cached, err := e.cached()
	if err != nil {
		return nil, err
	}
	install := req.Operation != "daemon-admin"
	review := req.Operation == "shell" && req.Mode == "new"
	var previous string
	var launchErr error
	var pending *launchDiagnostics
	if cached.Home != "" {
		if review && (!cached.Review || !e.sameRoute(cached)) {
			if err := e.remember(cached.Home, true); err != nil {
				return nil, err
			}
		}
		previous, err = deployment.Command(cached.Home, id, version, remoteServiceCommand)
		if err != nil {
			return nil, err
		}
		pending = &launchDiagnostics{}
		process, err := startRemoteService(ctx, client, previous, req, pending)
		if err == nil {
			pending.forward(diagnostic)
			return process, nil
		}
		if _, exited := errors.AsType[exitStatusError](err); !exited {
			pending.forward(diagnostic)
			return nil, err
		}
		launchErr = err
	}

	track := func(size int64, source io.Reader, send func(io.Reader) error) error {
		label := "Deploy server " + version + " to " + e.Host
		run := func(count *atomic.Int64) error { return send(files.CountReads(source, count)) }
		if relay, ok := diagnostic.(*shellDiagnostics); ok {
			return relay.trackProgress(label, size, run)
		}
		return termui.RunProgress(diagnostic, label, size, run)
	}
	resolved, uploaded, err := deployment.Prepare(ctx, client, id, version, deploymentReleaseBase, install, track)
	if err != nil {
		if pending != nil && !errors.Is(err, deployment.ErrMissing) {
			pending.forward(diagnostic)
			err = errors.Join(launchErr, err)
		}
		return nil, err
	}
	command, err := deployment.Command(resolved, id, version, remoteServiceCommand)
	if err != nil {
		return nil, err
	}
	// An existing executable that failed to start must surface the original
	// error. Retry only after upload or discovery of a different path.
	if command == previous && !uploaded {
		pending.forward(diagnostic)
		return nil, launchErr
	}
	if install {
		if err := e.remember(resolved, review); err != nil {
			return nil, err
		}
	}
	return startRemoteService(ctx, client, command, req, diagnostic)
}

func startRemoteService(ctx context.Context, client *sshio.Client, command string, req remoteServiceRequest, diagnostic io.Writer) (_ *sshio.Process, result error) {
	process, err := client.Start(command, diagnostic)
	if err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(startup, func() { _ = process.Close() })
	defer stop()
	defer func() {
		if result != nil {
			_ = process.CloseAndWait()
		}
	}()
	// Send the request immediately. The first response byte distinguishes an
	// exec failure from an operation that has reached the managed executable.
	writeErr := writeControlJSON(process, req)
	var ready [1]byte
	if _, err = io.ReadFull(process, ready[:]); err != nil {
		if startup.Err() != nil {
			return nil, fmt.Errorf("remote service startup: %w", startup.Err())
		}
		if exit := process.Wait(); exit != nil {
			return nil, fmt.Errorf("remote service startup: %w", exit)
		}
		return nil, fmt.Errorf("remote service did not acknowledge startup: %w", err)
	}
	if ready[0] != remoteServiceReady {
		return nil, fmt.Errorf("invalid remote service startup response")
	}
	if writeErr != nil {
		return nil, writeErr
	}
	return process, nil
}

// Hold early stderr until it is clear whether the attempt needs provisioning.
type launchDiagnostics struct {
	mu  sync.Mutex
	buf bytes.Buffer
	out io.Writer
}

func (d *launchDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.out != nil {
		return d.out.Write(p)
	}
	_, _ = d.buf.Write(p[:min(len(p), (16<<10)-d.buf.Len())])
	return len(p), nil
}

func (d *launchDiagnostics) forward(out io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if out == nil {
		out = io.Discard
	}
	d.out = out
	_, _ = d.buf.WriteTo(out)
}

func runRemoteService(args []string) error {
	if len(args) != 0 {
		return usage("invalid remote service request")
	}
	if deployedStateRoot() == "" {
		return fmt.Errorf("remote service must run from a deployment directory")
	}
	if err := writeFull(os.Stdout, []byte{remoteServiceReady}); err != nil {
		return err
	}
	var req remoteServiceRequest
	if err := readControlJSON(os.Stdin, &req, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid remote service request: %w", err)
	}
	if req.Release == "" {
		return usage("invalid remote service request")
	}
	if req.Operation != "daemon-admin" {
		if err := checkDeploymentAdmission(); err != nil {
			_ = writeMessage(os.Stdout, message{Type: "fatal", SoftwareVersion: version, Error: err.Error()})
			return err
		}
	}
	return serveRemoteRequest(req)
}
