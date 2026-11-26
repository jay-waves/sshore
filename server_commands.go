package main

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"

	"github.com/jay-waves/sshore/internal/jsonstream"
	sshio "github.com/jay-waves/sshore/internal/ssh"
)

const remoteServiceCommand = "service"
const remoteServiceProtocol = 1

// This is the on-the-wire SSH command argument. Service handlers receive their
// own arguments after dispatch.
type remoteServiceRequest struct {
	Forward           *remoteForwardRequest `json:"forward,omitempty"`
	Protocol          int                   `json:"protocol"`
	Release           string                `json:"release"`
	Operation         string                `json:"operation"`
	Mode              string                `json:"mode,omitempty"`
	Name              string                `json:"name,omitempty"`
	NewName           string                `json:"new_name,omitempty"`
	Instance          string                `json:"instance,omitempty"`
	OpenIfUnavailable bool                  `json:"open_if_unavailable,omitempty"`
}

type remoteForwardRequest struct {
	Kind       string `json:"kind"`
	Port       int    `json:"port,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
}

func remoteService(connection sshio.Options, server string, req remoteServiceRequest) sshio.Options {
	connection.Command = "sshore"
	if server != "" && server != connection.Command {
		connection.Command = shellQuote(server)
	}
	connection.Command += " " + remoteServiceCommand
	req.Protocol, req.Release = remoteServiceProtocol, version
	connection.WriteRequest = func(w io.Writer) error {
		return jsonstream.NewEncoder(w).Encode(req)
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
