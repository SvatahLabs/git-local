package encryption

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

// GL-124: encrypt migrate refuses a completed seed whose file list is lost
// before it rewrites anything: no migration journal, no policy, and only the
// Git processes of its checks. Before, it noticed only when converting the
// seed's journal, after rewriting every snapshot.
func TestMigrateRefusesLostSeedListBeforeRewritingHistory(t *testing.T) {
	t.Parallel()
	r, w, root, key, _, seedID := seededMigrationFixture(t)
	state := filepath.Join(w.Common, "gitlocal")
	testutil.SeedFilesBeside(t, state, seedID, true)
	if err := os.Remove(filepath.Join(state, "transactions", "seed-files", seedID+".json")); err != nil {
		t.Fatal(err)
	}
	// More history to rewrite makes a refusal after the rewrite costlier.
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if err := os.WriteFile(filepath.Join(root, ".env.local"), []byte(fmt.Sprintf("TOKEN=%d\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := history.Commit(ctx, r, root, "more history"); err != nil {
			t.Fatal(err)
		}
	}
	before := r.SubprocessCount()
	_, err := Migrate(ctx, r, root, key, []string{".env.local"})
	processes := r.SubprocessCount() - before
	t.Logf("Git processes before the refusal: %d", processes)
	var listErr *configtx.SeedListError
	if !errors.As(err, &listErr) || listErr.ID != seedID {
		t.Fatalf("migrate with a lost seed list: %v", err)
	}
	// Its checks run about 55; rewriting the history ran over 100 more.
	if processes > 80 {
		t.Fatalf("migrate ran %d Git processes before refusing", processes)
	}
	entries, err := os.ReadDir(filepath.Join(state, "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("refused migration left %s", entry.Name())
		}
	}
	if _, err := os.Lstat(filepath.Join(w.Common, secureblob.PolicyFile)); !os.IsNotExist(err) {
		t.Fatalf("refused migration published a policy: %v", err)
	}
}
