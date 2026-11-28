//go:build windows || (darwin && arm64)

package main

func handoffRemoteShell(o options) (bool, error) { return false, nil }
