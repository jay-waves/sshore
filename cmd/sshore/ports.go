package main

import (
	"bufio"
	"context"
	"errors"

	"github.com/jay-waves/sshore/internal/ports"
	"github.com/jay-waves/sshore/internal/termui"
)

func browseListeners(ctx context.Context, host string, ssh []string) (ports.ListenerSnapshot, error) {
	if host == "here" {
		return ports.ReadListenerSnapshot(ctx)
	}
	stream, err := startHostService(ctx, host, ssh, "listeners")
	if err != nil {
		return ports.ListenerSnapshot{}, err
	}
	defer stream.CloseAndWait()
	var rows ports.ListenerSnapshot
	if err := readControlJSON(bufio.NewReader(stream), &rows); err != nil {
		return ports.ListenerSnapshot{}, err
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
		row, err := console.PickPort(loc.host, peer.host != "", func(ctx context.Context) (ports.ListenerSnapshot, error) {
			return browseListeners(ctx, loc.host, a.ssh)
		})
		port, udp = row.Port, row.UDP
		return err
	})
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	dest, err := portPrompt("Listen on "+cleanText(peer.host)+" at port: ", port)
	if err != nil {
		return 1, err
	}
	if _, err = parsePort(dest); err != nil {
		return 2, err
	}
	args := []string{formatCLIEndpoint(loc.host, port), formatCLIEndpoint(peer.host, dest)}
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
