package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
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
	loc, peer, err := parseBrowseLocations(a)
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
		index, err := console.PickPort(loc.host, labels, peer.host != "")
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
		port, err = portPrompt("Source port: ", "")
		if err != nil {
			return 1, err
		}
		if _, err = parsePort(port); err != nil {
			return 2, err
		}
	}
	dest, err := portPrompt("Listen on "+cleanText(peer.host)+" at port: ", port)
	if err != nil {
		return 1, err
	}
	if _, err = parsePort(dest); err != nil {
		return 2, err
	}
	args := []string{sshio.FormatEndpoint(loc.host, port), sshio.FormatEndpoint(peer.host, dest)}
	if udp {
		args = append(args, "--udp")
	}
	return browseCommand("forward", a.ssh, args...)
}

func portPrompt(prompt, initial string) (string, error) {
	var value string
	err := termui.RunLocalConsole(func(*termui.LocalConsole) error {
		input, err := newLocalInput()
		if err != nil {
			return err
		}
		value, err = input.ReadPrefilled(prompt, false, initial)
		return errors.Join(err, input.Close())
	})
	return value, err
}
