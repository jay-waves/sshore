package review

import "time"

const TTL = 10 * time.Minute
const MaxItems = 16
const MaxPending = 32
const MaxRecord = 128 * 1024

type Operation struct {
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Port       int    `json:"port,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
	LocalHint  string `json:"local_hint,omitempty"`
	LocalPath  string `json:"-"`
}

type Record struct {
	PID                                           int
	SoftwareVersion                               string
	ID                                            string
	Name                                          string
	Created                                       time.Time
	Host, Session, Server, Directory, DownloadDir string
	SSH                                           []string
	Quiet, NoEscape                               bool
	Operations                                    []Operation
}

func (r Record) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// Port always denotes the listener, TargetPort the service, including pending requests.
func (op Operation) ServerPort() int {
	if op.Reverse {
		return op.Port
	}
	return op.TargetPort
}

func (op Operation) LocalPortPending() bool {
	return op.Kind == "forward" && (op.Port == 0 || op.TargetPort == 0)
}
