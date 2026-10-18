package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// Each command a test runs through callCLI runs in a child process of the
// test binary, in its own working directory and with the test's environment
// (setenv), so tests run in parallel without changing the working directory
// or environment of this process (GL-123).

const childArgs = "GITLOCAL_TEST_CLI_ARGS"

// The race detector sets a failing exit code only when the program would
// exit 0, so a race in a command that fails would go unreported. The child
// halts at its first race with raceExit instead, and callCLI fails the test
// on that code or on a race report.
const raceExit = 66

// testBinary is this test binary, by absolute path: cmd.Dir would resolve a
// relative os.Args[0] from the child's working directory.
var testBinary = func() string {
	path, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return path
}()

// TestCLIChild runs one command for callCLI and exits with its code.
func TestCLIChild(t *testing.T) {
	encoded, ok := os.LookupEnv(childArgs)
	if !ok {
		t.Skip("child process for callCLI")
	}
	os.Unsetenv(childArgs)
	var args []string
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		t.Fatal(err)
	}
	os.Exit(Execute(context.Background(), args, os.Stdout, os.Stderr))
}

var (
	envMu sync.Mutex
	// envs holds, by top-level test, the variables setenv set.
	envs = map[string]map[string]string{}
)

func topLevel(t testing.TB) string { name, _, _ := strings.Cut(t.Name(), "/"); return name }

// setenv is t.Setenv for the commands callCLI runs, without changing this
// process's environment, so the test can run in parallel.
func setenv(t testing.TB, key, value string) {
	t.Helper()
	envMu.Lock()
	defer envMu.Unlock()
	name := topLevel(t)
	if envs[name] == nil {
		envs[name] = map[string]string{}
		t.Cleanup(func() {
			envMu.Lock()
			defer envMu.Unlock()
			delete(envs, name)
		})
	}
	envs[name][key] = value
}

// isolateHome gives the commands a test runs home as HOME, USERPROFILE and
// XDG_CONFIG_HOME.
func isolateHome(t testing.TB, home string) {
	t.Helper()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		setenv(t, key, home)
	}
}

// childEnv is this process's environment with the test's variables, without
// PWD (the child's working directory is cwd), and with GORACE halting at the
// first race.
func childEnv(t testing.TB, args []string) []string {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	envMu.Lock()
	set := envs[topLevel(t)]
	envMu.Unlock()
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := set[key]; !ok && key != "PWD" && key != childArgs && key != "GORACE" {
			env = append(env, entry)
		}
	}
	for key, value := range set {
		env = append(env, key+"="+value)
	}
	race := strings.TrimSpace(os.Getenv("GORACE") + fmt.Sprintf(" halt_on_error=1 exitcode=%d", raceExit))
	return append(env, "GORACE="+race, childArgs+"="+string(encoded))
}

func callCLI(t *testing.T, cwd string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(testBinary, "-test.run=^TestCLIChild$", "-test.timeout=10m")
	cmd.Dir = cwd
	cmd.Env = childEnv(t, args)
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("git local %v: %v", args, err)
	}
	code := cmd.ProcessState.ExitCode()
	if code == raceExit || strings.Contains(diagnostic.String(), "WARNING: DATA RACE") {
		t.Fatalf("git local %v: data race (exit %d)\n%s", args, code, diagnostic.String())
	}
	return code, out.String(), diagnostic.String()
}
