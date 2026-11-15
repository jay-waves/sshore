package main

import (
	"errors"
	"os"
)

func runShellCommand(args []string) (int, error) {
	if code, handled, err := runTaskID(args); handled {
		return code, err
	}
	o, err := parseOptions(args)
	if errors.Is(err, errHelp) {
		return 0, showHelp(args[:1], os.Stdout)
	}
	if err != nil {
		return 2, err
	}
	if o.local {
		return runLocalCommand(o)
	}
	if err = resolveRecentAttach(&o); err != nil {
		return 1, err
	}
	o.downloadDir, err = prepareDownloadDir(o.downloadDir)
	if err != nil {
		return 1, err
	}
	if o.list {
		return runInventory(o)
	}
	return runClient(o)
}
