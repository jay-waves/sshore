package termui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

const progressInterval = 200 * time.Millisecond

// Progress displays one transfer. Call Update and Close from the same goroutine.
type Progress struct {
	out      io.Writer
	terminal *os.File
	restore  func() error
	color    bool
	label    string
	total    int64
	start    time.Time
	at       time.Time
	bytes    int64
	rate     float64
}

// NewProgress starts a progress bar, or periodic status lines without a terminal.
// A negative total is indeterminate.
func NewProgress(out io.Writer, label string, total int64) *Progress {
	now := time.Now()
	p := &Progress{out: out, label: CleanText(label), total: total, start: now, at: now}
	if f, ok := out.(*os.File); ok && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd())) {
		if restore, err := enableConsoleOutput(f); err == nil {
			p.terminal, p.restore, p.color = f, restore, ColorEnabled(f)
			p.draw(0)
		}
	}
	return p
}

// RunProgress displays progress while run updates the byte counter.
func RunProgress(out io.Writer, label string, total int64, run func(*atomic.Int64) error) error {
	if out == nil || out == io.Discard {
		return run(nil)
	}
	p := NewProgress(out, label, total)
	defer p.Close()
	return MonitorProgress(p.Update, run)
}

// MonitorProgress samples the counter until the transfer returns. Use it to
// forward progress to a terminal owned by another process.
func MonitorProgress(report func(int64), run func(*atomic.Int64) error) error {
	var transferred atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				report(transferred.Load())
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	return run(&transferred)
}

// Close clears the progress bar before any subsequent terminal output.
func (p *Progress) Close() {
	if p.terminal != nil {
		fmt.Fprint(p.terminal, "\r\x1b[2K")
		_ = p.restore()
	}
}

// Update displays the latest total number of bytes transferred.
func (p *Progress) Update(done int64) {
	now := time.Now()
	elapsed := now.Sub(p.at)
	if elapsed <= 0 || p.terminal == nil && elapsed < time.Second {
		return
	}
	done = max(p.bytes, done)
	rate := float64(done-p.bytes) / elapsed.Seconds()
	if p.rate == 0 {
		p.rate = rate
	} else {
		p.rate = .3*rate + .7*p.rate
	}
	p.at, p.bytes = now, done
	if p.terminal != nil {
		p.draw(now.Sub(p.start))
	} else {
		Log(p.out, "", Info, "%s · %s", p.label, p.detail())
	}
}

func (p *Progress) draw(elapsed time.Duration) {
	width, _, err := term.GetSize(int(p.terminal.Fd()))
	if err != nil || width <= 0 {
		width = 80
	}
	width-- // Leave the last column free to avoid a wrapped progress line.
	const cells = 12
	bar, percent := "", ""
	if p.total >= 0 {
		fraction := 1.0
		if p.total > 0 {
			fraction = min(1, float64(p.bytes)/float64(p.total))
		}
		filled := int(fraction * cells)
		bar = strings.Repeat("=", filled) + strings.Repeat("-", cells-filled)
		percent = fmt.Sprintf(" %3d%%", int(fraction*100))
	} else {
		position := int(elapsed/progressInterval) % (2*cells - 2)
		position = min(position, 2*cells-2-position)
		bar = strings.Repeat("-", position) + "=" + strings.Repeat("-", cells-position-1)
	}
	meter := Render(p.color, Info, "["+bar+"]"+percent) + " " + p.detail()
	labelWidth := max(12, width-ansi.StringWidth(meter)-1)
	line := ansi.Truncate(ansi.Truncate(p.label, labelWidth, "…")+" "+meter, width, "…")
	fmt.Fprintf(p.terminal, "\r\x1b[2K%s", line)
}

func (p *Progress) detail() string {
	detail := FormatBytes(p.bytes)
	if p.total >= 0 {
		detail += "/" + FormatBytes(p.total)
		if p.bytes >= p.total {
			return detail + " · finishing"
		}
	}
	if p.rate >= 1 {
		detail += " · " + FormatBytes(int64(p.rate)) + "/s"
	}
	return detail
}

// FormatBytes formats a byte count in binary units.
func FormatBytes(size int64) string {
	for _, unit := range []struct {
		size  int64
		label string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if size >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(size)/float64(unit.size), unit.label)
		}
	}
	return fmt.Sprintf("%d B", size)
}
