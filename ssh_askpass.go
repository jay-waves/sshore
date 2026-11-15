package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"golang.org/x/term"
)

func startSpawnAuth() ([]string, func(), error) {
	token := resourceToken()
	var prompts sync.Mutex
	l, broker, err := resourceListen(token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/ask" {
			http.NotFound(w, r)
			return
		}
		prompts.Lock()
		defer prompts.Unlock()
		serveSSHAskpass(w, r)
	})
	if err != nil {
		return nil, nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		broker.Close()
		return nil, nil, err
	}
	return sshAskpassEnvironment(exe, l.Addr().String(), token), func() { broker.Close() }, nil
}

func serveSSHAskpass(w http.ResponseWriter, r *http.Request) {
	prompt, err := io.ReadAll(io.LimitReader(r.Body, 8193))
	if err != nil || len(prompt) > 8192 {
		http.Error(w, "invalid prompt", 400)
		return
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		http.Error(w, "SSH authentication requires a terminal", 409)
		return
	}
	fmt.Fprint(os.Stderr, cleanText(string(prompt))+" ")
	answer, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		http.Error(w, "terminal input failed", 409)
		return
	}
	_, _ = w.Write(answer)
}

func sshAskpassEnvironment(exe, address, token string) []string {
	return []string{
		"SSH_ASKPASS=" + exe,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=sshore",
		"SSHORE_ASKPASS=1",
		"SSHORE_ASKPASS_ADDR=" + address,
		"SSHORE_ASKPASS_TOKEN=" + token,
	}
}
