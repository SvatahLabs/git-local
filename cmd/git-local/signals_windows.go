//go:build windows

package main

import (
	"os"
	"syscall"
)

// cancelSignals are the signals that cancel a command: Ctrl-C or Ctrl-Break,
// and a closed console window, logoff or shutdown, which Go reports as SIGTERM.
var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
