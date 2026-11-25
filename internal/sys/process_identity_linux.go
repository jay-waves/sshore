package sys

import "fmt"

func LiveIdentity(pid int) (string, error) {
	_, fields, err := readOwnedStat(processPath(pid), pid)
	if err != nil {
		return "", err
	}
	if len(fields) < 20 {
		return "", fmt.Errorf("short process stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", fmt.Errorf("parent shell exited")
	}
	return fields[19], nil
}
