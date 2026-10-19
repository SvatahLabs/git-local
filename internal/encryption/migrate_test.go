package encryption

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/keymaterial"
	"github.com/SvatahLabs/git-local/internal/restore"
	"github.com/SvatahLabs/git-local/internal/secureblob"
	"github.com/SvatahLabs/git-local/internal/seed"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func migrationFixture(t *testing.T) (*gitexec.Runner, workspace.Context, string, string, *testutil.Repo) {
	t.Helper()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ctx := context.Background()
	w, err := workspace.Init(ctx, r, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(repo.Root, ".env.local")
	if err := os.WriteFile(secret, []byte("TOKEN=old-private\x00value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "notes.txt"), []byte("public v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, repo.Root, "first snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("TOKEN=new-private\x00value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, repo.Root, "second snapshot"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(keyPath, repo.Root); err != nil {
		t.Fatal(err)
	}
	return r, *w, repo.Root, keyPath, repo
}

func TestMigrateExistingHistoryEncryptsEverySnapshot(t *testing.T) {
	r, w, root, key, _ := migrationFixture(t)
	ctx := context.Background()
	old, err := history.Resolve(ctx, r, root, w.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	plainID := old.Files[".env.local"].ID
	oldPublicID := old.Files["notes.txt"].ID
	ancestry, err := r.History(ctx, w.Store, nil, "rev-list", "--reverse", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstID := strings.Fields(string(ancestry))[0]
	first, err := history.Resolve(ctx, r, root, w.WorkspaceID, firstID)
	if err != nil {
		t.Fatal(err)
	}
	firstPlainID := first.Files[".env.local"].ID
	id, err := Migrate(ctx, r, root, key, []string{".env.local"})
	if err != nil || !storage.ValidUUID(id) {
		t.Fatalf("migrate: %s %v", id, err)
	}
	if _, err := r.History(ctx, w.Store, nil, "cat-file", "-t", plainID); err == nil {
		t.Fatal("old sensitive plaintext blob remains in live history store")
	}
	if _, err := r.History(ctx, w.Store, nil, "cat-file", "-t", firstPlainID); err == nil {
		t.Fatal("older sensitive plaintext blob remains in live history store")
	}
	if count, err := history.AuditWorkspaceLocked(ctx, r, w, w.WorkspaceID); err != nil || count != 2 {
		t.Fatalf("migrated audit: %d %v", count, err)
	}
	latest, err := history.Resolve(ctx, r, root, w.WorkspaceID, "latest")
	if err != nil || latest.Metadata.EncryptedDigests[".env.local"] == "" || latest.Metadata.SchemaVersion != 2 {
		t.Fatalf("migrated latest: %+v %v", latest.Metadata, err)
	}
	if latest.Files["notes.txt"].ID != oldPublicID {
		t.Fatal("public file blob changed during migration")
	}
	if _, err := os.Stat(filepath.Join(w.Common, "gitlocal", backupName(id))); !os.IsNotExist(err) {
		t.Fatalf("plaintext backup remains: %v", err)
	}
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("second migration accepted: %v", err)
	}
}

func TestMigrationRollbackWorksWithoutKey(t *testing.T) {
	r, w, root, key, _ := migrationFixture(t)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
		if point == "old-store-moved" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("migration interruption: %v", err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMigration(ctx, r, root, id, "rollback"); err != nil {
		t.Fatalf("rollback without key: %v", err)
	}
	if count, err := history.AuditWorkspaceLocked(ctx, r, w, w.WorkspaceID); err != nil || count != 2 {
		t.Fatalf("restored original history: %d %v", count, err)
	}
}

func seededMigrationFixture(t *testing.T) (*gitexec.Runner, workspace.Context, string, string, string, string) {
	t.Helper()
	r, w, root, key, repo := migrationFixture(t)
	linked := filepath.Join(t.TempDir(), "seeded-linked")
	repo.Git("worktree", "add", "-q", "-b", "migration-seed", linked)
	seeded, err := seed.Run(context.Background(), r, linked, w.WorkspaceID, nil)
	if err != nil || seeded.TransactionID == "" {
		t.Fatalf("seed migration fixture: %+v %v", seeded, err)
	}
	return r, w, root, key, linked, seeded.TransactionID
}

func seedJournalEvidence(t *testing.T, w workspace.Context, id string) (int, string, string, string) {
	t.Helper()
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	var journal struct {
		SchemaVersion  int    `json:"schemaVersion"`
		Snapshot       string `json:"sourceSnapshot"`
		KeyFingerprint string `json:"keyFingerprint"`
		FilesDigest    string `json:"filesDigest"`
		Files          []struct {
			Path        string `json:"path"`
			Blob        string `json:"blob"`
			PlainDigest string `json:"plainDigest"`
		} `json:"files"`
	}
	data, err := common.ReadFile("gitlocal/transactions/completed/seed-"+id+".json", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.FilesDigest != "" {
		// The file list kept beside the journal must match its digest.
		list, err := common.ReadFile("gitlocal/transactions/seed-files/"+id+".json", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(list); hex.EncodeToString(sum[:]) != journal.FilesDigest || len(journal.Files) != 0 {
			t.Fatal("seed file list beside the journal doesn't match it")
		}
		if err := json.Unmarshal(list, &journal); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range journal.Files {
		if file.Path == ".env.local" {
			return journal.SchemaVersion, journal.Snapshot, file.Blob, file.PlainDigest
		}
	}
	t.Fatal("completed seed journal has no sensitive file")
	return 0, "", "", ""
}

// A large seed keeps its file list beside the journal (GL-111); migration
// converts both forms.
func TestMigrationConvertsCompletedSeedJournal(t *testing.T) {
	for _, beside := range []bool{false, true} {
		t.Run(fmt.Sprintf("files-beside=%v", beside), func(t *testing.T) {
			convertsCompletedSeedJournal(t, beside)
		})
	}
}

func convertsCompletedSeedJournal(t *testing.T, beside bool) {
	r, w, root, key, linked, seedID := seededMigrationFixture(t)
	if beside {
		testutil.SeedFilesBeside(t, filepath.Join(w.Common, "gitlocal"), seedID, true)
	}
	ctx := context.Background()
	oldSchema, oldSnapshot, oldBlob, oldDigest := seedJournalEvidence(t, w, seedID)
	if oldSchema != 1 || oldDigest != "" {
		t.Fatal("fixture did not create a plaintext seed journal")
	}
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); err != nil {
		t.Fatalf("migrate completed seed: %v", err)
	}
	schema, snapshot, blob, digest := seedJournalEvidence(t, w, seedID)
	if schema != 2 || snapshot == oldSnapshot || blob == oldBlob || digest == "" {
		t.Fatalf("seed journal was not converted: schema=%d snapshot=%s blob=%s digest=%s", schema, snapshot, blob, digest)
	}
	latest, err := history.Resolve(ctx, r, root, w.WorkspaceID, "latest")
	if err != nil || snapshot != latest.ID || blob != latest.Files[".env.local"].ID || digest != latest.Metadata.EncryptedDigests[".env.local"] {
		t.Fatalf("converted seed journal differs from history: %v", err)
	}
	if _, err := configtx.RecoverSeed(ctx, r, linked, seedID, false); err != nil {
		t.Fatalf("completed seed recovery no longer validates: %v", err)
	}
	if beside {
		list, err := os.ReadFile(filepath.Join(w.Common, "gitlocal", "transactions", "seed-files", seedID+".json"))
		if err != nil || bytes.Contains(list, []byte(oldBlob)) {
			t.Fatalf("plaintext blob ID kept beside the converted journal: %v", err)
		}
	}
}

func TestMigrationSeedJournalPublicationRecovery(t *testing.T) {
	for _, point := range []string{"policy-published", "record-updates", "record-updates/files-beside"} {
		t.Run(point, func(t *testing.T) {
			r, w, root, key, linked, seedID := seededMigrationFixture(t)
			if strings.HasSuffix(point, "/files-beside") {
				testutil.SeedFilesBeside(t, filepath.Join(w.Common, "gitlocal"), seedID, true)
			}
			point := strings.TrimSuffix(point, "/files-beside")
			ctx := context.Background()
			id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(at string) error {
				if at == point {
					return errors.New("injected interruption")
				}
				return nil
			})
			if !errors.Is(err, storage.ErrConflict) {
				t.Fatalf("seed migration interruption: %v", err)
			}
			if err := RecoverMigration(ctx, r, root, id, "resume"); err != nil {
				t.Fatalf("resume seed journal publication: %v", err)
			}
			schema, _, _, digest := seedJournalEvidence(t, w, seedID)
			if schema != 2 || digest == "" {
				t.Fatal("seed journal conversion was not resumed")
			}
			if _, err := configtx.RecoverSeed(ctx, r, linked, seedID, false); err != nil {
				t.Fatalf("converted seed journal invalid: %v", err)
			}
		})
	}
}

func restoredMigrationFixture(t *testing.T) (*gitexec.Runner, workspace.Context, string, string, string, string) {
	t.Helper()
	r, w, root, key, _ := migrationFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".env.local"), []byte("TOKEN=unsaved-preimage\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := restore.Execute(context.Background(), r, root, w.WorkspaceID, "latest", []string{".env.local"}, true)
	if err != nil || result.TransactionID == "" {
		t.Fatalf("completed restore fixture: %+v %v", result, err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	data, err := common.ReadFile("gitlocal/transactions/completed/restore-"+result.TransactionID+".json", 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Manifest struct {
			Plan struct {
				Operations []struct {
					Before struct {
						BlobID string `json:"blobId"`
					} `json:"before"`
				} `json:"operations"`
			} `json:"plan"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(data, &old); err != nil || len(old.Manifest.Plan.Operations) != 1 {
		t.Fatalf("old restore journal: %v", err)
	}
	return r, w, root, key, result.TransactionID, old.Manifest.Plan.Operations[0].Before.BlobID
}

func checkConvertedRestore(t *testing.T, ctx context.Context, r *gitexec.Runner, w workspace.Context, root, id, oldPreimage string) {
	t.Helper()
	if _, err := r.History(ctx, w.Store, nil, "cat-file", "-t", oldPreimage); err == nil {
		t.Fatal("unsaved restore preimage remains plaintext in migrated store")
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	data, err := common.ReadFile("gitlocal/transactions/completed/restore-"+id+".json", 16<<20)
	common.Close()
	if err != nil {
		t.Fatal(err)
	}
	var updated struct {
		SchemaVersion int    `json:"schemaVersion"`
		Recovery      string `json:"recoverySnapshot"`
		Manifest      struct {
			BeforeStored []string `json:"beforeStored"`
			Plan         struct {
				Operations []struct {
					Encrypted bool `json:"encrypted"`
					Before    struct {
						KeyedDigest string `json:"keyedDigest"`
						BlobID      string `json:"blobId"`
					} `json:"before"`
				} `json:"operations"`
			} `json:"plan"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(data, &updated); err != nil || updated.SchemaVersion != 2 || len(updated.Manifest.BeforeStored) != 1 || len(updated.Manifest.Plan.Operations) != 1 {
		t.Fatalf("converted restore journal: %+v %v", updated, err)
	}
	op := updated.Manifest.Plan.Operations[0]
	if !op.Encrypted || op.Before.KeyedDigest == "" || op.Before.BlobID != "" || updated.Manifest.BeforeStored[0] == oldPreimage {
		t.Fatalf("restore preimage evidence remains unprotected: %+v", op)
	}
	if err := configtx.ReadDiagnostic(ctx, r, root, func(d configtx.DiagnosticState) error {
		return restore.InspectJournal(ctx, r, d.Workspace, d.Selection, d.ConfigDigest, id, true, d.Sources)
	}); err != nil {
		t.Fatalf("converted restore journal failed validation: %v", err)
	}
}

func TestMigrationConvertsCompletedRestoreRecovery(t *testing.T) {
	r, w, root, key, id, preimage := restoredMigrationFixture(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); err != nil {
		t.Fatalf("migrate restore recovery: %v", err)
	}
	checkConvertedRestore(t, ctx, r, w, root, id, preimage)
}

func TestMigrationRestoreRecoveryPublication(t *testing.T) {
	for _, point := range []string{"journal", "policy-published", "record-updates"} {
		t.Run(point, func(t *testing.T) {
			r, w, root, key, restoreID, preimage := restoredMigrationFixture(t)
			ctx := context.Background()
			id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(at string) error {
				if at == point {
					return errors.New("injected interruption")
				}
				return nil
			})
			if !errors.Is(err, storage.ErrConflict) {
				t.Fatalf("restore migration interruption: %v", err)
			}
			if _, err := workspace.Init(ctx, r, root); err == nil || !strings.Contains(err.Error(), "git local encrypt recover") {
				t.Fatalf("init did not explain pending migration: %v", err)
			}
			if point == "journal" {
				if err := RecoverMigration(ctx, r, root, id, "rollback"); err != nil {
					t.Fatalf("rollback restore migration: %v", err)
				}
				if _, err := r.History(ctx, w.Store, nil, "cat-file", "-t", preimage); err != nil {
					t.Fatalf("rollback lost original recovery preimage: %v", err)
				}
				return
			}
			if err := RecoverMigration(ctx, r, root, id, "resume"); err != nil {
				t.Fatalf("resume restore migration: %v", err)
			}
			checkConvertedRestore(t, ctx, r, w, root, restoreID, preimage)
		})
	}
}

func TestMigrationRejectsDamagedStagedRecoveryBeforePublication(t *testing.T) {
	r, w, root, key, restoreID, _ := restoredMigrationFixture(t)
	ctx := context.Background()
	id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(at string) error {
		if at == "journal" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("migration interruption: %v", err)
	}
	meta, err := storage.OpenDir(filepath.Join(w.Common, "gitlocal"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := meta.ReadFile(filepath.Join(recordsName(id), "restore-"+restoreID+".json"), 16<<20)
	meta.Close()
	if err != nil {
		t.Fatal(err)
	}
	var prepared struct {
		Manifest struct {
			BeforeStored []string `json:"beforeStored"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(data, &prepared); err != nil || len(prepared.Manifest.BeforeStored) != 1 {
		t.Fatalf("prepared recovery journal: %v", err)
	}
	blob := prepared.Manifest.BeforeStored[0]
	stage := filepath.Join(w.Common, "gitlocal", stageName(id))
	if err := os.Remove(filepath.Join(stage, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMigration(ctx, r, root, id, "resume"); err == nil {
		t.Fatal("damaged recovery image accepted")
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	_, published, err := secureblob.ReadLocked(common, w.Root, w.Common)
	common.Close()
	if err != nil || published {
		t.Fatalf("policy published after damaged recovery: %v", err)
	}
	if err := RecoverMigration(ctx, r, root, id, "rollback"); err != nil {
		t.Fatalf("rollback damaged migration: %v", err)
	}
}

func TestMigrationRecoversAtPublicationBoundaries(t *testing.T) {
	for _, at := range []string{"journal", "old-store-moved", "encrypted-store-published", "policy-published"} {
		t.Run(at, func(t *testing.T) {
			r, w, root, key, _ := migrationFixture(t)
			ctx := context.Background()
			id, err := migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
				if point == at {
					return errors.New("injected interruption")
				}
				return nil
			})
			if !errors.Is(err, storage.ErrConflict) || !storage.ValidUUID(id) {
				t.Fatalf("interruption: %s %v", id, err)
			}
			if at == "old-store-moved" {
				if _, err := history.Resolve(ctx, r, root, w.WorkspaceID, "latest"); err == nil || !strings.Contains(err.Error(), "git local encrypt recover") {
					t.Fatalf("missing migration recovery diagnostic: %v", err)
				}
			}
			if at != "policy-published" {
				if err := RecoverMigration(ctx, r, root, id, "rollback"); err != nil {
					t.Fatalf("rollback: %v", err)
				}
				if _, err := os.Stat(filepath.Join(w.Common, "gitlocal", journalName(id))); !os.IsNotExist(err) {
					t.Fatalf("journal remains after rollback: %v", err)
				}
				if count, err := history.AuditWorkspaceLocked(ctx, r, w, w.WorkspaceID); err != nil || count != 2 {
					t.Fatalf("original history after rollback: %d %v", count, err)
				}
				id, err = migrate(ctx, r, root, key, []string{".env.local"}, func(point string) error {
					if point == at {
						return errors.New("injected interruption")
					}
					return nil
				})
				if !errors.Is(err, storage.ErrConflict) {
					t.Fatalf("second interruption: %v", err)
				}
			} else if err := RecoverMigration(ctx, r, root, id, "rollback"); err == nil {
				t.Fatal("rollback accepted after policy publication")
			}
			if err := RecoverMigration(ctx, r, root, id, "resume"); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if count, err := history.AuditWorkspaceLocked(ctx, r, w, w.WorkspaceID); err != nil || count != 2 {
				t.Fatalf("encrypted history after resume: %d %v", count, err)
			}
		})
	}
}

func TestMigrationRefusesRecoveryRefWithoutPublication(t *testing.T) {
	r, w, root, key, _ := migrationFixture(t)
	ctx := context.Background()
	old, err := history.Resolve(ctx, r, root, w.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.History(ctx, w.Store, nil, "update-ref", "refs/gitlocal/recovery/"+w.WorkspaceID+"/test", old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("unsupported recovery ref accepted: %v", err)
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	if _, err := common.ReadFile("gitlocal/encryption.json", 1<<20); !os.IsNotExist(err) {
		t.Fatalf("policy published after rejected migration: %v", err)
	}
	contents, err := r.History(ctx, w.Store, nil, "cat-file", "blob", old.Files[".env.local"].ID)
	if err != nil || !bytes.Contains(contents, []byte("TOKEN=new-private")) {
		t.Fatalf("original history changed: %v", err)
	}
	if _, err := r.History(ctx, w.Store, nil, "update-ref", "-d", "refs/gitlocal/recovery/"+w.WorkspaceID+"/test"); err != nil {
		t.Fatal(err)
	}
	restoreID, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := common.AtomicWrite("gitlocal/transactions/completed/restore-"+restoreID+".json", []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("completed restore journal accepted without conversion: %v", err)
	}
}

func TestMigrationIncludesRetainedWorkspaceHistory(t *testing.T) {
	r, w, root, key, repo := migrationFixture(t)
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "migration-retained", linked)
	retained, err := workspace.Init(ctx, r, linked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".env.local"), []byte("TOKEN=retained-private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configtx.Change(ctx, r, linked, true, ".env.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, linked, "retained snapshot"); err != nil {
		t.Fatal(err)
	}
	old, err := history.Resolve(ctx, r, root, retained.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	repo.Git("worktree", "remove", "--force", linked)
	if result, err := configtx.Prune(ctx, r, root, false); err != nil || len(result.Retired) != 1 || result.Retired[0] != retained.WorkspaceID {
		t.Fatalf("retire linked workspace: %+v %v", result, err)
	}
	if _, err := Migrate(ctx, r, root, key, []string{".env.local"}); err != nil {
		t.Fatalf("migrate retained history: %v", err)
	}
	if count, err := history.AuditWorkspaceLocked(ctx, r, w, retained.WorkspaceID); err != nil || count != 1 {
		t.Fatalf("migrated retained workspace audit: %d %v", count, err)
	}
	if _, err := r.History(ctx, w.Store, nil, "cat-file", "-t", old.Files[".env.local"].ID); err == nil {
		t.Fatal("retained workspace plaintext blob remains")
	}
}
