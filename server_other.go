//go:build !linux

package main

import "fmt"

func serverCommand(args []string) error {
	return fmt.Errorf("%s runs on the Linux server, inside the remote session", args[0])
}
