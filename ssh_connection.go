package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/term"
	"github.com/jay-waves/sshore/internal/termui"
	cryptossh "golang.org/x/crypto/ssh"
	xterm "golang.org/x/term"
)

func sshConnectionOptions(o options) sshio.Options {
	return sshio.Options{Host: o.host, Command: remoteCommand(o), Args: o.ssh, Environment: o.environment, Directory: o.sshDirectory, Confirm: confirmSSHHost, Secret: promptSSHSecret}
}

func withSSHDiagnostics(o sshio.Options, out io.Writer, auth *sshAuthEndpoint) sshio.Options {
	o.IgnoredForwarding = func(ctx context.Context, host string) {
		warning := ignoredSSHForwarding(host)
		if !auth.warn(ctx, warning) {
			logSSHWarning(out, warning)
		}
	}
	o.Negotiated = func(host string, algorithms cryptossh.NegotiatedAlgorithms) {
		direction := func(a cryptossh.DirectionAlgorithms) string {
			if a.MAC == "" {
				return a.Cipher + " (AEAD)"
			}
			return a.Cipher + " / " + a.MAC
		}
		fmt.Fprintf(out, "[sshore] SSH %q: kex=%s hostkey=%s send=%s receive=%s\n", host, algorithms.KeyExchange, algorithms.HostKey, direction(algorithms.Write), direction(algorithms.Read))
	}
	return o
}

func ignoredSSHForwarding(host string) string {
	return fmt.Sprintf("Ignoring SSH forwarding rules for %q. Use sshore forward for TCP forwarding.", host)
}

func logSSHWarning(out io.Writer, warning string) {
	warning = cleanText(warning)
	if file, ok := out.(*os.File); ok {
		termui.Log(file, "", termui.Warning, "WARNING: %s", warning)
		return
	}
	fmt.Fprintf(out, "[sshore] WARNING: %s\n", warning)
}

func confirmSSHHost(ctx context.Context, prompt string) (bool, error) {
	if !xterm.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("SSH host trust requires an interactive terminal")
	}
	answer, err := readSSHLine(ctx, cleanText(prompt)+" Type yes: ")
	return strings.TrimSpace(answer) == "yes", err
}

func readSSHLine(ctx context.Context, prompt string) (string, error) {
	return readSSHInput(ctx, func(input *termui.Input) (string, error) { return input.Read(prompt, false) })
}

func readSSHInput(ctx context.Context, read func(*termui.Input) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return "", usage("local selection requires an interactive terminal; specify the local endpoint")
	}
	term.ResetInputModes()
	if err := term.FlushInput(); err != nil {
		return "", err
	}
	restore, err := term.RawConsole()
	if err != nil {
		return "", err
	}
	defer restore()

	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	input, err := termui.NewInputFrom(reader)
	if err != nil {
		return "", err
	}
	defer input.Close()
	// Interrupt the editor via its input pipe. Only the owning goroutine
	// closes the editor and restores terminal modes.
	keyboardCtx, cancelKeyboard := context.WithCancel(ctx)
	defer cancelKeyboard()
	stopCancel := context.AfterFunc(keyboardCtx, func() { _ = writer.CloseWithError(keyboardCtx.Err()) })
	defer stopCancel()
	stopKeyboard, err := term.StartKeyboard(keyboardCtx, func(data []byte, err error) {
		defer clear(data)
		if len(data) != 0 {
			_, _ = writer.Write(data)
		}
		if err != nil {
			_ = writer.CloseWithError(err)
		}
	})
	if err != nil {
		return "", err
	}
	defer func() {
		cancelKeyboard()
		_ = writer.Close()
		stopKeyboard()
	}()
	answer, err := read(input)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return answer, err
}

const maxSSHPrompt = 8192
const maxSSHResponse = 4096

func promptSSHSecret(ctx context.Context, identity string) ([]byte, error) {
	if !xterm.IsTerminal(int(os.Stdin.Fd())) || !xterm.IsTerminal(int(os.Stdout.Fd())) {
		return nil, fmt.Errorf("SSH authentication requires an interactive terminal")
	}
	prompt := cleanText(identity) + ": "
	if strings.HasPrefix(identity, "\x00password:") {
		prompt = cleanText(strings.TrimPrefix(identity, "\x00password:")) + "'s password: "
	}
	if operation, ok := strings.CutPrefix(identity, "\x00approve-forward:"); ok {
		answer, err := readSSHLine(ctx, "Approve "+cleanText(operation)+"? [y/N] ")
		if termui.ConfirmYes(answer) {
			return []byte("yes"), err
		}
		return nil, err
	}
	if strings.HasPrefix(identity, "\x00challenge:") {
		var challenge sshio.Challenge
		if err := json.Unmarshal([]byte(strings.TrimPrefix(identity, "\x00challenge:")), &challenge); err != nil {
			return nil, err
		}
		for _, line := range []string{challenge.Title, challenge.Instruction} {
			if line != "" {
				fmt.Fprintln(os.Stderr, cleanText(line))
			}
		}
		prompt = cleanText(challenge.Question)
		if challenge.Echo {
			answer, err := readSSHLine(ctx, prompt)
			if err != nil {
				return nil, err
			}
			if len(answer) > maxSSHResponse {
				return nil, fmt.Errorf("SSH response is too long")
			}
			return []byte(answer), nil
		}
	}
	return readSSHSecret(ctx, prompt)
}

// Reuse the joined console reader so cancellation cannot leave stdin owned by
// an abandoned password prompt. Secret bytes never enter readline or history.
func readSSHSecret(parent context.Context, prompt string) ([]byte, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	restore, err := term.RawConsole()
	if err != nil {
		return nil, err
	}
	defer restore()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type input struct {
		data []byte
		err  error
	}
	incoming := make(chan input)
	stop, err := term.StartKeyboard(ctx, func(data []byte, err error) {
		select {
		case incoming <- input{data, err}:
		case <-ctx.Done():
			clear(data)
		}
	})
	if err != nil {
		return nil, err
	}
	defer func() { cancel(); stop() }()
	if _, err := fmt.Fprint(os.Stderr, cleanText(prompt)); err != nil {
		return nil, err
	}
	defer fmt.Fprint(os.Stderr, "\r\n")
	secret := make([]byte, 0, maxSSHResponse)
	accepted := false
	defer func() {
		if !accepted {
			clear(secret)
		}
	}()
	escape := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event := <-incoming:
			for _, key := range event.data {
				if key == 3 {
					clear(event.data)
					return nil, context.Canceled
				}
				if escape > 0 {
					if escape == 1 && (key == '[' || key == 'O') {
						escape = 2
					} else if escape == 1 || key >= '@' && key <= '~' {
						escape = 0
					}
					continue
				}
				switch key {
				case '\r', '\n':
					clear(event.data)
					accepted = true
					return secret, nil
				case 4:
					clear(event.data)
					return nil, io.EOF
				case 8, 127:
					_, size := utf8.DecodeLastRune(secret)
					clear(secret[len(secret)-size:])
					secret = secret[:len(secret)-size]
				case 21:
					clear(secret)
					secret = secret[:0]
				case 27:
					escape = 1
				default:
					if len(secret) >= maxSSHResponse {
						clear(event.data)
						return nil, fmt.Errorf("SSH response is too long")
					}
					secret = append(secret, key)
				}
			}
			clear(event.data)
			if event.err != nil {
				return nil, event.err
			}
		}
	}
}

// Bound loopback connection setup and idle retention. Prompt input stays
// cancellable without imposing a fixed deadline on the user's decision.
var sshAuthHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:  (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns: 16, MaxIdleConnsPerHost: 2, IdleConnTimeout: 5 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func postSSHAuth(ctx context.Context, address, token, route string, body io.Reader) (*http.Response, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return nil, fmt.Errorf("invalid SSH authentication broker address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+route, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return sshAuthHTTPClient.Do(req)
}

func listenSSHAuth(ctx context.Context, token string, handler http.HandlerFunc) (net.Listener, *http.Server, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	expected := []byte("Bearer " + token)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    8192,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			handler(w, r)
		}),
	}
	go server.Serve(listener)
	return listener, server, nil
}

type sshAuthEndpoint struct {
	Address string
	Token   string
}

func (auth *sshAuthEndpoint) warn(ctx context.Context, warning string) bool {
	if auth == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	response, err := postSSHAuth(ctx, auth.Address, auth.Token, "/warn", strings.NewReader(warning))
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusNoContent
}

func (auth *sshAuthEndpoint) callbacks() (confirm func(context.Context, string) (bool, error), secret func(context.Context, string) ([]byte, error)) {
	if auth == nil {
		return nil, nil
	}
	request := func(ctx context.Context, route, prompt, description string, limit int64) ([]byte, error) {
		response, err := postSSHAuth(ctx, auth.Address, auth.Token, route, strings.NewReader(prompt))
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s unavailable", description)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(data)) > limit {
			clear(data)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("invalid %s response", description)
		}
		return data, nil
	}
	confirm = func(ctx context.Context, prompt string) (bool, error) {
		data, err := request(ctx, "/ask", prompt, "SSH host confirmation", 3)
		if err != nil {
			return false, err
		}
		switch string(data) {
		case "yes":
			return true, nil
		case "no":
			return false, nil
		default:
			return false, fmt.Errorf("invalid SSH host confirmation response")
		}
	}
	secret = func(ctx context.Context, prompt string) ([]byte, error) {
		return request(ctx, "/unlock", prompt, "SSH authentication", maxSSHResponse)
	}
	return confirm, secret
}

func startSSHAuthBroker() (*sshAuthEndpoint, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	token := resourceToken()
	prompts := make(chan struct{}, 1)
	listener, broker, err := listenSSHAuth(ctx, token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/warn":
			warning, err := io.ReadAll(io.LimitReader(r.Body, maxSSHPrompt+1))
			if err != nil || len(warning) > maxSSHPrompt {
				http.Error(w, "invalid warning", http.StatusBadRequest)
				return
			}
			logSSHWarning(os.Stderr, string(warning))
			w.WriteHeader(http.StatusNoContent)
			return
		case "/ask", "/unlock":
		default:
			http.NotFound(w, r)
			return
		}
		select {
		case prompts <- struct{}{}:
			defer func() { <-prompts }()
		case <-r.Context().Done():
			return
		}
		if r.Context().Err() == nil {
			serveSSHConfirmation(w, r)
		}
	})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return &sshAuthEndpoint{Address: listener.Addr().String(), Token: token}, sync.OnceFunc(func() {
		cancel()
		_ = broker.Close()
		// Join the active prompt so shutdown includes terminal restoration.
		// Retain the slot after cancellation so no later handler can read input.
		prompts <- struct{}{}
	}), nil
}

func serveSSHConfirmation(w http.ResponseWriter, r *http.Request) {
	prompt, err := io.ReadAll(io.LimitReader(r.Body, maxSSHPrompt+1))
	if err != nil || len(prompt) > maxSSHPrompt {
		http.Error(w, "invalid prompt", 400)
		return
	}
	var answer []byte
	if r.URL.Path == "/unlock" {
		answer, err = promptSSHSecret(r.Context(), string(prompt))
	} else {
		var ok bool
		ok, err = confirmSSHHost(r.Context(), string(prompt))
		if ok {
			answer = []byte("yes")
		} else {
			answer = []byte("no")
		}
	}
	defer clear(answer)
	if err != nil {
		http.Error(w, "terminal input failed", 409)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(answer)
}
