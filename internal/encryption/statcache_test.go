package encryption

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// cachedPlainID runs status until w's stat cache holds path, and returns the
// plaintext blob ID it keeps. The cache file is also copied under another
// workspace's name, as another worktree's status would leave it.
func cachedPlainID(t *testing.T, r *gitexec.Runner, w workspace.Context, root, path string) string {
	t.Helper()
	dir := filepath.Join(w.Common, "gitlocal", history.StatCacheDir)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := history.Status(context.Background(), r, root); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, w.WorkspaceID+".json"))
		if err == nil {
			var file struct {
				Entries map[string]struct {
					ID string `json:"id"`
				} `json:"entries"`
			}
			if err := json.Unmarshal(data, &file); err != nil {
				t.Fatal(err)
			}
			if id := file.Entries[path].ID; id != "" {
				if err := os.WriteFile(filepath.Join(dir, "4f9d2c1e-7a3b-4c5d-8e6f-1a2b3c4d5e6f.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				return id
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never cached %q", path)
		}
		time.Sleep(60 * time.Millisecond)
	}
}

// assertNoStatCacheHolds fails if any workspace's stat cache holds id.
func assertNoStatCacheHolds(t *testing.T, w workspace.Context, id string) {
	t.Helper()
	dir := filepath.Join(w.Common, "gitlocal", history.StatCacheDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(id)) {
			t.Fatalf("stat cache %s still holds the sensitive file's plaintext blob ID %s", entry.Name(), id)
		}
	}
}

// GL-138 review F3: once encrypt migrate makes a cached path sensitive, no
// stat cache, of any workspace, keeps its plaintext blob ID.
func TestMigrationDropsStatCaches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no stat cache")
	}
	r, w, root, key, _ := migrationFixture(t)
	ctx := context.Background()
	id := cachedPlainID(t, r, w, root, ".env.local")
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); err != nil {
		t.Fatal(err)
	}
	assertNoStatCacheHolds(t, w, id)
	if _, err := history.Status(ctx, r, root); err != nil {
		t.Fatal(err)
	}
	assertNoStatCacheHolds(t, w, id)
}

// GL-138 review F3: enabling encryption drops the plaintext blob IDs status
// cached before.
func TestEnableFreshDropsStatCaches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no stat cache")
	}
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, ".env.local"), []byte("TOKEN=private-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	id := cachedPlainID(t, r, *w, repo.Root, ".env.local")
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableFresh(ctx, r, repo.Root, keyPath, []string{".env.local"}); err != nil {
		t.Fatal(err)
	}
	assertNoStatCacheHolds(t, *w, id)
}
