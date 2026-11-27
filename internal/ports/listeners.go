package ports

import (
	"cmp"
	"context"
	"slices"
	"syscall"

	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type Listener struct {
	Protocol string
	Address  string
	Port     uint32
	PID      int32
	Process  string
}

func Listeners(ctx context.Context) ([]Listener, error) {
	names := map[int32]string{}
	var rows []Listener
	sockets, err := gnet.ConnectionsWithContext(ctx, "inet4")
	if err != nil {
		return nil, err
	}
	for _, socket := range sockets {
		var protocol string
		switch {
		case socket.Type == syscall.SOCK_STREAM && socket.Status == "LISTEN":
			protocol = "tcp"
		case socket.Type == syscall.SOCK_DGRAM && socket.Raddr.Port == 0:
			protocol = "udp"
		default:
			continue
		}
		if socket.Laddr.IP != "127.0.0.1" && socket.Laddr.IP != "0.0.0.0" {
			continue
		}
		name, ok := names[socket.Pid]
		if !ok && socket.Pid > 0 {
			if proc, err := process.NewProcessWithContext(ctx, socket.Pid); err == nil {
				name, _ = proc.NameWithContext(ctx)
			}
			names[socket.Pid] = name
		}
		rows = append(rows, Listener{protocol, socket.Laddr.IP, socket.Laddr.Port, socket.Pid, name})
	}
	slices.SortFunc(rows, func(a, b Listener) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Address, b.Address), cmp.Compare(a.PID, b.PID))
	})
	return rows, nil
}
