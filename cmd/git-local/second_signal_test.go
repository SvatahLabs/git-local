//go:build !windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func init() {
	testModes["hang"] = func() {
		ctx, _ := cancelContext()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("canceled")
		select {}
	}
}

// TestSecondSignalEndsHungCancellation checks that the first signal cancels
// and a second one ends a process that doesn't finish canceling.
func TestSecondSignalEndsHungCancellation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), "GITLOCAL_TEST_MODE=hang")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := bufio.NewScanner(stdout)
	expect := func(want string) {
		t.Helper()
		if !lines.Scan() || lines.Text() != want {
			t.Fatalf("got %q, want %q (%v)", lines.Text(), want, lines.Err())
		}
	}
	expect("ready")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	expect("canceled")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(10 * time.Second)
	for {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
				t.Fatalf("process ended with %v, want termination by SIGTERM", err)
			}
			return
		case <-deadline:
			t.Fatal("a second SIGTERM did not end the process")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
