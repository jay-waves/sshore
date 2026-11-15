package main

import (
	"errors"
	"github.com/jay-waves/sshore/internal/termui"
	"os"
)

type sessionSwitch struct{ from, to options }

func (s *sessionSwitch) Error() string { return "switch session" }
func runClient(o options) (int, error) {
	previous := ""
	var fallback *sessionSwitch
	fallbackPrevious := ""
	for {
		ready := false
		code, err := runClientOnce(o, previous, func() { ready = true })
		var next *sessionSwitch
		if errors.As(err, &next) {
			fallback = next
			fallbackPrevious = previous
			previous = next.from.session
			o = next.to
			continue
		}
		if !ready && fallback != nil {
			termui.Log(os.Stderr, termui.Warning, "[sshore] Target unavailable; restoring %s.\n", cleanText(fallback.from.session))
			o = fallback.from
			o.command = "attach"
			o.resumePID = 0
			o.approved = nil
			o.approvalID = ""
			o.stay = false
			previous = fallbackPrevious
			fallback = nil
			continue
		}
		return code, err
	}
}
