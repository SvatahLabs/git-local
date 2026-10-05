package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// HostileCommand returns a Git-config-compatible command that invokes a test
// in the current native executable. It works without a separate POSIX helper.
func HostileCommand(t testing.TB, testName, marker string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		executable = filepath.ToSlash(executable)
		marker = filepath.ToSlash(marker)
	}
	quote := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	return quote(executable) + " -test.run=^" + testName + "$ -- gitlocal-hostile-helper " + quote(marker)
}

// RunHostileProgram is called by the test selected in HostileCommand. A normal
// package test invocation returns; a Git subprocess writes its marker and exits
// without producing test-runner output that could affect Git protocols.
func RunHostileProgram() {
	for i, arg := range os.Args {
		if arg != "gitlocal-hostile-helper" || i+1 >= len(os.Args) {
			continue
		}
		if err := os.WriteFile(os.Args[i+1], []byte("ran\n"), 0600); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}
