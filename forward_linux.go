package main

import "fmt"

func forwardHelper(args []string) error {
	m, err := parseForward(args)
	if err != nil {
		return err
	}
	if m.Action == "add" {
		return requestOperations([]operation{{Kind: "forward", Reverse: m.Reverse, Port: m.Port, RemoteHost: m.RemoteHost, RemotePort: m.RemotePort}})
	}
	return fmt.Errorf("manage independent forwards on your computer: sshore list / sshore stop forward/NAME")
}
