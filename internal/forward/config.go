package forward

import (
	"errors"
	"strconv"
)

var ErrPort = errors.New("port must be an integer from 1 to 65535")

type Config struct {
	Retries             int
	Session, ApprovalID string
	ResumePID           int
	Host, Name, Server  string
	Port, Target        int
	Reverse             bool
	SSH                 []string
}

func ParsePort(s string) (int, error) {
	if s == "" {
		return 0, ErrPort
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrPort
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, ErrPort
	}
	return n, nil
}
