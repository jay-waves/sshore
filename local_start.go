package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type localStartup struct {
	Record localShellRecord
	Error  string
}

func startLocalSession(name string, owners ...spawnOwner) (localShellRecord, error) {
	var empty localShellRecord
	reservation, err := reserveResourceName("shell", name)
	if err != nil {
		return empty, err
	}
	defer reservation.Close()
	dir, err := os.Getwd()
	if err != nil {
		return empty, err
	}
	reservation.owner.Close()
	reservation.owner = nil
	request := localShellRecord{Name: reservation.name, Directory: dir, Version: version, Token: resourceToken()}
	if len(owners) > 0 {
		request.Owner = owners[0]
	}
	c, err := openLocalDaemon(localDaemonRequest{Kind: "shell", Record: request, Env: os.Environ()})
	if err != nil {
		return empty, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var reply localStartup
	if err = json.NewDecoder(c).Decode(&reply); err != nil {
		return empty, err
	}
	if reply.Error != "" {
		return empty, fmt.Errorf("start local shell: %s", reply.Error)
	}
	if reply.Record.Name != request.Name || reply.Record.Token != request.Token || reply.Record.Version != version {
		return empty, fmt.Errorf("invalid local shell startup response")
	}
	record, err := readLocalSession(request.Name)
	if err != nil {
		return empty, err
	}
	if record != reply.Record {
		return empty, fmt.Errorf("local shell changed during startup")
	}
	if err = json.NewEncoder(c).Encode("ready"); err != nil {
		return empty, err
	}
	return record, nil
}
