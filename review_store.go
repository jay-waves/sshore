package main

import (
	"fmt"
	"strings"

	"github.com/jay-waves/sshore/internal/files"
)

const maxRequestItems = 16

type operation struct {
	Kind       string `json:"kind"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Port       int    `json:"port,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
	LocalHint  string `json:"local_hint,omitempty"`
}

type requestRecord struct {
	Connection recentConnection
	PID        int
	ID         string
	Directory  string
	Operations []operation
}

// Port denotes the listener and TargetPort denotes the service.
func (op operation) ServerPort() int {
	if op.Reverse {
		return op.Port
	}
	return op.TargetPort
}

func (op operation) LocalPortPending() bool {
	return op.Kind == "forward" && (op.Port == 0 || op.TargetPort == 0)
}

func validateRequestOperations(ops []operation) error {
	if len(ops) == 0 || len(ops) > maxRequestItems {
		return fmt.Errorf("request must contain 1–%d operations", maxRequestItems)
	}
	var total int64
	for _, op := range ops {
		if len(op.LocalHint) > 4096 || strings.ContainsAny(op.LocalHint, "\x00\r\n") {
			return fmt.Errorf("invalid local endpoint hint")
		}
		if op.ReadOnly && op.Kind != "mount" {
			return fmt.Errorf("unexpected read-only flag")
		}
		switch op.Kind {
		case "download", "upload", "mount":
			// The remote server is Linux, even when the client is Windows.
			if !strings.HasPrefix(op.Path, "/") || len(op.Path) > 4096 || strings.ContainsAny(op.Path, "\x00\r\n") {
				return fmt.Errorf("invalid remote path")
			}
			if op.Port != 0 || op.TargetPort != 0 || op.Reverse {
				return fmt.Errorf("unexpected port in file request")
			}
			if op.Kind == "mount" && len(ops) != 1 {
				return fmt.Errorf("mount requests cannot be batched")
			}
			if op.Kind == "download" {
				if err := files.ValidateName(op.Name); err != nil {
					return err
				}
				if op.Size < 0 || op.Size > (1<<63-1)-total {
					return fmt.Errorf("invalid download size")
				}
				total += op.Size
			} else if op.Name != "" || op.Size != 0 {
				return fmt.Errorf("unexpected upload metadata")
			}
		case "forward":
			if op.LocalHint != "" {
				if _, err := parseForwardPort(op.LocalHint); err != nil {
					return err
				}
			}
			if len(ops) != 1 || op.Port < 0 || op.Port > 65535 || op.TargetPort < 0 || op.TargetPort > 65535 || op.ServerPort() == 0 || op.Path != "" || op.Name != "" || op.Size != 0 {
				return fmt.Errorf("invalid port request")
			}
		default:
			return fmt.Errorf("unsupported requested operation")
		}
	}
	return nil
}

func validRequestID(id string) bool { return validTaskUID(id) }
