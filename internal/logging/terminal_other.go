//go:build !windows

package logging

import "os"

func enableVirtualTerminal(f *os.File) {}
