package mux

import "golang.org/x/sys/windows"

// A temporary size change asks ConPTY to repaint without injecting input.
func RequestRedraw(t *Terminal, _ int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.size.Cols < 2 {
		return
	}
	changed := windows.Coord{X: int16(t.size.Cols - 1), Y: int16(t.size.Rows)}
	if windows.ResizePseudoConsole(t.console, changed) == nil {
		_ = windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(t.size.Cols), Y: int16(t.size.Rows)})
	}
}
