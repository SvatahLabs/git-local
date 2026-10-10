package encryption

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/keymaterial"
	"gitlocal/internal/restore"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/seed"
	"gitlocal/internal/storage"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

// legacyPolicyFixture gives a store encrypted under a policy from an earlier
// release that recorded the directory config/secrets as a file rule, so the
// files below it were saved unencrypted. It also leaves a completed seed and
// restore record that name those files. The returned markers are plaintext
// that must not remain anywhere in the private store after a correction.
type legacyFixture struct {
	r              *gitexec.Runner
	w              workspace.Context
	root, key      string
	linked, seedID string
	restoreID      string
	plainBlobs     []string
	markers        []string
	envBlob        string
}

func legacyPolicyFixture(t *testing.T) legacyFixture {
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
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(repo.Root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env.local", "TOKEN=env-private\n")
	write("config/secrets/a.txt", "A=legacy-secret-one\n")
	write("config/secrets/b.txt", "B=legacy-secret-two\n")
	write("config/public.txt", "public\n")
	if _, err := configtx.Change(ctx, r, repo.Root, true, ".env.local", "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Commit(ctx, r, repo.Root, "first snapshot"); err != nil {
		t.Fatal(err)
	}
	write("config/secrets/a.txt", "A=legacy-secret-three\n")
	if _, err := history.Commit(ctx, r, repo.Root, "second snapshot"); err != nil {
		t.Fatal(err)
	}
	f := legacyFixture{r: r, w: *w, root: repo.Root, markers: []string{"legacy-secret-one", "legacy-secret-two", "legacy-secret-three", "unsaved-secret-four"}}
	// A completed seed record that names the files below config/secrets.
	f.linked = filepath.Join(t.TempDir(), "seeded-linked")
	repo.Git("worktree", "add", "-q", "-b", "correction-seed", f.linked)
	seeded, err := seed.Run(ctx, r, f.linked, w.WorkspaceID, nil)
	if err != nil || seeded.TransactionID == "" {
		t.Fatalf("seed: %+v %v", seeded, err)
	}
	f.seedID = seeded.TransactionID
	// A completed restore whose recovery copy is an unsaved plaintext
	// version of a file below config/secrets.
	write("config/secrets/a.txt", "A=unsaved-secret-four\n")
	write(".env.local", "TOKEN=unsaved-env\n")
	restored, err := restore.Execute(ctx, r, repo.Root, w.WorkspaceID, "latest", []string{"config/secrets/a.txt", ".env.local"}, true)
	if err != nil || restored.TransactionID == "" {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	f.restoreID = restored.TransactionID
	objects, err := r.History(ctx, w.Store, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(objects)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "blob" {
			continue
		}
		data, err := r.History(ctx, w.Store, nil, "cat-file", "blob", fields[0])
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("legacy-secret-")) || bytes.Contains(data, []byte("unsaved-secret-")) {
			f.plainBlobs = append(f.plainBlobs, fields[0])
		}
	}
	if len(f.plainBlobs) != 4 {
		t.Fatalf("fixture plaintext blobs: %v", f.plainBlobs)
	}
	f.key = filepath.Join(t.TempDir(), "sensitive.key")
	if err := keymaterial.Create(f.key, repo.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, r, repo.Root, f.key, []string{".env.local"}); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	// The earlier release recorded config/secrets as a file rule.
	f.writePolicy(t, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}, {Path: "config/secrets", Kind: "file"}})
	latest, err := history.Resolve(ctx, r, repo.Root, w.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	f.envBlob = latest.Files[".env.local"].ID
	for _, blob := range f.plainBlobs {
		if _, err := r.History(ctx, w.Store, nil, "cat-file", "-t", blob); err != nil {
			t.Fatalf("fixture lost legacy plaintext blob %s: %v", blob, err)
		}
	}
	return f
}

func (f legacyFixture) writePolicy(t *testing.T, rules []secureblob.PathRule) {
	t.Helper()
	policy, err := secureblob.PolicyForKey(f.key, f.w.Root, f.w.Common, rules)
	if err != nil {
		t.Fatal(err)
	}
	common, err := storage.OpenDir(f.w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	if err := secureblob.WriteLocked(common, policy, f.w.Root, f.w.Common); err != nil {
		t.Fatal(err)
	}
}

func (f legacyFixture) policy(t *testing.T) secureblob.Policy {
	t.Helper()
	common, err := storage.OpenDir(f.w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer common.Close()
	policy, exists, err := secureblob.ReadLocked(common, f.w.Root, f.w.Common)
	if err != nil || !exists {
		t.Fatalf("policy: %v %v", exists, err)
	}
	return policy
}

func (f legacyFixture) correct(fault migrationFault) (string, error) {
	return migrate(context.Background(), f.r, f.root, f.key, []string{".env.local", "config/secrets"}, fault)
}

// checkCorrected asserts the corrected policy is in service, no plaintext of
// the files below config/secrets remains in the private store (raw object
// scan), every snapshot and completed record is consistent with the new
// policy, and snapshots work again.
func (f legacyFixture) checkCorrected(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	want := []secureblob.PathRule{{Path: ".env.local", Kind: "file"}, {Path: "config/secrets", Kind: "directory"}}
	if got := f.policy(t).Sensitive; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("corrected policy rules: %+v", got)
	}
	for _, blob := range f.plainBlobs {
		if _, err := f.r.History(ctx, f.w.Store, nil, "cat-file", "-t", blob); err == nil {
			t.Fatalf("plaintext blob %s remains in the live store", blob)
		}
	}
	gitDir := filepath.Join(f.w.Common, "gitlocal")
	objects, err := f.r.History(ctx, f.w.Store, nil, "cat-file", "--batch-all-objects", "--batch")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range f.markers {
		if bytes.Contains(objects, []byte(marker)) {
			t.Fatalf("plaintext %q remains in the private store", marker)
		}
	}
	count, err := history.AuditWorkspaceLocked(ctx, f.r, f.w, f.w.WorkspaceID)
	if err != nil || count < 2 {
		t.Fatalf("corrected audit: %d %v", count, err)
	}
	latest, err := history.Resolve(ctx, f.r, f.root, f.w.WorkspaceID, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Files[".env.local"].ID != f.envBlob {
		t.Fatal("already encrypted blob was re-encrypted")
	}
	for _, path := range []string{"config/secrets/a.txt", "config/secrets/b.txt"} {
		if latest.Metadata.EncryptedDigests[path] == "" {
			t.Fatalf("%s isn't encrypted in the latest snapshot", path)
		}
	}
	if latest.Metadata.EncryptedDigests["config/public.txt"] != "" {
		t.Fatal("public file was encrypted")
	}
	if data, err := history.Show(ctx, f.r, f.root, f.w.WorkspaceID, "latest", "config/secrets/a.txt"); err != nil || string(data) != "A=legacy-secret-three\n" {
		t.Fatalf("show corrected first version: %q %v", data, err)
	}
	if err := configtx.ReadDiagnostic(ctx, f.r, f.root, func(d configtx.DiagnosticState) error {
		return restore.InspectJournal(ctx, f.r, d.Workspace, d.Selection, d.ConfigDigest, f.restoreID, true, d.Sources)
	}); err != nil {
		t.Fatalf("converted restore record failed validation: %v", err)
	}
	var restored struct {
		Manifest struct {
			Plan struct {
				Operations []struct {
					Path      string `json:"path"`
					Encrypted bool   `json:"encrypted"`
				} `json:"operations"`
			} `json:"plan"`
		} `json:"manifest"`
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "transactions", "completed", "restore-"+f.restoreID+".json"))
	if err != nil || json.Unmarshal(data, &restored) != nil || len(restored.Manifest.Plan.Operations) != 2 {
		t.Fatalf("converted restore record: %v", err)
	}
	for _, op := range restored.Manifest.Plan.Operations {
		if !op.Encrypted {
			t.Fatalf("restore image %q isn't encrypted", op.Path)
		}
	}
	var seeded struct {
		Files []struct {
			Path        string `json:"path"`
			PlainDigest string `json:"plainDigest"`
		} `json:"files"`
	}
	data, err = os.ReadFile(filepath.Join(gitDir, "transactions", "completed", "seed-"+f.seedID+".json"))
	if err != nil || json.Unmarshal(data, &seeded) != nil {
		t.Fatalf("converted seed record: %v", err)
	}
	for _, file := range seeded.Files {
		if strings.HasPrefix(file.Path, "config/secrets/") != (file.PlainDigest != "") && file.Path != ".env.local" {
			t.Fatalf("seed record file %q digest %q", file.Path, file.PlainDigest)
		}
	}
	if _, err := configtx.RecoverSeed(ctx, f.r, f.linked, f.seedID, false); err != nil {
		t.Fatalf("converted seed record no longer validates: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "config", "secrets", "c.txt"), []byte("C=after-correction\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := history.Commit(ctx, f.r, f.root, "after correction")
	if err != nil || !result.Changed || len(result.Warnings) != 0 {
		t.Fatalf("snapshot after correction: %+v %v", result, err)
	}
	latest, err = history.Resolve(ctx, f.r, f.root, f.w.WorkspaceID, "latest")
	if err != nil || latest.Metadata.EncryptedDigests["config/secrets/c.txt"] == "" {
		t.Fatalf("new file below the corrected rule isn't encrypted: %v", err)
	}
}

func TestMigrateCorrectsFileRuleNamingADirectory(t *testing.T) {
	f := legacyPolicyFixture(t)
	ctx := context.Background()
	// Snapshots that would save a mis-covered file refuse, naming the command.
	if _, err := history.Commit(ctx, f.r, f.root, "legacy"); !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "git local encrypt migrate") {
		t.Fatalf("snapshot under the legacy policy: %v", err)
	}
	id, err := Migrate(ctx, f.r, f.root, f.key, []string{".env.local", "config/secrets"})
	if err != nil || !storage.ValidUUID(id) {
		t.Fatalf("correct policy: %s %v", id, err)
	}
	if _, err := os.Stat(filepath.Join(f.w.Common, "gitlocal", journalName(id))); !os.IsNotExist(err) {
		t.Fatalf("journal remains after correction: %v", err)
	}
	f.checkCorrected(t)
}

func TestPolicyCorrectionRefusesNarrowingKeyChangeAndNoChange(t *testing.T) {
	f := legacyPolicyFixture(t)
	ctx := context.Background()
	other := filepath.Join(t.TempDir(), "other.key")
	if err := keymaterial.Create(other, f.root); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		key   string
		paths []string
		want  string
	}{
		"narrowing":  {f.key, []string{"config/secrets"}, `doesn't cover sensitive path ".env.local"`},
		"other key":  {other, []string{".env.local", "config/secrets"}, "different key"},
		"no change":  {f.key, []string{".env.local", "config/secrets"}, ""},
		"misspelled": {f.key, []string{".env.local", "config/Secrets"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "no change" {
				f.writePolicy(t, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}, {Path: "config/secrets", Kind: "directory"}})
				defer f.writePolicy(t, []secureblob.PathRule{{Path: ".env.local", Kind: "file"}, {Path: "config/secrets", Kind: "file"}})
				tc.want = "already enabled with these paths"
			}
			if name == "misspelled" {
				// config/Secrets doesn't exist; the rule must be spelled as on disk.
				tc.want = "config/Secrets"
			}
			_, err := Migrate(ctx, f.r, f.root, tc.key, tc.paths)
			if !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("migrate %v: %v", tc.paths, err)
			}
		})
	}
	for _, blob := range f.plainBlobs[:1] {
		if _, err := f.r.History(ctx, f.w.Store, nil, "cat-file", "-t", blob); err != nil {
			t.Fatalf("refused correction changed the store: %v", err)
		}
	}
	if got := f.policy(t).Sensitive; got[1].Kind != "file" {
		t.Fatalf("refused correction changed the policy: %+v", got)
	}
}

func TestPolicyCorrectionRecoversAtEveryPublicationPoint(t *testing.T) {
	for _, point := range []string{"journal", "old-store-moved", "encrypted-store-published", "policy-published", "record-updates"} {
		t.Run(point, func(t *testing.T) {
			f := legacyPolicyFixture(t)
			ctx := context.Background()
			stop := func(at string) error {
				if at == point {
					return errors.New("injected interruption")
				}
				return nil
			}
			id, err := f.correct(stop)
			if !errors.Is(err, storage.ErrConflict) || !storage.ValidUUID(id) {
				t.Fatalf("interruption: %s %v", id, err)
			}
			published := point == "policy-published" || point == "record-updates"
			pending, err := configtx.PendingJournals(ctx, f.r, f.root)
			if err != nil || len(pending) != 1 || pending[0].ID != id || pending[0].Kind != "encrypt" || pending[0].Err != nil || pending[0].Published != published {
				t.Fatalf("pending correction: %+v %v", pending, err)
			}
			if published {
				if err := RecoverMigration(ctx, f.r, f.root, id, "rollback"); err == nil {
					t.Fatal("rollback accepted after the corrected policy was published")
				}
			} else {
				if err := RecoverMigration(ctx, f.r, f.root, id, "rollback"); err != nil {
					t.Fatalf("rollback: %v", err)
				}
				if got := f.policy(t).Sensitive; len(got) != 2 || got[1].Kind != "file" {
					t.Fatalf("rollback didn't keep the old policy: %+v", got)
				}
				for _, blob := range f.plainBlobs {
					if _, err := f.r.History(ctx, f.w.Store, nil, "cat-file", "-t", blob); err != nil {
						t.Fatalf("rollback lost the original history: %v", err)
					}
				}
				if count, err := history.AuditWorkspaceLocked(ctx, f.r, f.w, f.w.WorkspaceID); err != nil || count != 2 {
					t.Fatalf("original history after rollback: %d %v", count, err)
				}
				if id, err = f.correct(stop); !errors.Is(err, storage.ErrConflict) {
					t.Fatalf("second interruption: %v", err)
				}
			}
			if err := RecoverMigration(ctx, f.r, f.root, id, "resume"); err != nil {
				t.Fatalf("resume: %v", err)
			}
			f.checkCorrected(t)
		})
	}
}

func TestPolicyCorrectionAbandonBeforePublicationKeepsOldPolicy(t *testing.T) {
	f := legacyPolicyFixture(t)
	ctx := context.Background()
	id, err := f.correct(func(at string) error {
		if at == "journal" {
			return errors.New("injected interruption")
		}
		return nil
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("interruption: %v", err)
	}
	result, err := AbandonMigration(ctx, f.r, f.root, id)
	if err != nil || result.Quarantine == "" {
		t.Fatalf("abandon: %+v %v", result, err)
	}
	if got := f.policy(t).Sensitive; got[1].Kind != "file" {
		t.Fatalf("abandon changed the policy: %+v", got)
	}
	if _, err := Migrate(ctx, f.r, f.root, f.key, []string{".env.local", "config/secrets"}); err != nil {
		t.Fatalf("correction after abandon: %v", err)
	}
}
