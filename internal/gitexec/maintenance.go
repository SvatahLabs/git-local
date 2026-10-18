package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// StoreStats is a private store's object count and size, from count-objects.
type StoreStats struct {
	LooseObjects  int64 `json:"looseObjects"`
	LooseBytes    int64 `json:"looseBytes"`
	PackedObjects int64 `json:"packedObjects"`
	Packs         int64 `json:"packs"`
	PackBytes     int64 `json:"packBytes"`
	GarbageBytes  int64 `json:"garbageBytes"`
}

// Bytes is the store's total object size on disk.
func (s StoreStats) Bytes() int64 { return s.LooseBytes + s.PackBytes + s.GarbageBytes }

// StoreStats counts a private store's loose and packed objects.
func (r *Runner) StoreStats(ctx context.Context, store string) (StoreStats, error) {
	if err := r.CheckCapabilities(ctx); err != nil {
		return StoreStats{}, err
	}
	if err := r.checkStore(store); err != nil {
		return StoreStats{}, err
	}
	out, err := r.run(ctx, store, true, nil, []string{"--git-dir=" + store}, nil, "count-objects", "-v")
	if err != nil {
		return StoreStats{}, err
	}
	var stats StoreStats
	fields := map[string]*int64{"count": &stats.LooseObjects, "size": &stats.LooseBytes, "in-pack": &stats.PackedObjects, "packs": &stats.Packs, "size-pack": &stats.PackBytes, "size-garbage": &stats.GarbageBytes}
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		target := fields[key]
		if !ok || target == nil {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return StoreStats{}, fmt.Errorf("unexpected count-objects value %q", line)
		}
		// count-objects reports sizes in KiB.
		if strings.HasPrefix(key, "size") {
			n *= 1024
		}
		*target = n
		seen++
	}
	if seen != len(fields) {
		return StoreStats{}, errors.New("incomplete count-objects output")
	}
	return stats, nil
}

// MaintainStore packs every object reachable from the store's refs into one
// pack and deletes the rest. The caller must hold the repository's exclusive
// lock, so no git-local process writes or reads the store meanwhile, and must
// have checked that no transaction is pending: unreachable objects are deleted
// at once, not after Git's usual grace period. CheckStore then verifies the
// result; it only reads, so the caller can run it under the shared lock.
func (r *Runner) MaintainStore(ctx context.Context, store string) error {
	if err := r.CheckCapabilities(ctx); err != nil {
		return err
	}
	if err := r.checkStore(store); err != nil {
		return err
	}
	// The store's own config makes loose objects and refs durable; a repack
	// must also sync its pack before deleting what it replaces.
	prefix := []string{"--git-dir=" + store, "-c", "core.fsync=loose-object,reference,pack,pack-metadata", "-c", "repack.writeBitmaps=false"}
	if _, err := r.run(ctx, store, true, nil, prefix, nil, "repack", "-a", "-d", "-q", "-n"); err != nil {
		return err
	}
	_, err := r.run(ctx, store, true, nil, prefix, nil, "prune", "--expire=now")
	return err
}

// CheckStore validates a private store's layout and every object in it.
func (r *Runner) CheckStore(ctx context.Context, store string) error {
	if err := r.CheckCapabilities(ctx); err != nil {
		return err
	}
	if err := r.checkStore(store); err != nil {
		return err
	}
	if err := ValidateStore(store); err != nil {
		return err
	}
	_, err := r.run(ctx, store, true, nil, []string{"--git-dir=" + store}, nil, "fsck", "--full", "--strict", "--no-reflogs", "--no-progress", "--no-dangling")
	return err
}

// LooseObjects counts a store's loose objects and their bytes by listing its
// object directories, without starting Git.
func LooseObjects(store string) (count, size int64, err error) {
	objects := filepath.Join(store, "objects")
	dirs, err := os.ReadDir(objects)
	if err != nil {
		return 0, 0, err
	}
	for _, dir := range dirs {
		if len(dir.Name()) != 2 || !isHex(dir.Name()) || !dir.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(objects, dir.Name()))
		if err != nil {
			return 0, 0, err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !isHex(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return 0, 0, err
			}
			count++
			size += info.Size()
		}
	}
	return count, size, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

// PackLoose packs the loose objects the store's refs reach into one new pack,
// leaving existing packs alone, and deletes the loose objects that remain.
// MaintainStore's rules for the caller apply.
func (r *Runner) PackLoose(ctx context.Context, store string) error {
	if err := r.CheckCapabilities(ctx); err != nil {
		return err
	}
	if err := r.checkStore(store); err != nil {
		return err
	}
	prefix := []string{"--git-dir=" + store, "-c", "core.fsync=loose-object,reference,pack,pack-metadata", "-c", "repack.writeBitmaps=false"}
	if _, err := r.run(ctx, store, true, nil, prefix, nil, "repack", "-d", "-q", "-n"); err != nil {
		return err
	}
	_, err := r.run(ctx, store, true, nil, prefix, nil, "prune", "--expire=now")
	return err
}
