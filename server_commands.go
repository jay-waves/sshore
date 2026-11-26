package main

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"

	sshio "github.com/jay-waves/sshore/internal/ssh"
)

const remoteServiceCommand = "service"
const remoteServiceProtocol = 1

type remoteServiceRequest struct {
	PeerMetadata jsontext.Value        `json:"peer_metadata,omitempty"`
	Forward      *remoteForwardRequest `json:"forward,omitempty"`
	Protocol     int                   `json:"protocol"`
	Release      string                `json:"release"`
	Operation    string                `json:"operation"`
	Mode         string                `json:"mode,omitempty"`
	Name         string                `json:"name,omitempty"`
	Directory    string                `json:"directory,omitempty"`
	Instance     string                `json:"instance,omitempty"`
}

type remoteForwardRequest struct {
	Kind       string `json:"kind"`
	Port       int    `json:"port,omitzero"`
	TargetPort int    `json:"target_port,omitzero"`
	Reverse    bool   `json:"reverse,omitzero"`
}

func remoteExecutable(server string) string {
	if server == "" || server == "sshore" {
		return "sshore"
	}
	return shellQuote(server)
}

func remoteService(connection sshio.Options, server string, req remoteServiceRequest) sshio.Options {
	connection.Command = remoteExecutable(server) + " " + remoteServiceCommand
	req.Protocol, req.Release = remoteServiceProtocol, version
	connection.WriteRequest = func(w io.Writer) error {
		return writeControlJSON(w, req)
	}
	return connection
}

func runRemoteService(args []string) error {
	if len(args) != 0 {
		return usage("invalid remote service request")
	}
	var req remoteServiceRequest
	if err := readControlJSON(os.Stdin, &req, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid remote service request: %w", err)
	}
	if req.Protocol != remoteServiceProtocol || req.Release == "" {
		return usage("invalid remote service request")
	}
	if os.Getenv("SSHORE_DISABLED") == "1" {
		return disabledFeaturesError()
	}
	return serveRemoteRequest(req)
}

func disabledFeaturesError() error {
	return fmt.Errorf("sshore features are disabled in this recovery shell; finish maintenance and reconnect from your local shell")
}
