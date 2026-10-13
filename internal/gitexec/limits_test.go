package gitexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gitlocal/internal/testutil"
)

// repeating is an endless reader of one line.
type repeating string

func (r repeating) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		n += copy(b[n:], r)
	}
	return n, nil
}

// GL-115: a capped command stops once its output passes the cap, instead of
// running to the end; here Git would print the blob forever.
func TestCappedOutputStopsGit(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	r := newRunner(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := filepath.Join(t.TempDir(), "store.git")
	if err := r.InitBare(ctx, store); err != nil {
		t.Fatal(err)
	}
	blob, err := r.History(ctx, store, strings.NewReader(strings.Repeat("x", 1<<20)), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.History(ctx, store, repeating(strings.TrimSpace(string(blob))+"\n"), "cat-file", "--batch")
	if err == nil || !strings.Contains(err.Error(), "exceeded safety limit") {
		t.Fatalf("endless capped output: %v", err)
	}
}

// GL-115: check-ignore records repeat the deciding rule, which the repository
// controls; a long rule matched by many paths is kept once, not once per
// path. Not parallel: it measures allocations.
func TestCheckIgnoreKeepsRepeatedRulesOnce(t *testing.T) {
	repo := testutil.New(t)
	r := newRunner(t, repo)
	rule := "[" + strings.Repeat("a", 1<<20) + "]*\n"
	if err := os.WriteFile(filepath.Join(repo.Root, ".gitignore"), []byte(rule), 0600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 0; i < 64; i++ {
		paths = append(paths, fmt.Sprintf("a-%02d", i))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	matches, err := r.CheckIgnoreMatches(context.Background(), repo.Root, filepath.Join(repo.Root, ".git"), paths)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if m := matches[p]; !m.Ignored || m.Source != ".gitignore" || m.Line != 1 || len(m.Pattern) != len(rule)-1 {
			t.Fatalf("%s: %+v", p, m.Ignored)
		}
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("check-ignore of 64 paths matching one 1 MiB rule allocated %d MiB", allocated>>20)
	}
}

// An output bound that stops Git is reported as the safety limit, not as the
// exit status Git got when its output was cut (reset's filter attributes, and
// check-ignore records). Not parallel: it lowers MaxOutput.
func TestStoppedOutputReportsTheSafetyLimit(t *testing.T) {
	repo := testutil.New(t)
	r := newRunner(t, repo)
	ctx := context.Background()
	admin := filepath.Join(repo.Root, ".git")
	// Each path's filter value is longer than reset allows per value (and
	// within the 2,048 bytes Git reads of an attributes line).
	if err := os.WriteFile(filepath.Join(repo.Root, ".gitattributes"), []byte("* filter="+strings.Repeat("x", 1500)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("file-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(repo.Root, name), []byte("x\n"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	repo.Git("add", ".")
	repo.Git("commit", "-qm", "long filter values")
	commit := strings.TrimSpace(string(repo.Git("rev-parse", "HEAD")))
	if _, err := r.ResetFilters(ctx, repo.Root, admin, commit, paths); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("reset attribute response over its bound: %v", err)
	}
	saved := MaxOutput
	MaxOutput = 8 << 10
	t.Cleanup(func() { MaxOutput = saved })
	if err := os.WriteFile(filepath.Join(repo.Root, ".gitignore"), []byte("["+strings.Repeat("a", 64<<10)+"]*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckIgnoreMatches(ctx, repo.Root, admin, []string{"a-1", "a-2"}); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("check-ignore record over the limit: %v", err)
	}
}
