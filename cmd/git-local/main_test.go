//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// testModes lets the test binary stand in for git-local in a subprocess.
var testModes = map[string]func(){"main": main}

func TestMain(m *testing.M) {
	if run := testModes[os.Getenv("GITLOCAL_TEST_MODE")]; run != nil {
		run()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// gitLocal returns a command that runs this test binary as git-local in the
// repository, with an isolated HOME and its own temporary directory.
func gitLocal(t *testing.T, repo *testutil.Repo, tmp string, args ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, args...)
	cmd.Dir = repo.Root
	cmd.Env = append(repo.RunnerEnv(), "GITLOCAL_TEST_MODE=main", "TMPDIR="+tmp)
	return cmd
}

func runnerDirs(t *testing.T, tmp string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(tmp, "gitlocal-runner-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestStopSignalsEndAutoWatchWithSummary(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			repo := testutil.New(t)
			repo.Commit()
			tmp := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo.Root, "notes.local"), []byte("timer snapshot\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init"}, {"track", "notes.local"}} {
				if out, err := gitLocal(t, repo, tmp, args...).CombinedOutput(); err != nil {
					t.Fatalf("%v: %v %s", args, err, out)
				}
			}
			watch := gitLocal(t, repo, tmp, "auto", "watch", "--interval", "1s", "--json")
			var out, diagnostic bytes.Buffer
			watch.Stdout, watch.Stderr = &out, &diagnostic
			if err := watch.Start(); err != nil {
				t.Fatal(err)
			}
			// Wait for the initial snapshot, so the summary has one to report.
			deadline := time.Now().Add(30 * time.Second)
			for {
				log, err := gitLocal(t, repo, tmp, "log", "-n", "1").Output()
				if err == nil && bytes.Contains(log, []byte("Automatic local snapshot")) {
					break
				}
				if time.Now().After(deadline) {
					watch.Process.Kill()
					t.Fatal("watch did not publish its initial snapshot")
				}
				time.Sleep(50 * time.Millisecond)
			}
			if err := watch.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- watch.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("watch ended with %v: %s %s", err, out.String(), diagnostic.String())
				}
			case <-time.After(20 * time.Second):
				watch.Process.Kill()
				t.Fatal("watch ignored the signal")
			}
			var envelope struct {
				OK   bool `json:"ok"`
				Data struct {
					Snapshots      int    `json:"snapshots"`
					LatestSnapshot string `json:"latestSnapshot"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Data.Snapshots != 1 || envelope.Data.LatestSnapshot == "" {
				t.Fatalf("summary %q (%v), diagnostics %q", out.String(), err, diagnostic.String())
			}
			if dirs := runnerDirs(t, tmp); len(dirs) != 0 {
				t.Fatalf("watch left its temporary directories: %v", dirs)
			}
		})
	}
}
