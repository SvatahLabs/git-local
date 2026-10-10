package history

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Once grep's 64 MiB content budget is spent it stops and says so; it doesn't
// count the remaining files as oversized or run Git for each of them.
func TestGrepStopsAtContentBudget(t *testing.T) {
	ctx := context.Background()
	repo, g, w := fixture(t)
	line := strings.Repeat("filler text without the marker ", 32) + "\n"
	big := strings.Repeat(line, (maxGrepBlob-1)/len(line))
	// Each big file differs, so each is its own blob.
	for i := 0; i < 70; i++ {
		write(t, repo.Root, fmt.Sprintf("notes/a-%03d.txt", i), []byte(fmt.Sprintf("needle %d\n%s", i, big)))
	}
	for i := 0; i < 200; i++ {
		write(t, repo.Root, fmt.Sprintf("notes/z-%03d.txt", i), []byte("needle\n"))
	}
	change(t, g, repo.Root, true, "notes")
	save(t, g, repo.Root)
	before := g.SubprocessCount()
	result, err := Grep(ctx, g, repo.Root, w.WorkspaceID, "needle", 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	spent := g.SubprocessCount() - before
	if !result.Truncated || !result.BudgetExhausted || result.SkippedLarge != 0 {
		t.Fatalf("grep result: truncated=%v budget=%v skippedLarge=%d", result.Truncated, result.BudgetExhausted, result.SkippedLarge)
	}
	if len(result.Matches) < 60 || len(result.Matches) > 64 {
		t.Fatalf("searched files: %d matches", len(result.Matches))
	}
	// About two Git processes per searched file; the 200 files after the
	// budget would add at least 200 more.
	if spent > 64*3+60 {
		t.Fatalf("grep ran %d Git processes", spent)
	}
	t.Logf("Git processes: %d", spent)
	if !strings.Contains(result.Human(), "64 MiB") {
		t.Fatalf("human output: %s", result.Human())
	}
}
