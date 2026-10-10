// Package storage provides anchored filesystem operations and durable metadata.
// Concurrent hostile modification of Git administrative directories is outside
// the contract; all extension writers must hold the common lock.
package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var ErrConflict = errors.New("conflict or recovery required")

func UUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}
func ValidUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validRelative(path string) error {
	if path == "" || !filepath.IsLocal(path) || strings.ContainsRune(path, 0) {
		return errors.New("path must be a nonempty relative path")
	}
	for _, c := range strings.Split(filepath.ToSlash(path), "/") {
		if c == "" || c == "." || c == ".." {
			return errors.New("noncanonical relative path")
		}
	}
	return nil
}

// shortName matches a Windows 8.3 short name: a tilde and digits ending the
// stem, with at most one extension.
var shortName = regexp.MustCompile(`~[0-9]+(\.[^.]*)?$`)

// ValidateSelectionPath deliberately applies portable name restrictions on every
// platform. Rejecting ambiguous aliases is safer than silently normalizing names.
func ValidateSelectionPath(path string) error {
	if err := validRelative(path); err != nil {
		return err
	}
	if !utf8.ValidString(path) || strings.ContainsAny(path, "\r\n\\") {
		return errors.New("unsupported path encoding or separator")
	}
	for _, c := range strings.Split(path, "/") {
		folded := AliasKey(c)
		if folded == ".git" || strings.HasPrefix(folded, "git~") || strings.TrimRight(folded, ". ") != folded || runtime.GOOS == "windows" && strings.ContainsAny(c, ":<>\"|?*") {
			return fmt.Errorf("unsupported path component %q", c)
		}
		// On Windows, an NTFS 8.3 short name such as PROGRA~1 or NAME~1.EXT
		// can resolve to a different long name, which alias checks can't see.
		// Elsewhere such names (backup~2.txt) are ordinary files and stay valid.
		if runtime.GOOS == "windows" && shortName.MatchString(c) {
			return fmt.Errorf("unsupported path component %q: it has the form of a Windows 8.3 short name", c)
		}
		stem := strings.SplitN(strings.ToUpper(c), ".", 2)[0]
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return fmt.Errorf("reserved path component %q", c)
		}
	}
	return nil
}

// PortableAlias maps a path that fails ValidateSelectionPath to the spelling a
// Windows filesystem would resolve it to: backslashes separate components,
// repeated separators collapse, an NTFS stream suffix (name::$DATA,
// name:stream) names the file itself, and trailing dots and spaces are
// dropped. Such a path can never be selected itself, so guards compare this
// alias with AliasKey instead of rejecting it.
func PortableAlias(path string) string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if i := strings.IndexByte(part, ':'); i >= 0 {
			part = part[:i]
		}
		if part = strings.TrimRight(part, ". "); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

// AliasKey is conservative across case-sensitive and normalization-sensitive
// filesystems. Callers reject collisions; they never change the stored name.
func AliasKey(path string) string {
	return cases.Fold().String(norm.NFKC.String(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, path)))
}

func (d *Dir) ReadFile(name string, limit int64) ([]byte, error) {
	b, _, err := d.readStable(name, limit)
	return b, err
}
func (d *Dir) readStable(name string, limit int64) ([]byte, fs.FileInfo, error) {
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if err = ValidateInfo(before); err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("expected regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(b)) > limit {
		return nil, nil, errors.New("file exceeds size limit")
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return nil, nil, fmt.Errorf("%w: file changed while reading", ErrConflict)
	}
	current, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: file changed while reading", ErrConflict)
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(before, currentInfo) || before.Size() != currentInfo.Size() || before.ModTime() != currentInfo.ModTime() || before.Mode() != currentInfo.Mode() {
		return nil, nil, fmt.Errorf("%w: file changed while reading", ErrConflict)
	}
	return b, before, nil
}
func (d *Dir) ReadJSON(name string, v any) error {
	b, err := d.ReadFile(name, 1<<20)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return fmt.Errorf("invalid metadata %s: %w", name, err)
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("invalid trailing metadata in %s", name)
	}
	return nil
}
func (d *Dir) WriteJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return d.AtomicWrite(name, append(b, '\n'), 0600)
}
func (d *Dir) AtomicWrite(name string, data []byte, mode fs.FileMode) error {
	if err := validRelative(name); err != nil {
		return err
	}
	if fi, err := d.Lstat(name); err == nil {
		if !fi.Mode().IsRegular() {
			return errors.New("replacement target is not a regular file")
		}
		if err = d.CheckFile(name); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	id, err := UUID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), ".gitlocal-tmp-"+id)
	f, err := d.open(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer d.Remove(tmp)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		// Creation permissions are filtered by umask. Replacement must retain
		// the requested mode, including preserved user configuration permissions.
		writeErr = f.Chmod(mode)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = d.Rename(tmp, name); err != nil {
		return err
	}
	return nil
}
func (d *Dir) MkdirAll(name string) error {
	if err := validRelative(name); err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		if err := d.mkdir(p); err != nil && !os.IsExist(err) {
			return err
		}
		fi, err := d.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return errors.New("directory path is obstructed")
		}
		if err = ValidateInfo(fi); err != nil {
			return err
		}
	}
	return nil
}

type Fingerprint struct {
	Exists bool   `json:"exists"`
	Digest string `json:"digest,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

func (d *Dir) Fingerprint(name string) (Fingerprint, error) {
	b, fi, err := d.readStable(name, 64<<20)
	if os.IsNotExist(err) {
		return Fingerprint{}, nil
	}
	if err != nil {
		return Fingerprint{}, err
	}
	sum := sha256.Sum256(b)
	return Fingerprint{true, hex.EncodeToString(sum[:]), uint32(fi.Mode()), int64(len(b))}, nil
}

type Lock struct{ file *os.File }

func (d *Dir) Lock(ctx context.Context, name string, shared bool) (*Lock, error) {
	// Elect a single creator, then open the existing inode without O_CREATE.
	// Concurrent nonexclusive creation can fail on supported local filesystems.
	f, err := d.open(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if os.IsExist(err) {
		f, err = d.open(name, os.O_RDWR, 0600)
	}
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, f, shared)
}

// LockExisting acquires a shared read lock without creating or rewriting state.
func (d *Dir) LockExisting(ctx context.Context, name string) (*Lock, error) {
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, f, true)
}
func lockFile(ctx context.Context, f *os.File, shared bool) (*Lock, error) {
	fi, err := f.Stat()
	if err == nil {
		err = ValidateInfo(fi)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		ok, err := tryLock(f, shared)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return &Lock{f}, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func (l *Lock) Close() error { return l.file.Close() }

// CheckFile validates a regular file through a no-follow handle without reading
// contents; on Windows this also checks the hard-link count on that handle.
func (d *Dir) CheckFile(name string) error {
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("expected regular file")
	}
	return ValidateInfo(fi)
}

// Interrupted atomic writes may leave private sibling files. Ignore these exact
// generated names during replay, preserving their bytes rather than guessing
// whether it is safe to delete them.
func IsTempName(name string) bool {
	const prefix = ".gitlocal-tmp-"
	return strings.HasPrefix(name, prefix) && ValidUUID(strings.TrimPrefix(name, prefix))
}
