package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const remoteServiceCommand = "service"
const remoteServiceProtocol = 1

// This request selects the SSH channel's service before any protocol uses stdin.
type remoteServiceRequest struct {
	Forward    *operation `json:"forward,omitempty"`
	Protocol   int        `json:"protocol"`
	Release    string     `json:"release"`
	Operation  string     `json:"operation"`
	Mode       string     `json:"mode,omitempty"`
	Name       string     `json:"name,omitempty"`
	NewName    string     `json:"new_name,omitempty"`
	Instance   string     `json:"instance,omitempty"`
	ApprovalID string     `json:"approval_id,omitempty"`
	PID        int        `json:"pid,omitempty"`
}

func remoteServiceArgs(req remoteServiceRequest) []string {
	req.Protocol = remoteServiceProtocol
	if req.Release == "" {
		req.Release = version
	}
	data, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return []string{remoteServiceCommand, string(data)}
}

func runRemoteService(args []string) error {
	if len(args) != 1 {
		return usage("invalid remote service request")
	}
	data := []byte(args[0])
	if encoded, ok := strings.CutPrefix(args[0], "base64:"); ok {
		var err error
		data, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return usage("invalid remote service request")
		}
	}
	var req remoteServiceRequest
	if json.Unmarshal(data, &req) != nil ||
		req.Protocol != remoteServiceProtocol || req.Release == "" {
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
