package termui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/jay-waves/sshore/internal/ports"
)

type PortRow struct {
	Label, Address, Port, Process string
	UDP                           bool
}

func portRows(listeners []ports.Listener) []PortRow {
	rows := make([]PortRow, 0, len(listeners))
	for _, listener := range listeners {
		owner := listener.Process
		if listener.PID > 0 {
			owner = fmt.Sprintf("%s (%d)", cmp.Or(owner, "PID"), listener.PID)
		}
		port := strconv.FormatUint(uint64(listener.Port), 10)
		rows = append(rows, PortRow{
			Label:   fmt.Sprintf("%-5s  %-6s  %-15s  %s", strings.ToUpper(listener.Protocol), port, listener.Address, owner),
			Address: net.JoinHostPort(listener.Address, port), Port: port, Process: listener.Process, UDP: listener.Protocol == "udp",
		})
	}
	return rows
}

func comparePortProcess(a, b PortRow) int {
	if (a.Process == "") != (b.Process == "") {
		if a.Process == "" {
			return 1
		}
		return -1
	}
	return cmp.Compare(strings.ToLower(a.Process), strings.ToLower(b.Process))
}

// PickPort returns the selected listener or a zero row for manual port entry.
func (c *LocalConsole) PickPort(host string, forward bool, load func(context.Context) (ports.ListenerSnapshot, error)) (selected PortRow, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listeners, err := load(ctx)
	if err != nil {
		return PortRow{}, err
	}
	terminal, err := openPickerTerminal(ctx)
	if err != nil {
		return PortRow{}, err
	}
	defer func() {
		if terminal.close != nil {
			err = errors.Join(err, terminal.close())
		}
	}()
	rows := portRows(listeners.Rows)
	var decoder keyDecoder
	var screen listScreen
	index := 0
	size, resized := watchScreenSize(ctx)
	var filtering, byProcess, udpOnly bool
	var query []rune
	visible := make([]PortRow, 0, len(rows))
	filter := func() {
		visible = visible[:0]
		needle := strings.ToLower(string(query))
		for _, row := range rows {
			if row.UDP == udpOnly && strings.Contains(strings.ToLower(row.Label), needle) {
				visible = append(visible, row)
			}
		}
		if byProcess {
			slices.SortStableFunc(visible, comparePortProcess)
		}
		index = 0
	}
	// Keep the same address and protocol when the rows are reordered or refreshed.
	selectedRow := func() PortRow {
		if len(visible) == 0 {
			return PortRow{}
		}
		return visible[index]
	}
	rebuild := func(previous PortRow) {
		filter()
		if i := slices.IndexFunc(visible, func(row PortRow) bool {
			return row.Address == previous.Address && row.UDP == previous.UDP
		}); i >= 0 {
			index = i
		}
	}
	filter()
	var status string
	color := ColorEnabled(os.Stdout)
	for {
		w, h := size.width, size.height
		sortLabel := "By port"
		if byProcess {
			sortLabel = "By process"
		}
		protocol := "TCP"
		if udpOnly {
			protocol = "UDP"
		}
		privilege := "Non-admin"
		if listeners.Admin {
			privilege = "Admin"
		}
		header := strings.Join([]string{
			Render(color, Heading, protocol+" ports"),
			Render(color, Accent, CleanText(host)),
			Render(color, Heading, privilege),
			Render(color, Muted, sortLabel),
		}, Render(color, Muted, "  ·  "))
		lines := []string{header, Render(color, Muted, " Proto  Port    Address          Process")}
		page := max(1, h-3)
		first := max(0, index-page+1)
		for i := first; i < min(len(visible), first+page); i++ {
			row := CleanText(visible[i].Label)
			if i == index {
				row = "\x1b[7m" + row + "\x1b[0m"
			}
			lines = append(lines, " "+row)
		}
		if len(visible) == 0 {
			message := "No listeners"
			if len(query) > 0 {
				message = "No matches"
			}
			lines = append(lines, Render(color, Muted, " "+message))
		}
		hint := "j/k Move"
		if forward {
			hint += "  Enter Forward"
		}
		hint += "  t TCP  u UDP  / Filter  r Refresh  s Sort  y Copy address"
		if forward {
			hint += "  : Enter port"
		}
		hint += "  q Quit"
		if filtering {
			hint = "/" + string(query)
		} else if status != "" {
			hint = Render(color, Info, status)
		} else {
			hint = shortcutLine(color, hint)
		}
		for len(lines) < h-1 {
			lines = append(lines, "")
		}
		lines = append(lines, hint)
		for i := range lines {
			lines[i] = ansi.Truncate(lines[i], w-1, "…")
		}
		if err := screen.paint(terminal.out, w, lines, 0); err != nil {
			return PortRow{}, err
		}
		select {
		case size = <-resized:
		case <-decoder.escape:
			decoder.finishEscape()
			filtering = false
		case input := <-terminal.input:
			if input.Err != nil {
				return PortRow{}, input.Err
			}
		keys:
			for event := range decoder.feed(input.Data) {
				r := event.value
				// Pasted text belongs only to the filter, never to command handling.
				if event.pasted && (!filtering || !unicode.IsPrint(r)) {
					continue
				}
				if filtering && r != keyUp && r != keyDown {
					switch r {
					case 0x1b:
						filtering = false
						continue
					case 3:
						filtering = false
						query = nil
					case '\n':
						if forward {
							if len(visible) > 0 {
								return visible[index], nil
							}
						} else {
							filtering = false
						}
						continue
					case '\b':
						if len(query) > 0 {
							query = query[:len(query)-1]
						}
					default:
						if unicode.IsPrint(r) && len(query) < 128 {
							query = append(query, r)
						}
					}
					filter()
					continue
				}
				status = ""
				switch r {
				case 'r':
					previous := selectedRow()
					closeErr := terminal.close()
					terminal = pickerTerminal{}
					if err := errors.Join(closeErr, c.close()); err != nil {
						return PortRow{}, err
					}
					next, loadErr := load(ctx)
					if err := c.open(); err != nil {
						return PortRow{}, errors.Join(loadErr, err)
					}
					var err error
					terminal, err = openPickerTerminal(ctx)
					if err != nil {
						return PortRow{}, errors.Join(loadErr, err)
					}
					screen, decoder = listScreen{}, keyDecoder{}
					if loadErr != nil {
						status = "Refresh failed. " + CleanText(loadErr.Error())
						break keys
					}
					listeners = next
					rows = portRows(next.Rows)
					rebuild(previous)
					status = "Ports refreshed"
					break keys
				case 't', 'u':
					udpOnly = r == 'u'
					rebuild(selectedRow())
				case 's':
					byProcess = !byProcess
					rebuild(selectedRow())
				case 'y':
					if len(visible) > 0 {
						address := visible[index].Address
						if err := copyToClipboard(terminal.out, address); err != nil {
							return PortRow{}, err
						}
						status = "Copied address " + address
					}
				case '/':
					filtering = true
				case 'q', 3:
					return PortRow{}, context.Canceled
				case ':':
					if forward {
						return PortRow{UDP: udpOnly}, nil
					}
				case 'j', keyDown:
					index = min(max(0, len(visible)-1), index+1)
				case 'k', keyUp:
					index = max(0, index-1)
				case '\n':
					if forward && len(visible) > 0 {
						return visible[index], nil
					}
				}
			}
		}
	}
}
