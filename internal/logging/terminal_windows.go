//go:build windows

package logging

import (
	"os"

	"golang.org/x/sys/windows"
)

func enableVirtualTerminal(f *os.File) {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err == nil {
		mode |= windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
		_ = windows.SetConsoleMode(handle, mode)
	}
}
