package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/jay-waves/sshore/internal/files"
)

const maxRequestItems = 16

type operation struct {
	Kind      string      `json:"kind"`
	LF        bool        `json:"lf,omitempty"`
	Recursive bool        `json:"recursive,omitempty"`
	ReadOnly  bool        `json:"read_only,omitempty"`
	Path      string      `json:"path,omitempty"`
	Name      string      `json:"name,omitempty"`
	Size      int64       `json:"size,omitempty"`
	Forward   forwardRule `json:"forward,omitzero"`
	LocalHint string      `json:"local_hint,omitempty"`
}

type requestRecord struct {
	Connection recentConnection
	PID        int
	ID         string
	Directory  string
	Operations []operation
}

func validateOperations(ops []operation) error {
	if len(ops) == 0 || len(ops) > maxRequestItems {
		return fmt.Errorf("request must contain 1–%d operations", maxRequestItems)
	}
	var total int64
	for _, op := range ops {
		if len(op.LocalHint) > 4096 || strings.ContainsAny(op.LocalHint, "\x00\r\n") {
			return fmt.Errorf("invalid local endpoint hint")
		}
		if op.LF && op.Kind != "mount" && (op.Kind != "upload" || len(ops) != 1 || op.LocalHint != "") {
			return fmt.Errorf("unexpected LF flag")
		}
		if op.ReadOnly && op.Kind != "mount" {
			return fmt.Errorf("unexpected read-only flag")
		}
		if op.Recursive && (op.Kind != "download" && op.Kind != "upload" || op.Size != 0) {
			return fmt.Errorf("invalid recursive copy request")
		}
		switch op.Kind {
		case "download", "upload", "mount":
			// Paths are slash-prefixed on the wire on both host platforms.
			if !strings.HasPrefix(op.Path, "/") || len(op.Path) > 4096 || strings.ContainsAny(op.Path, "\x00\r\n") {
				return fmt.Errorf("invalid remote path")
			}
			if op.Forward != (forwardRule{}) {
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
			if err := validateForwardProposal(op, len(ops)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported requested operation")
		}
	}
	return nil
}

func validRequestID(id string) bool { return validTaskUID(id) }

// Only one helper can hand a request to the attached client at a time.
type approvalExchanges struct {
	mu       sync.Mutex
	token    string
	response chan message
	ready    bool
}

func (a *approvalExchanges) enable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = true
}

func (a *approvalExchanges) disable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = false
}

func (a *approvalExchanges) open(token string) (<-chan message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready {
		return nil, fmt.Errorf("attached client unavailable for approval")
	}
	if a.response != nil {
		return nil, fmt.Errorf("another approval is in progress")
	}
	a.token = token
	a.response = make(chan message, 1)
	return a.response, nil
}
func (a *approvalExchanges) remove(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.token, a.response = "", nil
	}
}
func (a *approvalExchanges) reply(m message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != m.Token {
		return
	}
	select {
	case a.response <- m:
	default:
	}
}
