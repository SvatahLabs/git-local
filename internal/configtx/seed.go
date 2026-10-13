package configtx

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type SeedFile struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	Blob        string `json:"blob"`
	PlainDigest string `json:"plainDigest,omitempty"`
	Stage       string `json:"stage"`
}
type SeedPlan struct {
	SourceWorkspace string
	SourceSnapshot  string
	KeyFingerprint  string
	Selection       selection.State
	Files           []SeedFile
}
type SeedPlanner func(workspace.Context) (SeedPlan, error)
type SeedResult struct {
	WorkspaceID, TransactionID string
	Warnings                   []string
}
type seedJournal struct {
	SchemaVersion  int                `json:"schemaVersion"`
	Kind           string             `json:"kind"`
	ID             string             `json:"id"`
	RepositoryID   string             `json:"repositoryId"`
	WorkspaceID    string             `json:"workspaceId"`
	Source         string             `json:"sourceWorkspaceId"`
	Snapshot       string             `json:"sourceSnapshot"`
	KeyFingerprint string             `json:"keyFingerprint,omitempty"`
	Direction      string             `json:"direction"`
	Outcome        string             `json:"outcome,omitempty"`
	Config         Journal            `json:"config"`
	Files          []SeedFile         `json:"files"`
	Directories    []string           `json:"createDirectories"`
	Ownership      selection.Evidence `json:"ownership"`
	// FilesDigest names a file list kept beside the journal (seedFilesPath)
	// when the journal with its files would exceed JournalLimit; Files is then
	// empty on disk. A journal that fits keeps its files inline, as earlier
	// releases wrote and read them.
	FilesDigest string `json:"filesDigest,omitempty"`
}

// seedFileList is the content of a seed's file list kept beside its journal.
type seedFileList struct {
	Files []SeedFile `json:"files"`
}

const seedFilesDir = "seed-files"

// seedFilesLimit is the size limit of a seed's file list kept beside its
// journal, for writing and reading alike; tests lower it.
var seedFilesLimit = 64 << 20

func seedPath(id string) string          { return "transactions/seed-" + id + ".json" }
func seedCompletedPath(id string) string { return "transactions/completed/seed-" + id + ".json" }

// seedFilesPath stays the same when the journal is archived.
func seedFilesPath(id string) string { return "transactions/" + seedFilesDir + "/" + id + ".json" }

// encodeSeed returns the journal and, for a journal whose files don't fit in
// JournalLimit, the file list to write beside it first. A list that is already
// beside the journal (FilesDigest) is never written again.
func encodeSeed(j seedJournal) ([]byte, []byte, error) {
	if j.FilesDigest != "" {
		j.Files = []SeedFile{}
		b, err := encode(j)
		return b, nil, err
	}
	b, err := encode(j)
	if !errors.Is(err, errJournalLimit) {
		return b, nil, err
	}
	return encodeSeedBeside(j)
}

// encodeSeedBeside is encodeSeed for a file list kept beside the journal.
func encodeSeedBeside(j seedJournal) ([]byte, []byte, error) {
	files, err := json.MarshalIndent(seedFileList{j.Files}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	files = append(files, '\n')
	if len(files) > seedFilesLimit {
		return nil, nil, fmt.Errorf("seeding %d files exceeds the %d MiB limit of a seed's file list; seed a subset with git local init --from <source> --paths <path>...", len(j.Files), seedFilesLimit>>20)
	}
	j.FilesDigest = digestOf(files)
	j.Files = []SeedFile{}
	// The journal's later writes have no smaller form to fall back to.
	b, err := encodeNew(j)
	if errors.Is(err, errJournalLimit) {
		err = fmt.Errorf("seed journal exceeds its 1 MiB limit even without its file list (selection, ignore rules or created directories); seed a subset with git local init --from <source> --paths <path>...")
	}
	return b, files, err
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// decodeSeed decodes a seed journal, reading a file list kept beside it with
// files and checking it against the journal's digest.
func decodeSeed(data []byte, files func(id string) ([]byte, error)) (seedJournal, error) {
	var j seedJournal
	if err := decode(data, &j); err != nil {
		return j, err
	}
	if j.FilesDigest == "" {
		return j, nil
	}
	if len(j.Files) != 0 || !validSeedDigest(j.FilesDigest) || !storage.ValidUUID(j.ID) {
		return j, errors.New("invalid seed journal")
	}
	list, err := files(j.ID)
	if err != nil {
		return j, err
	}
	if digestOf(list) != j.FilesDigest {
		return j, fmt.Errorf("%w: seed file list %s doesn't match its journal", storage.ErrConflict, seedFilesPath(j.ID))
	}
	var decoded seedFileList
	if err := decode(list, &decoded); err != nil {
		return j, err
	}
	j.Files = decoded.Files
	return j, nil
}

// readSeed reads a pending or completed seed journal from meta.
func readSeed(meta *storage.Dir, name string) (seedJournal, error) {
	data, err := meta.ReadFile(name, JournalLimit)
	if err != nil {
		return seedJournal{}, err
	}
	j, err := decodeSeed(data, func(id string) ([]byte, error) { return ReadSeedFiles(meta, id) })
	if err != nil {
		return j, fmt.Errorf("invalid metadata %s: %w", name, err)
	}
	return j, nil
}

// ReadSeedFiles reads the file list kept beside seed journal id in the
// private state directory meta.
func ReadSeedFiles(meta *storage.Dir, id string) ([]byte, error) {
	return meta.ReadFile(seedFilesPath(id), SeedFilesLimit())
}

// SeedFilesLimit is the size limit of a seed's file list kept beside its
// journal.
func SeedFilesLimit() int64 { return int64(seedFilesLimit) }

// WriteSeedFiles writes the file list of seed journal id beside it; the
// journal that names it is written afterwards.
func WriteSeedFiles(meta *storage.Dir, id string, files []byte) error {
	if err := meta.MkdirAll("transactions/" + seedFilesDir); err != nil {
		return err
	}
	return meta.AtomicWrite(seedFilesPath(id), files, 0600)
}

func oidValid(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// Seed journals selection, ignore rules, and every created working file as a
// single recoverable operation. It requires a freshly initialized, empty
// destination workspace; initialization itself is idempotent bootstrap.
func Seed(ctx context.Context, r *gitexec.Runner, cwd string, planner SeedPlanner) (SeedResult, error) {
	return seed(ctx, r, cwd, planner, nil)
}
func seed(ctx context.Context, r *gitexec.Runner, cwd string, planner SeedPlanner, f fault) (SeedResult, error) {
	result := SeedResult{}
	s, err := open(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return result, err
	}
	if len(pending) > 0 {
		return result, pendingError(pending[0])
	}
	var current Source
	for _, src := range s.sources {
		if src.ID == s.w.WorkspaceID {
			current = src
			break
		}
	}
	old, err := current.state()
	if err != nil {
		return result, err
	}
	if !old.SemanticEqual(selection.Empty()) {
		return result, fmt.Errorf("%w: destination selection is not empty", storage.ErrConflict)
	}
	plan, err := planner(*s.w)
	if err != nil {
		return result, err
	}
	plan.Selection, err = selection.Canonical(plan.Selection)
	if err != nil {
		return result, err
	}
	if !storage.ValidUUID(plan.SourceWorkspace) || plan.SourceWorkspace == s.w.WorkspaceID || !oidValid(plan.SourceSnapshot) {
		return result, errors.New("invalid seed source")
	}
	engine, err := selection.New(r, s.w.Root, s.w.Admin)
	if err != nil {
		return result, err
	}
	defer engine.Close()
	validated, err := engine.Validate(ctx, plan.Selection)
	if err != nil {
		return result, err
	}
	if len(validated.Entries) != 0 {
		return result, fmt.Errorf("%w: destination already has files inside the source selection", storage.ErrConflict)
	}
	if len(plan.Files) == 0 && len(plan.Selection.Include) == 0 {
		return result, errors.New("source selection is empty")
	}
	beforeIgnore, err := readImage(s.common, "info/exclude")
	if err != nil {
		return result, err
	}
	config, err := prepare(s, current, plan.Selection, beforeIgnore, true)
	if err != nil {
		return result, err
	}
	files, err := storage.OpenDir(s.w.Root)
	if err != nil {
		return result, err
	}
	defer files.Close()
	created := map[string]bool{}
	last := ""
	stages := make([]string, 0, len(plan.Files))
	for i := range plan.Files {
		file := &plan.Files[i]
		if runtime.GOOS == "windows" && (file.Mode == "100755" || file.Mode == "120000") {
			return result, fmt.Errorf("unsupported source mode %s for Windows seeding", file.Mode)
		}
		if file.Path <= last || !plan.Selection.Selected(file.Path) || storage.ValidateSelectionPath(file.Path) != nil || !oidValid(file.Blob) || (file.Mode != "100644" && file.Mode != "100755" && file.Mode != "120000") {
			return result, errors.New("invalid or unordered seed file")
		}
		last = file.Path
		if err := engine.CheckRestorePath(file.Path); err != nil {
			return result, err
		}
		if _, err := files.Lstat(file.Path); !os.IsNotExist(err) {
			if err == nil {
				err = fmt.Errorf("%w: destination exists at %q", storage.ErrConflict, file.Path)
			}
			return result, err
		}
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			info, e := files.Lstat(parent)
			if os.IsNotExist(e) {
				created[parent] = true
				continue
			}
			if e != nil {
				return result, e
			}
			if !info.IsDir() {
				return result, fmt.Errorf("%w: unsafe seed parent %q", storage.ErrConflict, parent)
			}
		}
		token, e := storage.UUID()
		if e != nil {
			return result, e
		}
		file.Stage = path.Join(path.Dir(file.Path), ".gitlocal-tmp-"+token)
		if err := engine.CheckRestorePath(file.Stage); err != nil {
			return result, err
		}
		stages = append(stages, file.Stage)
		if _, e = files.Lstat(file.Stage); !os.IsNotExist(e) {
			if e == nil {
				e = errors.New("seed staging path already exists")
			}
			return result, e
		}
	}
	// One pass folds each main-owned path once (see CheckPaths).
	if err := validated.Ownership.CheckPaths(stages); err != nil {
		return result, err
	}
	dirs := []string{}
	for name := range created {
		dirs = append(dirs, name)
	}
	sort.Slice(dirs, func(i, j int) bool {
		a, b := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if a != b {
			return a < b
		}
		return dirs[i] < dirs[j]
	})
	journalVersion := 1
	if plan.KeyFingerprint != "" {
		journalVersion = 2
	}
	j := seedJournal{SchemaVersion: journalVersion, Kind: "seed", ID: config.ID, RepositoryID: s.w.RepositoryID, WorkspaceID: s.w.WorkspaceID, Source: plan.SourceWorkspace, Snapshot: plan.SourceSnapshot, KeyFingerprint: plan.KeyFingerprint, Direction: "apply", Config: config, Files: plan.Files, Directories: dirs, Ownership: validated.Ownership.Evidence}
	if err := validateSeed(j, s.w.RepositoryID, s.w.WorkspaceID); err != nil {
		return result, err
	}
	if _, err := s.seedKey(j); err != nil {
		return result, err
	}
	result.WorkspaceID, result.TransactionID = j.WorkspaceID, j.ID
	encoded, list, err := encodeSeed(j)
	if err != nil {
		return result, err
	}
	if list != nil {
		if err := WriteSeedFiles(s.meta, j.ID, list); err != nil {
			return result, err
		}
		j.FilesDigest = digestOf(list)
	}
	if err := s.meta.AtomicWrite(seedPath(j.ID), encoded, 0600); err != nil {
		return result, err
	}
	if err := point(ctx, f, "seed-journal"); err != nil {
		return result, withRecovery(j.ID, err)
	}
	result.Warnings, err = s.runSeed(ctx, files, &j, f)
	if err != nil {
		return result, withRecovery(j.ID, err)
	}
	return result, nil
}

func validateSeed(j seedJournal, repo, workspaceID string) error {
	if (j.SchemaVersion != 1 && j.SchemaVersion != 2) || j.Kind != "seed" || !storage.ValidUUID(j.ID) || j.RepositoryID != repo || j.WorkspaceID != workspaceID || !storage.ValidUUID(j.Source) || j.Source == workspaceID || !oidValid(j.Snapshot) || (j.Direction != "apply" && j.Direction != "rollback") || (j.Outcome != "" && j.Outcome != "committed" && j.Outcome != "rolledback" && j.Outcome != OutcomeAbandoned) || j.Outcome == "committed" && j.Direction != "apply" || j.Outcome == "rolledback" && j.Direction != "rollback" || j.Config.ID != j.ID || j.Config.WorkspaceID != workspaceID || j.Config.Direction != "apply" || j.Config.Outcome != "" || j.Config.Operation != "track" {
		return errors.New("invalid seed journal")
	}
	if j.SchemaVersion == 1 && j.KeyFingerprint != "" || j.SchemaVersion == 2 && !oidValid(j.KeyFingerprint) {
		return errors.New("invalid seed encryption fingerprint")
	}
	if err := validateJournal(j.Config, repo, j.ID); err != nil {
		return err
	}
	var state selection.State
	if err := decode(j.Config.After.Data, &state); err != nil {
		return err
	}
	var before selection.State
	if err := decode(j.Config.Before.Data, &before); err != nil {
		return err
	}
	// A schema-1 selection from an earlier release has no exclude list.
	before, err := selection.Canonical(before)
	if err != nil {
		return err
	}
	if !before.SemanticEqual(selection.Empty()) {
		return errors.New("seed destination was not empty")
	}
	last := ""
	encryptedFiles := 0
	for _, file := range j.Files {
		if file.Path <= last || last != "" && strings.HasPrefix(file.Path, last+"/") || storage.ValidateSelectionPath(file.Path) != nil || !state.Selected(file.Path) || !oidValid(file.Blob) || (file.Mode != "100644" && file.Mode != "100755" && file.Mode != "120000") || !strings.HasPrefix(path.Base(file.Stage), ".gitlocal-tmp-") || path.Dir(file.Stage) != path.Dir(file.Path) || !storage.ValidUUID(strings.TrimPrefix(path.Base(file.Stage), ".gitlocal-tmp-")) || file.PlainDigest != "" && (j.SchemaVersion != 2 || !validSeedDigest(file.PlainDigest)) {
			return errors.New("invalid seed file journal")
		}
		if file.PlainDigest != "" {
			encryptedFiles++
		}
		last = file.Path
	}
	if j.SchemaVersion == 2 && encryptedFiles == 0 {
		return errors.New("encrypted seed journal has no encrypted files")
	}
	seenStages := map[string]bool{}
	for _, file := range j.Files {
		if seenStages[file.Stage] || file.Stage == file.Path {
			return errors.New("duplicate seed staging path")
		}
		seenStages[file.Stage] = true
	}
	for _, file := range j.Files {
		if seenStages[file.Path] {
			return errors.New("seed stage collides with destination")
		}
	}
	last = ""
	for _, dir := range j.Directories {
		allowed := false
		for _, file := range j.Files {
			if strings.HasPrefix(file.Path, dir+"/") {
				allowed = true
				break
			}
		}
		if storage.ValidateSelectionPath(dir) != nil || !allowed || last != "" && (strings.Count(dir, "/") < strings.Count(last, "/") || strings.Count(dir, "/") == strings.Count(last, "/") && dir <= last) {
			return errors.New("invalid seed directory journal")
		}
		last = dir
	}
	return nil
}

func validSeedDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *session) writeSeed(j seedJournal) error {
	b, list, err := encodeSeed(j)
	if err != nil {
		return err
	}
	if list != nil {
		if err := WriteSeedFiles(s.meta, j.ID, list); err != nil {
			return err
		}
	}
	return s.meta.AtomicWrite(seedPath(j.ID), b, 0600)
}
func (s *session) archiveSeed(j seedJournal) error {
	if err := s.meta.MkdirAll("transactions/completed"); err != nil {
		return err
	}
	if _, err := s.meta.Lstat(seedCompletedPath(j.ID)); err == nil {
		return fmt.Errorf("%w: seed completed journal already exists", storage.ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	return s.meta.Rename(seedPath(j.ID), seedCompletedPath(j.ID))
}

func (s *session) seedKey(j seedJournal) ([]byte, error) {
	policy, exists, err := secureblob.ReadLocked(s.common, s.w.Root, s.w.Common)
	if err != nil {
		return nil, err
	}
	for _, file := range j.Files {
		covered := exists && policy.Covers(file.Path)
		if covered != (file.PlainDigest != "") {
			return nil, fmt.Errorf("%w: sensitive-file policy and seed journal disagree for %q", storage.ErrConflict, file.Path)
		}
	}
	if j.SchemaVersion == 1 {
		return nil, nil
	}
	if !exists || policy.KeyFingerprint != j.KeyFingerprint {
		return nil, fmt.Errorf("%w: missing matching sensitive-file policy for seed", storage.ErrConflict)
	}
	return secureblob.LoadKey(policy, s.w.Root, s.w.Common)
}

func fileMatches(files *storage.Dir, file SeedFile, name string, key []byte, identity secureblob.Identity) (bool, bool, error) {
	info, err := files.Lstat(name)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var data io.Reader
	if file.Mode == "120000" {
		if info.Mode()&os.ModeSymlink == 0 {
			return true, false, nil
		}
		target, e := files.ReadLink(name)
		if e != nil {
			return true, false, e
		}
		data = strings.NewReader(target)
	} else {
		if !info.Mode().IsRegular() || file.Mode == "100755" && info.Mode().Perm()&0111 == 0 || file.Mode == "100644" && info.Mode().Perm()&0111 != 0 {
			return true, false, nil
		}
	}
	if file.PlainDigest != "" {
		var digest string
		if data != nil {
			digest, err = secureblob.Digest(data, key, identity)
		} else {
			_, err = files.StreamStable(name, func(input io.Reader) error {
				var e error
				digest, e = secureblob.Digest(input, key, identity)
				return e
			})
		}
		return true, digest == file.PlainDigest, err
	}
	var h hash.Hash = sha1.New()
	if len(file.Blob) == 64 {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d%c", info.Size(), 0)
	if data != nil {
		_, err = io.Copy(h, data)
	} else {
		_, err = files.StreamStable(name, func(input io.Reader) error { _, e := io.Copy(h, input); return e })
	}
	if err != nil {
		return true, false, err
	}
	return true, hex.EncodeToString(h.Sum(nil)) == file.Blob, nil
}

// seedStageWriter adds fault points around each streamed stage write.
type seedStageWriter struct {
	ctx   context.Context
	out   io.Writer
	f     fault
	index int
}

func (w seedStageWriter) Write(b []byte) (int, error) {
	if err := point(w.ctx, w.f, fmt.Sprintf("before-seed-stage-write:%d", w.index)); err != nil {
		return 0, err
	}
	n, err := w.out.Write(b)
	if err != nil {
		return n, err
	}
	return n, point(w.ctx, w.f, fmt.Sprintf("seed-stage-write:%d", w.index))
}

type limitedSeedBuffer struct{ bytes.Buffer }

func (b *limitedSeedBuffer) Write(data []byte) (int, error) {
	if len(data) > 1<<20-b.Len() {
		return 0, errors.New("encrypted seed symlink target exceeds 1 MiB")
	}
	return b.Buffer.Write(data)
}

func (s *session) runSeed(ctx context.Context, files *storage.Dir, j *seedJournal, f fault) ([]string, error) {
	if err := s.recognize(j.Config); err != nil {
		return nil, err
	}
	if err := (selection.Ownership{Evidence: j.Ownership}).Revalidate(ctx, s.r, s.w.Root, s.w.Admin); err != nil {
		return nil, err
	}
	if j.Outcome != "" {
		return nil, s.archiveSeed(*j)
	}
	key, err := s.seedKey(*j)
	if err != nil {
		return nil, err
	}
	if j.Direction == "rollback" {
		if err := s.writeSeed(*j); err != nil {
			return nil, err
		}
		return nil, s.rollbackSeed(ctx, files, j, key)
	}
	if err := s.install(ctx, j.Config, j.Config.ExcludeUnion, false, f, "seed-union"); err != nil {
		return nil, err
	}
	var next selection.State
	if err := decode(j.Config.After.Data, &next); err != nil {
		return nil, err
	}
	warnings, err := s.verify(ctx, j.WorkspaceID, next, false)
	if err != nil {
		return warnings, err
	}
	if err := s.install(ctx, j.Config, j.Config.After, true, f, "seed-selection"); err != nil {
		return warnings, err
	}
	for _, dir := range j.Directories {
		info, e := files.Lstat(dir)
		if os.IsNotExist(e) {
			e = files.MakeDirectory(dir)
		} else if e == nil && !info.IsDir() {
			e = fmt.Errorf("%w: seed directory obstruction %q", storage.ErrConflict, dir)
		}
		if e != nil {
			return warnings, e
		}
	}
	for index, file := range j.Files {
		identity := secureblob.Identity{RepositoryID: s.w.RepositoryID, WorkspaceID: j.Source, Path: file.Path, Mode: file.Mode}
		if err := ctx.Err(); err != nil {
			return warnings, err
		}
		if err := s.recognize(j.Config); err != nil {
			return warnings, err
		}
		if err := (selection.Ownership{Evidence: j.Ownership}).Revalidate(ctx, s.r, s.w.Root, s.w.Admin); err != nil {
			return warnings, err
		}
		exists, match, e := fileMatches(files, file, file.Path, key, identity)
		if e != nil {
			return warnings, e
		}
		if exists {
			if !match {
				return warnings, fmt.Errorf("%w: seed destination changed at %q", storage.ErrConflict, file.Path)
			}
			continue
		}
		stageExists, stageMatch, e := fileMatches(files, file, file.Stage, key, identity)
		if e != nil {
			return warnings, e
		}
		if stageExists && !stageMatch {
			// The journal reserved this random name and StageNamed creates it
			// exclusively, so other bytes there are an interrupted write.
			if err := files.Remove(file.Stage); err != nil {
				return warnings, err
			}
			stageExists = false
		}
		if !stageExists {
			if file.Mode == "120000" {
				var target string
				if file.PlainDigest != "" {
					var plain limitedSeedBuffer
					if e := secureblob.DecryptGitBlob(ctx, s.r, s.w.Store, file.Blob, &plain, key, identity); e != nil {
						return warnings, e
					}
					target = plain.String()
				} else {
					b, e := s.r.History(ctx, s.w.Store, nil, "cat-file", "blob", file.Blob)
					if e != nil {
						return warnings, e
					}
					target = string(b)
				}
				e = files.StageNamedSymlink(file.Stage, target)
				if e != nil {
					return warnings, e
				}
			} else {
				mode := os.FileMode(0644)
				if file.Mode == "100755" {
					mode = 0755
				}
				e = files.StageNamed(file.Stage, mode, func(out io.Writer) error {
					writer := seedStageWriter{ctx: ctx, out: out, f: f, index: index}
					if file.PlainDigest != "" {
						return secureblob.DecryptGitBlob(ctx, s.r, s.w.Store, file.Blob, writer, key, identity)
					}
					return s.r.HistoryBlob(ctx, s.w.Store, file.Blob, writer)
				})
				if e != nil {
					return warnings, e
				}
			}
		}
		if file.Mode != "120000" {
			mode := os.FileMode(0644)
			if file.Mode == "100755" {
				mode = 0755
			}
			if err := files.ChmodStaged(file.Stage, mode); err != nil {
				return warnings, err
			}
		}
		if err := files.SyncStaged(file.Stage); err != nil {
			return warnings, err
		}
		if _, match, e = fileMatches(files, file, file.Stage, key, identity); e != nil || !match {
			if e == nil {
				e = fmt.Errorf("%w: seed staging file differs", storage.ErrConflict)
			}
			return warnings, e
		}
		if exists, _, e = fileMatches(files, file, file.Path, key, identity); e != nil || exists {
			if e == nil {
				e = fmt.Errorf("%w: seed destination appeared", storage.ErrConflict)
			}
			return warnings, e
		}
		if err := point(ctx, f, fmt.Sprintf("seed-publish-%d", index)); err != nil {
			return warnings, err
		}
		// A file created since the check above is never replaced.
		if err := files.RenameNoReplace(file.Stage, file.Path); errors.Is(err, fs.ErrExist) {
			return warnings, fmt.Errorf("%w: seed destination %s appeared", storage.ErrConflict, file.Path)
		} else if err != nil {
			return warnings, err
		}
		if err := point(ctx, f, fmt.Sprintf("seed-file-%d", index)); err != nil {
			return warnings, err
		}
	}
	if err := s.install(ctx, j.Config, j.Config.ExcludeAfter, false, f, "seed-final-exclude"); err != nil {
		return warnings, err
	}
	more, err := s.verify(ctx, j.WorkspaceID, next, false)
	warnings = append(warnings, more...)
	if err != nil {
		return warnings, err
	}
	j.Outcome = "committed"
	if err := s.writeSeed(*j); err != nil {
		return warnings, err
	}
	return uniqueWarnings(warnings), s.archiveSeed(*j)
}

func (s *session) rollbackSeed(ctx context.Context, files *storage.Dir, j *seedJournal, key []byte) error {
	if err := s.install(ctx, j.Config, j.Config.ExcludeUnion, false, nil, "seed-rollback-union"); err != nil {
		return err
	}
	for i := len(j.Files) - 1; i >= 0; i-- {
		file := j.Files[i]
		identity := secureblob.Identity{RepositoryID: s.w.RepositoryID, WorkspaceID: j.Source, Path: file.Path, Mode: file.Mode}
		exists, match, err := fileMatches(files, file, file.Path, key, identity)
		if err != nil {
			return err
		}
		_, stageErr := files.Lstat(file.Stage)
		if stageErr != nil && !os.IsNotExist(stageErr) {
			return stageErr
		}
		// While the stage exists the seed never published this file, so a
		// different file at its path was created by someone else and stays.
		if exists && !match && stageErr != nil {
			return fmt.Errorf("%w: seed file changed at %q", storage.ErrConflict, file.Path)
		}
		if exists && match {
			if err := files.Remove(file.Path); err != nil {
				return err
			}
		}
		// The stage belongs to the journal; partial or complete, it goes.
		if stageErr == nil {
			if err := files.Remove(file.Stage); err != nil {
				return err
			}
		}
	}
	for i := len(j.Directories) - 1; i >= 0; i-- {
		name := j.Directories[i]
		if _, err := files.Lstat(name); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		// A directory the seed created stays when it now holds files the
		// seed didn't write.
		if err := files.RemoveDirectory(name); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if err := s.install(ctx, j.Config, j.Config.Before, true, nil, "seed-rollback-selection"); err != nil {
		return err
	}
	if err := s.install(ctx, j.Config, j.Config.ExcludeBefore, false, nil, "seed-rollback-exclude"); err != nil {
		return err
	}
	j.Outcome = "rolledback"
	if err := s.writeSeed(*j); err != nil {
		return err
	}
	return s.archiveSeed(*j)
}

type SeedRecoveryValidator func(workspace.Context, string, string, selection.State, []SeedFile) error

func RecoverSeed(ctx context.Context, r *gitexec.Runner, cwd, id string, rollback bool, validators ...SeedRecoveryValidator) (SeedResult, error) {
	result := SeedResult{}
	if !storage.ValidUUID(id) {
		return result, errors.New("invalid seed transaction ID")
	}
	s, err := open(ctx, r, cwd)
	if err != nil {
		return result, err
	}
	defer s.close()
	pending, err := s.pending()
	if err != nil {
		return result, err
	}
	for _, other := range pending {
		if other != id {
			return result, pendingError(other)
		}
	}
	j, err := readSeed(s.meta, seedPath(id))
	completed := false
	if os.IsNotExist(err) {
		j, err = readSeed(s.meta, seedCompletedPath(id))
		completed = true
	}
	if err != nil {
		return result, err
	}
	if j.ID != id {
		return result, errors.New("seed journal ID mismatch")
	}
	if j.WorkspaceID != s.w.WorkspaceID {
		return result, fmt.Errorf("%w: recover seed transaction %s from its destination workspace %s", storage.ErrConflict, id, j.WorkspaceID)
	}
	if err := validateSeed(j, s.w.RepositoryID, s.w.WorkspaceID); err != nil {
		return result, err
	}
	result = SeedResult{WorkspaceID: j.WorkspaceID, TransactionID: id}
	if completed {
		if rollback && j.Outcome != "rolledback" || !rollback && j.Outcome != "committed" {
			return result, errors.New("seed transaction completed with a different outcome")
		}
		return result, nil
	}
	if len(validators) > 1 {
		return result, errors.New("multiple seed validators")
	}
	if len(validators) == 1 && validators[0] != nil {
		var next selection.State
		if err := decode(j.Config.After.Data, &next); err != nil {
			return result, err
		}
		if err := validators[0](*s.w, j.Source, j.Snapshot, next, j.Files); err != nil {
			return result, err
		}
	}
	if j.Direction == "rollback" && !rollback {
		return result, errors.New("seed rollback has started; use --rollback")
	}
	if rollback && j.Outcome == "committed" {
		return result, errors.New("seed transaction already committed")
	}
	if rollback {
		// runSeed records the direction only after its recognition,
		// ownership and key checks pass, so a refused rollback leaves
		// resume available.
		j.Direction = "rollback"
	}
	files, err := storage.OpenDir(s.w.Root)
	if err != nil {
		return result, err
	}
	defer files.Close()
	result.Warnings, err = s.runSeed(ctx, files, &j, nil)
	if err != nil {
		return result, withRecovery(id, err)
	}
	return result, nil
}
