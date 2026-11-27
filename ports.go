package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jay-waves/sshore/internal/ports"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/termui"
)

func browseListeners(ctx context.Context, host string, ssh []string) ([]ports.Listener, error) {
	if host == "here" {
		return ports.Listeners(ctx)
	}
	stream, err := startHostService(ctx, host, ssh, "listeners")
	if err != nil {
		return nil, err
	}
	defer stream.CloseAndWait()
	var rows []ports.Listener
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
	var udp bool
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
			labels[i] = fmt.Sprintf("%-5s  %-6d  %-15s  %s", strings.ToUpper(row.Protocol), row.Port, row.Address, owner)
		}
		index, err := console.PickPort(loc.host, labels)
		if err == nil && index >= 0 {
			udp = rows[index].Protocol == "udp"
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
	args := []string{sshio.FormatEndpoint(loc.host, port), sshio.FormatEndpoint(peer, dest)}
	if udp {
		args = append(args, "--udp")
	}
	return browseCommand("forward", a.ssh, args...)
}
