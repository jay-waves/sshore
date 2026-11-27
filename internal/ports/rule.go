package ports

import "fmt"

// Ports are relative to the client that owns the forward.
type Rule struct {
	LocalPort  int  `json:"local_port,omitzero"`
	RemotePort int  `json:"remote_port"`
	UDP        bool `json:"udp,omitzero"`
	Reverse    bool `json:"reverse,omitzero"`
}

func (r Rule) Validate(allowUnselectedLocal bool) error {
	if r.RemotePort < 1 || r.RemotePort > 65535 || r.LocalPort < 0 ||
		r.LocalPort > 65535 || (r.LocalPort == 0 && !allowUnselectedLocal) {
		return fmt.Errorf("invalid forward ports")
	}
	return nil
}

func (r Rule) ports(remote bool) (listen, target int) {
	port := r.LocalPort
	if remote {
		port = r.RemotePort
	}
	if remote == r.Reverse {
		return port, 0
	}
	return 0, port
}
