package term

import (
	"os"

	"golang.org/x/sys/unix"
)

func FlushInput() error {
	// TIOCFLUSH takes a pointer to the XNU FREAD bit.
	const fRead = 1
	return unix.IoctlSetPointerInt(int(os.Stdin.Fd()), unix.TIOCFLUSH, fRead)
}
