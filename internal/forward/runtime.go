package forward

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jay-waves/sshore/internal/termui"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const ReadyMarker = "sshore-forward-ready"

// Runtime receives process construction and resource publication from the program.
// Command returns a fresh, context-bound command; all hooks and Log are required.
// Heartbeat metadata is encoded once per attempt and contains only public fields.
type Runtime struct {
	Retries           int
	Description       string
	Log               io.Writer
	HeartbeatInterval time.Duration
	Command           func(context.Context, bool) *exec.Cmd
	Ready             func() error
	Status            func(string, error)
	Heartbeat         func() any
}

// Run retries established forwards only. Cancellation closes this transport,
// while existing application TCP connections are never replayed.
func Run(ctx context.Context, runtime Runtime) (int, error) {
	started := false
	for attempt := 0; ; attempt++ {
		code, err := runAttempt(ctx, runtime, attempt == 0, &started)
		if ctx.Err() != nil {
			return 0, nil
		}
		if err == nil || attempt >= runtime.Retries || !started {
			return code, err
		}
		runtime.Status("reconnecting", err)
		delay := retryDelay(attempt)
		fmt.Fprintf(runtime.Log, "[sshore] Forward disconnected; TCP connections lost. Retry %d/%d in %s.\n", attempt+1, runtime.Retries, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil
		case <-timer.C:
		}
	}
}
func retryDelay(attempt int) time.Duration {
	return time.Second * time.Duration(1<<min(attempt, 5))
}
func runAttempt(parent context.Context, runtime Runtime, initial bool, started *bool) (int, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := runtime.Command(ctx, initial)
	var diagnostics diagnosticsTail
	cmd.Stderr = io.MultiWriter(runtime.Log, &diagnostics)
	input, e := cmd.StdinPipe()
	if e != nil {
		return 1, e
	}
	defer input.Close()
	// Let the remote watcher receive EOF and remove its lease before forcing SSH
	// down. Authentication or active forwarded sockets still have a bounded exit.
	cmd.Cancel = func() error { return input.Close() }
	cmd.WaitDelay = 2 * time.Second
	output, e := cmd.StdoutPipe()
	if e != nil {
		return 1, e
	}
	defer output.Close()
	if e = cmd.Start(); e != nil {
		return 1, e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		cancel()
		input.Close()
		output.Close()
		if !exited {
			<-done
		}
	}()
	// Read only the bounded protocol marker. Unexpected login output is an error.
	marker, e := bufio.NewReader(io.LimitReader(output, int64(len(ReadyMarker)+2))).ReadString('\n')
	if e != nil {
		cancel()
		exitErr := <-done
		exited = true
		detail := diagnostics.String()
		if detail == "" {
			detail = fmt.Sprint(exitErr)
		}
		if strings.Contains(detail, "remote port forwarding failed") {
			detail += "; remote listening port is occupied or denied by SSH policy; choose another remote port or inspect existing forwards"
		}
		return 1, fmt.Errorf("forward startup failed (%s): %s", runtime.Description, detail)
	}
	if marker != ReadyMarker+"\n" {
		return 1, fmt.Errorf("unexpected forward startup response: %q", termui.CleanText(marker))
	}
	*started = true
	if initial {
		if e = runtime.Ready(); e != nil {
			return 1, e
		}
	} else {
		runtime.Status("connected", nil)
	}
	task := runtime.Heartbeat()
	go func() {
		ticker := time.NewTicker(runtime.HeartbeatInterval)
		defer ticker.Stop()
		enc := json.NewEncoder(input)
		for {
			if e := enc.Encode(task); e != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	select {
	case e = <-done:
		exited = true
		if e != nil {
			return 1, e
		}
		return 1, fmt.Errorf("forward transport closed")
	case <-ctx.Done():
		return 0, nil
	}
}

// Keep only the current attempt's diagnostic tail, never earlier log entries.
type diagnosticsTail struct {
	mu   sync.Mutex
	data []byte
}

func (d *diagnosticsTail) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	if len(p) >= 4096 {
		d.data = append(d.data[:0], p[len(p)-4096:]...)
	} else {
		if len(d.data)+len(p) > 4096 {
			d.data = d.data[len(d.data)+len(p)-4096:]
		}
		d.data = append(d.data, p...)
	}
	return n, nil
}
func (d *diagnosticsTail) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return termui.CleanText(strings.TrimSpace(string(d.data)))
}
