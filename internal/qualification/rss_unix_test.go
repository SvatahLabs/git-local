//go:build darwin || linux

package qualification

import (
	"runtime"
	"syscall"
)

func peakRSSBytes() (int64, int64, error) {
	var self, children syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		return 0, 0, err
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children); err != nil {
		return 0, 0, err
	}
	multiplier := int64(1)
	if runtime.GOOS == "linux" {
		multiplier = 1024
	}
	return self.Maxrss * multiplier, children.Maxrss * multiplier, nil
}
