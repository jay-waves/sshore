//go:build linux && !sshoretest

package main

// Production setup inspects visible session daemons belonging to this account.
func includeSetupProcess(p daemonProcess) bool { return true }
