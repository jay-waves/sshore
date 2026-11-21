package pty

import (
	"errors"
	"time"
)

func stopTerminal(exited <-chan struct{}, hangup, force func() error) error {
	if err := hangup(); err != nil {
		return err
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-exited:
		return nil
	case <-timer.C:
	}
	if err := force(); err != nil {
		select {
		case <-exited:
			return nil
		default:
			return err
		}
	}
	timer.Reset(2 * time.Second)
	select {
	case <-exited:
		return nil
	case <-timer.C:
		return errors.New("shell has not exited")
	}
}
