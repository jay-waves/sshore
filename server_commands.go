package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const remoteServiceCommand = "service"
const remoteServiceProtocol = 1

// This request selects the SSH channel's service before any protocol uses stdin.
type remoteServiceRequest struct {
	Protocol    int    `json:"protocol"`
	Release     string `json:"release"`
	Operation   string `json:"operation"`
	Mode        string `json:"mode,omitempty"`
	Name        string `json:"name,omitempty"`
	NewName     string `json:"new_name,omitempty"`
	Instance    string `json:"instance,omitempty"`
	Identity    string `json:"identity,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Diagnostics bool   `json:"diagnostics,omitempty"`
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
	var req remoteServiceRequest
	if len(args) != 1 || json.Unmarshal([]byte(args[0]), &req) != nil ||
		req.Protocol != remoteServiceProtocol || req.Release == "" {
		return usage("invalid remote service request")
	}
	if os.Getenv("SSHORE_DISABLED") == "1" && req.Operation != "inventory" {
		return disabledFeaturesError()
	}
	return serveRemoteRequest(req)
}

func disabledFeaturesError() error {
	return fmt.Errorf("sshore features are disabled in this recovery shell; finish maintenance and reconnect from your local shell")
}
