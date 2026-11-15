package tmux

func NewTerminalInput(t *Terminal) (*InputLease, error) { return NewInputLease(t), nil }
