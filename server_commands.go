package main

import (
	"encoding/base64"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"os"
	"strings"
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

func renderRemoteCommand(server string, req remoteServiceRequest) string {
	command := "sshore"
	if server != "" && server != command {
		// Explicit executable paths are used for the Linux remote shell.
		command = shellQuote(server)
	}
	req.Protocol, req.Release = remoteServiceProtocol, version
	data, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	command += " " + remoteServiceCommand + " "
	if req.Operation == "daemon-admin" {
		// kill --all can stop an older Linux daemon after reviewing its snapshot.
		return command + shellQuote(string(data))
	}
	return command + "base64:" + base64.RawURLEncoding.EncodeToString(data)
}

func runRemoteService(args []string) error {
	if len(args) != 1 {
		return usage("invalid remote service request")
	}
	data := []byte(args[0])
	encoded, isBase64 := strings.CutPrefix(args[0], "base64:")
	if isBase64 {
		var err error
		data, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return usage("invalid remote service request")
		}
	}
	var req remoteServiceRequest
	if jsonv2.Unmarshal(data, &req, jsonv2.RejectUnknownMembers(isBase64)) != nil ||
		req.Protocol != remoteServiceProtocol || req.Release == "" {
		return usage("invalid remote service request")
	}
	shutdown := req.Operation == "daemon-admin"
	if shutdown == isBase64 {
		// Only the shutdown exchange uses a raw JSON argument.
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
