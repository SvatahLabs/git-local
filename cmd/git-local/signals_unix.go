//go:build !windows

package main

import (
	"os"
	"syscall"
)

// cancelSignals are the signals that cancel a command: Ctrl-C, a process
// supervisor's stop request, and a closed terminal.
var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
