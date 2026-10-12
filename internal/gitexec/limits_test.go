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
