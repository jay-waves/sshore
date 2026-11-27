package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type listener struct {
	Address string
	Port    uint32
	PID     int32
	Process string
}

func listeners(ctx context.Context) ([]listener, error) {
	sockets, err := gnet.ConnectionsWithContext(ctx, "tcp4")
	if err != nil {
		return nil, err
	}
	names := map[int32]string{}
	var rows []listener
	for _, socket := range sockets {
		if socket.Status != "LISTEN" || (socket.Laddr.IP != "127.0.0.1" && socket.Laddr.IP != "0.0.0.0") {
			continue
		}
		name, ok := names[socket.Pid]
		if !ok && socket.Pid > 0 {
			if proc, err := process.NewProcessWithContext(ctx, socket.Pid); err == nil {
				name, _ = proc.NameWithContext(ctx)
			}
			names[socket.Pid] = name
		}
		rows = append(rows, listener{socket.Laddr.IP, socket.Laddr.Port, socket.Pid, name})
	}
	slices.SortFunc(rows, func(a, b listener) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Address, b.Address), cmp.Compare(a.PID, b.PID))
	})
	return rows, nil
}

func browseListeners(ctx context.Context, host string, ssh []string) ([]listener, error) {
	if host == "here" {
		return listeners(ctx)
	}
	stream, err := browseStream(ctx, host, ssh, "listeners")
	if err != nil {
		return nil, err
	}
	defer stream.CloseAndWait()
	var rows []listener
	if err := readControlJSON(bufio.NewReader(stream), &rows); err != nil {
		return nil, err
	}
	return rows, stream.Wait()
}

func runPortBrowser(a cliArguments) (int, error) {
	loc, err := parseBrowseLocation(a)
	if err != nil {
		return 2, err
	}
	var port string
	err = termui.RunLocalConsole(func(console *termui.LocalConsole) error {
		rows, err := browseListeners(context.Background(), loc.host, a.ssh)
		if err != nil {
			return err
		}
		labels := make([]string, len(rows))
		for i, row := range rows {
			owner := row.Process
			if row.PID > 0 {
				owner = fmt.Sprintf("%s (%d)", cmp.Or(owner, "PID"), row.PID)
			}
			labels[i] = fmt.Sprintf("%-6d  %-15s  %s", row.Port, row.Address, owner)
		}
		index, err := console.PickPort(loc.host, labels)
		if err == nil && index >= 0 {
			port = strconv.FormatUint(uint64(rows[index].Port), 10)
		}
		return err
	})
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	if port == "" {
		port, err = browsePrompt("Source port: ", "")
		if err != nil {
			return 1, err
		}
		if _, err = parsePort(port); err != nil {
			return 2, err
		}
	}
	peer := "here"
	if loc.host == "here" {
		if os.Getenv("SSHORE_SOCKET") != "" {
			peer = "origin"
		} else {
			peer, err = browsePrompt("Forward to SSH host: ", "")
			if err != nil {
				return 1, err
			}
			if err = validateHostOperand("forward", peer); err != nil {
				return 2, err
			}
		}
	}
	dest, err := browsePrompt("Listen on "+cleanText(peer)+" at port: ", port)
	if err != nil {
		return 1, err
	}
	if _, err = parsePort(dest); err != nil {
		return 2, err
	}
	return browseCommand("forward", a.ssh, sshio.FormatEndpoint(loc.host, port), sshio.FormatEndpoint(peer, dest))
}
