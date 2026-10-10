// Package gitexec isolates Git subprocesses. All arguments come from domain code,
// never from a pass-through command line. Main reads and private history writes
// deliberately have different configuration policies.
package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MaxOutput = 16 << 20

type Runner struct {
	processes  atomic.Uint64
	executable string
	env        []string
	discovery  []string
	private    string
	mu         sync.Mutex
	checked    bool
	// backupConfig holds extra -c settings for backup transports; only
	// tests set it (to trust a test server's certificate).
	backupConfig []string
}

type Error struct {
	Command       string
	Code          int
	DiscoveryMiss bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s failed (exit %d); check repository ownership, configuration and required local objects", e.Command, e.Code)
}

// New rejects inherited contexts that could hide ownership before any Git runs.
func New(env []string) (*Runner, error) {
	blocked := map[string]bool{"GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true, "GIT_CONFIG_COUNT": true, "GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG": true, "GIT_CONFIG_SYSTEM": true, "GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_NOSYSTEM": true}
	r := &Runner{}
	var pathEnv string
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(k)
		if blocked[upper] && v != "" {
			return nil, fmt.Errorf("unsupported inherited %s; run outside alternate-index/configuration contexts", upper)
		}
		if upper == "GIT_DIR" || upper == "GIT_WORK_TREE" {
			r.discovery = append(r.discovery, entry)
		}
		if !strings.HasPrefix(upper, "GIT_") && upper != "LC_ALL" && upper != "LANG" {
			r.env = append(r.env, entry)
		}
		if upper == "PATH" {
			pathEnv = v
		}
	}
	// Resolve against explicit absolute PATH entries, never the working directory.
	name := "git"
	if filepath.Separator == '\\' {
		name = "git.exe"
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err == nil && fi.Mode().IsRegular() && (filepath.Separator == '\\' || fi.Mode()&0111 != 0) {
			r.executable, err = filepath.EvalSymlinks(p)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if r.executable == "" {
		return nil, errors.New("Git must be installed on an absolute PATH entry")
	}
	var err error
	r.private, err = os.MkdirTemp("", "gitlocal-runner-")
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"hooks", "template"} {
		if err = os.Mkdir(filepath.Join(r.private, d), 0700); err != nil {
			r.Close()
			return nil, err
		}
	}
	r.env = append(r.env, "LC_ALL=C", "LANG=C", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_LITERAL_PATHSPECS=1", "GIT_ATTR_NOSYSTEM=1")
	return r, nil
}
func (r *Runner) Close() error       { return os.RemoveAll(r.private) }
func (r *Runner) Executable() string { return r.executable }

// Guard runs a restricted read-only Git query without the expensive capability
// probe. Hooks execute on every commit and must inspect the live index.
func (r *Runner) Guard(ctx context.Context, cwd, index string, args ...string) ([]byte, error) {
	if len(args) == 0 || !map[string]bool{"rev-parse": true, "diff": true, "rev-list": true, "diff-tree": true, "for-each-ref": true, "cat-file": true}[args[0]] {
		return nil, errors.New("command not allowed by guard policy")
	}
	var extra []string
	if index != "" {
		if !filepath.IsAbs(index) {
			index = filepath.Join(cwd, index)
		}
		extra = []string{"GIT_INDEX_FILE=" + index}
	}
	return r.run(ctx, cwd, false, extra, nil, nil, args...)
}

// IndexBlob reads a stage-zero blob without filters or text conversion. A
// filter process may pass Git's temporary GIT_INDEX_FILE here.
func (r *Runner) IndexBlob(ctx context.Context, cwd, index, path string) ([]byte, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n\\") || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return nil, errors.New("invalid index path")
	}
	var extra []string
	if index != "" {
		if !filepath.IsAbs(index) {
			index = filepath.Join(cwd, index)
		}
		extra = []string{"GIT_INDEX_FILE=" + index}
	}
	// ":0:" names stage zero explicitly: ":"+path would read a path such as
	// 0:foo as stage 0 of foo.
	return r.run(ctx, cwd, false, extra, nil, nil, "cat-file", "blob", ":0:"+path)
}

func (r *Runner) IndexMode(ctx context.Context, cwd, index, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n\\") || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return "", errors.New("invalid index path")
	}
	var extra []string
	if index != "" {
		if !filepath.IsAbs(index) {
			index = filepath.Join(cwd, index)
		}
		extra = []string{"GIT_INDEX_FILE=" + index}
	}
	b, err := r.run(ctx, cwd, false, extra, nil, nil, "ls-files", "--stage", "-z", "--", path)
	if err != nil {
		return "", err
	}
	parts := bytes.Split(b, []byte{0})
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) != 0 {
		return "", errors.New("path is not one stage-zero index entry")
	}
	fields := bytes.SplitN(parts[0], []byte{'\t'}, 2)
	if len(fields) != 2 || string(fields[1]) != path {
		return "", errors.New("index path differs from requested path")
	}
	meta := strings.Fields(string(fields[0]))
	if len(meta) != 3 || meta[2] != "0" {
		return "", errors.New("path has unmerged index stages")
	}
	return meta[0], nil
}

func (r *Runner) CheckAttributes(ctx context.Context, cwd, path string) (map[string]string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n\\") || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return nil, errors.New("invalid attribute path")
	}
	// System attributes count too: Git for Windows ships some, and they
	// convert line endings as much as the repository's own.
	b, err := r.run(ctx, cwd, false, []string{"GIT_ATTR_NOSYSTEM=0"}, nil, nil, "check-attr", "-z", "filter", "text", "eol", "ident", "--", path)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(b, []byte{0})
	if len(parts) == 0 || len(parts[len(parts)-1]) != 0 || (len(parts)-1)%3 != 0 {
		return nil, errors.New("malformed Git attributes response")
	}
	result := map[string]string{}
	for i := 0; i < len(parts)-1; i += 3 {
		if string(parts[i]) != path {
			return nil, errors.New("attribute path differs from requested path")
		}
		result[string(parts[i+1])] = string(parts[i+2])
	}
	return result, nil
}

// HooksPath reads Git's effective hook directory without the runner's private
// hooks override. It is used only for explicit hook installation.
func (r *Runner) HooksPath(ctx context.Context, cwd string) (string, error) {
	cmd := exec.CommandContext(ctx, r.executable, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	cmd.Dir = cwd
	cmd.Env = append([]string{}, r.env...)
	cmd.WaitDelay = 2 * time.Second
	r.processes.Add(1)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve effective Git hooks path: %w", err)
	}
	if len(out) > 4096 {
		return "", errors.New("Git hooks path exceeds safety limit")
	}
	p := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") {
		return "", errors.New("invalid Git hooks path")
	}
	return filepath.Clean(p), nil
}

// SubprocessCount counts attempted launches, including capability probes.
func (r *Runner) SubprocessCount() uint64 { return r.processes.Load() }

func (r *Runner) controls() []string {
	args := []string{"--no-pager", "--no-optional-locks"}
	for _, c := range []string{"core.fsmonitor=false", "core.hooksPath=" + filepath.Join(r.private, "hooks"), "protocol.allow=never", "protocol.file.allow=never", "protocol.ext.allow=never", "protocol.http.allow=never", "protocol.https.allow=never", "protocol.ssh.allow=never", "protocol.git.allow=never", "maintenance.auto=false", "gc.auto=0", "commit.gpgSign=false", "tag.gpgSign=false", "core.askPass=", "credential.helper=", "diff.external=", "core.quotePath=true"} {
		args = append(args, "-c", c)
	}
	return args
}

type boundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := MaxOutput - b.Len()
	if room > 0 {
		b.Buffer.Write(p[:min(room, n)])
	}
	if n > room {
		b.overflow = true
	}
	return n, nil
}
func (r *Runner) run(ctx context.Context, cwd string, private bool, extraEnv []string, prefix []string, input io.Reader, args ...string) ([]byte, error) {
	return r.runOutput(ctx, cwd, private, extraEnv, prefix, input, nil, args...)
}
func (r *Runner) runOutput(ctx context.Context, cwd string, private bool, extraEnv []string, prefix []string, input io.Reader, output io.Writer, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	argv := append(r.controls(), prefix...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, r.executable, argv...)
	cmd.Dir = cwd
	cmd.Stdin = input
	cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	if private {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	}
	cmd.WaitDelay = 2 * time.Second
	var out, diagnostic boundedBuffer
	cmd.Stdout = &out
	if output != nil {
		cmd.Stdout = output
	}
	cmd.Stderr = &diagnostic
	r.processes.Add(1)
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if strings.Contains(diagnostic.String(), "unknown core.fsync") || strings.Contains(diagnostic.String(), "unknown fsync") || strings.Contains(diagnostic.String(), "fsync is not supported") {
		return nil, errors.New("Git does not support required durability controls")
	}
	if out.overflow || diagnostic.overflow {
		return nil, errors.New("Git output exceeded safety limit")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.Bytes(), &Error{Command: args[0], Code: exit.ExitCode(), DiscoveryMiss: strings.Contains(diagnostic.String(), "not a git repository") || strings.Contains(diagnostic.String(), "must be run in a work tree")}
		}
		return nil, fmt.Errorf("start Git: %w", err)
	}
	return out.Bytes(), nil
}

func (r *Runner) Discover(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "rev-parse" {
		return nil, errors.New("discovery permits only rev-parse")
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	return r.run(ctx, cwd, false, r.discovery, nil, nil, args...)
}

var mainCommands = map[string]bool{"rev-parse": true, "ls-files": true, "ls-tree": true, "check-ignore": true, "worktree": true, "symbolic-ref": true, "show-ref": true, "config": true, "cat-file": true, "check-ref-format": true, "remote": true}

func (r *Runner) Main(ctx context.Context, cwd, admin string, args ...string) ([]byte, error) {
	return r.mainInput(ctx, cwd, admin, nil, args...)
}

// RemoveLinkedWorktree is a narrowly scoped main-repository mutation. The
// caller must have identified an available linked worktree and saved its local
// files before invoking it.
func (r *Runner) RemoveLinkedWorktree(ctx context.Context, cwd, admin, target string, force bool) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || strings.ContainsAny(target, "\x00\r\n") {
		return errors.New("invalid linked worktree path")
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", target)
	_, err := r.run(ctx, cwd, false, nil, []string{"--git-dir=" + admin, "--work-tree=" + cwd}, nil, args...)
	return err
}

// ResetHard changes the main worktree only after the caller has saved its
// selected files. Accepting a resolved object ID prevents option or revision
// injection through Git's reset command line.
func (r *Runner) ResetHard(ctx context.Context, cwd, admin, commit string) error {
	if !validResetCommit(commit) {
		return errors.New("invalid reset commit ID")
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return err
	}
	_, err := r.run(ctx, cwd, false, nil, []string{"--git-dir=" + admin, "--work-tree=" + cwd}, nil, "reset", "--hard", commit)
	return err
}

func validResetCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, ch := range commit {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// ResetFilters reads the filter attribute from the exact target tree, including
// Git's info and global attributes. It does not execute configured filters.
func (r *Runner) ResetFilters(ctx context.Context, cwd, admin, commit string, paths []string) (map[string]string, error) {
	if !validResetCommit(commit) {
		return nil, errors.New("invalid reset commit ID")
	}
	result := make(map[string]string)
	if len(paths) == 0 {
		return result, nil
	}
	var input bytes.Buffer
	for _, path := range paths {
		if path == "" || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("invalid reset tree path")
		}
		input.WriteString(path)
		input.WriteByte(0)
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	output, err := r.run(ctx, cwd, false, nil, []string{"--git-dir=" + admin, "--work-tree=" + cwd}, &input, "check-attr", "--source="+commit, "-z", "--stdin", "filter")
	if err != nil {
		return nil, err
	}
	if len(output) == 0 || output[len(output)-1] != 0 {
		return nil, errors.New("malformed reset attribute response")
	}
	parts := bytes.Split(output[:len(output)-1], []byte{0})
	if len(parts) != len(paths)*3 {
		return nil, errors.New("incomplete reset attribute response")
	}
	for i, path := range paths {
		if string(parts[3*i]) != path || string(parts[3*i+1]) != "filter" {
			return nil, errors.New("reset attribute path differs from target tree")
		}
		value := string(parts[3*i+2])
		if value != "unspecified" && value != "unset" && value != "" {
			result[path] = value
		}
	}
	return result, nil
}

func (r *Runner) mainInput(ctx context.Context, cwd, admin string, input io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || !mainCommands[args[0]] {
		return nil, errors.New("command not allowed by main-read policy")
	}
	for _, arg := range args[1:] {
		if arg == "--filters" || arg == "--textconv" || arg == "--ext-diff" {
			return nil, errors.New("content conversion is forbidden")
		}
	}
	if args[0] == "show-ref" && (len(args) != 4 || args[1] != "--verify" || args[2] != "--quiet" || !strings.HasPrefix(args[3], "refs/")) {
		return nil, errors.New("only explicit ref existence inspection is allowed")
	}
	if args[0] == "symbolic-ref" && (len(args) != 3 || args[1] != "--quiet" || args[2] != "HEAD") {
		return nil, errors.New("only symbolic HEAD inspection is allowed")
	}
	if args[0] == "check-ref-format" && (len(args) != 2 || !strings.HasPrefix(args[1], "refs/heads/")) {
		return nil, errors.New("only branch ref validation is allowed")
	}
	if args[0] == "remote" && !(len(args) == 4 && args[1] == "get-url" && args[2] == "--all" || len(args) == 5 && args[1] == "get-url" && args[2] == "--push" && args[3] == "--all") {
		return nil, errors.New("only remote URL inspection is allowed")
	}
	if args[0] == "worktree" && (len(args) != 4 || args[1] != "list" || args[2] != "--porcelain" || args[3] != "-z") {
		return nil, errors.New("only worktree listing is allowed")
	}
	if args[0] == "config" && strings.Join(args[1:], " ") != "--null --list" {
		return nil, errors.New("only configuration inspection is allowed")
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	prefix := []string{"--git-dir=" + admin, "--work-tree=" + cwd}
	// Override every named protocol permission, including custom remote helpers.
	if args[0] != "config" {
		cfg, err := r.run(ctx, cwd, false, nil, prefix, nil, "config", "--null", "--list")
		if err != nil {
			return nil, err
		}
		for _, record := range bytes.Split(cfg, []byte{0}) {
			key, _, _ := strings.Cut(string(record), "\n")
			if strings.HasPrefix(key, "protocol.") && strings.HasSuffix(key, ".allow") {
				prefix = append(prefix, "-c", key+"=never")
			}
		}
	}
	var extraEnv []string
	if args[0] == "check-ignore" {
		// check-ignore rejects literal pathspec magic, including the environment
		// setting. Its stdin contains pathnames; CheckIgnore prefixes them with ./.
		extraEnv = []string{"GIT_LITERAL_PATHSPECS=0"}
	}
	return r.run(ctx, cwd, false, extraEnv, prefix, input, args...)
}

var historyCommands = map[string]bool{"symbolic-ref": true, "hash-object": true, "mktree": true, "commit-tree": true, "update-ref": true, "rev-parse": true, "cat-file": true, "ls-tree": true, "fsck": true, "show-ref": true, "rev-list": true, "diff-tree": true}

func (r *Runner) History(ctx context.Context, store string, input io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || !historyCommands[args[0]] {
		return nil, errors.New("command not allowed by history policy")
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	if err := ValidateStore(store); err != nil {
		return nil, err
	}
	return r.run(ctx, store, true, []string{"GIT_AUTHOR_NAME=Git Local", "GIT_AUTHOR_EMAIL=git-local@localhost", "GIT_COMMITTER_NAME=Git Local", "GIT_COMMITTER_EMAIL=git-local@localhost"}, []string{"--git-dir=" + store}, input, args...)
}

// BackupPush sends one explicit encrypted-archive commit from a disposable
// private store. All other protocols, local Git hooks and inherited Git config
// remain disabled. The caller must validate the effective destination URL.
func (r *Runner) BackupPush(ctx context.Context, store, remoteURL, commit, ref string) error {
	if err := ValidateStore(store); err != nil {
		return err
	}
	if remoteURL == "" || strings.ContainsAny(remoteURL, "\x00\r\n") || !strings.HasPrefix(ref, "refs/gitlocal/backups/") || strings.ContainsAny(ref, "\x00\r\n :") || len(commit) != 40 {
		return errors.New("invalid backup push arguments")
	}
	for _, c := range commit {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return errors.New("invalid backup commit ID")
		}
	}
	prefix, err := r.backupTransport(ctx, store, remoteURL)
	if err != nil {
		return err
	}
	_, err = r.run(ctx, store, true, nil, prefix, nil, backupArgs("push", remoteURL, commit+":"+ref)...)
	return err
}

// backupArgs builds a backup push or fetch. The URL follows "--", so Git never
// reads it as an option, whatever its first character.
func backupArgs(verb, remoteURL, refspec string) []string {
	args := []string{verb}
	if verb == "push" {
		args = append(args, "--porcelain", "--no-verify", "--no-follow-tags")
	} else {
		args = append(args, "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules")
	}
	return append(args, "--", remoteURL, refspec)
}

// backupTransport allows SSH and HTTPS for one backup command. For an HTTPS
// URL it also passes the user's credential helpers on: the credential.helper
// and credential.<url>.helper entries of the global and system config, in
// Git's order, so Git applies its own URL matching to them. Repository config
// stays ignored, and terminal and askpass prompts stay disabled.
func (r *Runner) backupTransport(ctx context.Context, store, remoteURL string) ([]string, error) {
	prefix := []string{"--git-dir=" + store, "-c", "protocol.ssh.allow=always", "-c", "protocol.https.allow=always"}
	for _, setting := range r.backupConfig {
		prefix = append(prefix, "-c", setting)
	}
	if !strings.HasPrefix(strings.ToLower(remoteURL), "https://") {
		return prefix, nil
	}
	for _, scope := range []string{"--system", "--global"} {
		out, err := r.run(ctx, store, false, nil, nil, nil, "config", "--includes", "--null", scope, "--get-regexp", `^credential\.(.+\.)?helper$`)
		var ge *Error
		if errors.As(err, &ge) && ge.Code == 1 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read credential helpers for the backup remote: %w", err)
		}
		for _, entry := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
			key, value, ok := strings.Cut(entry, "\n")
			if !ok || strings.Contains(key, "=") {
				return nil, fmt.Errorf("unsupported credential helper setting %q for the backup remote", key)
			}
			prefix = append(prefix, "-c", key+"="+value)
		}
	}
	return prefix, nil
}

// BackupFetch retrieves one explicit backup ref into a disposable store. It
// cannot fetch mainline refs or use local/custom transports.
func (r *Runner) BackupFetch(ctx context.Context, store, remoteURL, ref string) error {
	if err := ValidateStore(store); err != nil {
		return err
	}
	if remoteURL == "" || strings.ContainsAny(remoteURL, "\x00\r\n") || !strings.HasPrefix(ref, "refs/gitlocal/backups/") || strings.ContainsAny(ref, "\x00\r\n :") {
		return errors.New("invalid backup fetch arguments")
	}
	prefix, err := r.backupTransport(ctx, store, remoteURL)
	if err != nil {
		return err
	}
	_, err = r.run(ctx, store, true, nil, prefix, nil, backupArgs("fetch", remoteURL, ref+":refs/heads/import")...)
	return err
}

// HistoryBlobTo streams one private blob, avoiding the bounded diagnostic
// buffer for encrypted archives that may be larger than 16 MiB.
func (r *Runner) HistoryBlobTo(ctx context.Context, store, id string, dst io.Writer) error {
	if err := ValidateStore(store); err != nil {
		return err
	}
	if len(id) != 40 && len(id) != 64 {
		return errors.New("invalid private blob ID")
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return errors.New("invalid private blob ID")
		}
	}
	_, err := r.runOutput(ctx, store, true, nil, []string{"--git-dir=" + store}, nil, dst, "cat-file", "blob", id)
	return err
}

// CheckCapabilities probes behavior in private disposable repositories, once per
// runner. In particular the offline-read probe permits its local fixture transport
// so a silently ignored GIT_NO_LAZY_FETCH cannot pass by transport denial alone.
func (r *Runner) CheckCapabilities(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checked {
		return nil
	}
	v, err := r.run(ctx, r.private, true, nil, nil, nil, "version")
	if err != nil {
		return err
	}
	if err = requireVersion(v); err != nil {
		return err
	}
	base, err := os.MkdirTemp(r.private, "capability-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(base)
	source := filepath.Join(base, "source.git")
	if err = r.InitBare(ctx, source); err != nil {
		return err
	}
	prefix := []string{"--git-dir=" + source}
	run := func(input io.Reader, args ...string) ([]byte, error) {
		return r.run(ctx, base, true, []string{"GIT_AUTHOR_NAME=probe", "GIT_AUTHOR_EMAIL=probe@localhost", "GIT_COMMITTER_NAME=probe", "GIT_COMMITTER_EMAIL=probe@localhost"}, prefix, input, args...)
	}
	blob, err := run(strings.NewReader("capability probe"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		return err
	}
	tree, err := run(strings.NewReader("100644 blob "+strings.TrimSpace(string(blob))+"\tprobe\x00"), "mktree", "-z")
	if err != nil {
		return err
	}
	commit, err := run(strings.NewReader("probe\n"), "commit-tree", strings.TrimSpace(string(tree)))
	if err != nil {
		return err
	}
	if _, err = run(nil, "update-ref", "refs/heads/main", strings.TrimSpace(string(commit))); err != nil {
		return err
	}
	if _, err = run(nil, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if _, err = run(nil, "config", "uploadpack.allowFilter", "true"); err != nil {
		return err
	}
	clone := filepath.Join(base, "clone")
	if _, err = r.run(ctx, base, true, nil, []string{"-c", "protocol.file.allow=always"}, nil, "clone", "--quiet", "--no-local", "--no-checkout", "--filter=tree:0", source, clone); err != nil {
		return fmt.Errorf("offline capability fixture: %w", err)
	}
	// Source is disposable and trusted; enable only file transport for this probe.
	_, err = r.run(ctx, clone, true, nil, []string{"-c", "protocol.file.allow=always"}, nil, "ls-tree", "-r", "HEAD")
	var gitErr *Error
	if err == nil || !errors.As(err, &gitErr) {
		return errors.New("Git failed offline-read capability check")
	}
	positive, err := r.run(ctx, clone, true, []string{"GIT_NO_LAZY_FETCH=0"}, []string{"-c", "protocol.file.allow=always"}, nil, "ls-tree", "-r", "HEAD")
	if err != nil || !bytes.Contains(positive, []byte("probe")) {
		return errors.New("offline-read fixture could not verify its positive fetch control")
	}
	if _, err = r.run(ctx, clone, true, nil, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil {
		return err
	}
	// Git warns (successfully) about unknown fsync components, so validate the
	// effective values and require the version floor as well as actual writes.
	r.checked = true
	return nil
}

// AdminAt resolves a registered worktree without inherited repository selection.
func (r *Runner) AdminAt(ctx context.Context, root string) (string, error) {
	b, err := r.run(ctx, root, false, nil, nil, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	p := strings.TrimSuffix(string(b), "\n")
	p = strings.TrimSuffix(p, "\r")
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n\x00") {
		return "", errors.New("invalid administrative directory")
	}
	return filepath.EvalSymlinks(p)
}

// RootAt asks Git for a containing worktree without inherited selection.
func (r *Runner) RootAt(ctx context.Context, cwd string) (string, error) {
	b, err := r.run(ctx, cwd, false, nil, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	p := strings.TrimSuffix(string(b), "\n")
	p = strings.TrimSuffix(p, "\r")
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n\x00") {
		return "", errors.New("invalid worktree root")
	}
	return filepath.EvalSymlinks(p)
}

func requireVersion(v []byte) error {
	fields := strings.Fields(string(v))
	if len(fields) < 3 {
		return errors.New("unrecognized Git version")
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 3 {
		return errors.New("unrecognized Git version")
	}
	major, e1 := strconv.Atoi(parts[0])
	minor, e2 := strconv.Atoi(parts[1])
	patch, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return errors.New("unrecognized Git version")
	}
	if major < 2 || major == 2 && (minor < 50 || minor == 50 && patch < 1) {
		return fmt.Errorf("Git 2.50.1 or later required; found %s", strings.TrimSpace(string(v)))
	}

	return nil
}

// CheckIgnore supplies only NUL-framed paths, never arbitrary stdin commands.
func (r *Runner) CheckIgnore(ctx context.Context, root, admin string, paths []string) (map[string]bool, error) {
	matches, err := r.CheckIgnoreMatches(ctx, root, admin, paths)
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for path, match := range matches {
		result[path] = match.Ignored
	}
	return result, nil
}

// IgnoreMatch is the last ignore rule Git applied to a path.
type IgnoreMatch struct {
	Ignored bool
	Source  string
	Line    int
	Pattern string
}

// CheckIgnoreMatches reports, for every path, whether it is ignored and which
// rule decided it.
func (r *Runner) CheckIgnoreMatches(ctx context.Context, root, admin string, paths []string) (map[string]IgnoreMatch, error) {
	var input bytes.Buffer
	for _, path := range paths {
		if strings.ContainsRune(path, 0) {
			return nil, errors.New("NUL in ignore query")
		}
		input.WriteString("./" + path)
		input.WriteByte(0)
	}
	if len(paths) == 0 {
		return map[string]IgnoreMatch{}, nil
	}
	out, err := r.mainInput(ctx, root, admin, &input, "check-ignore", "--stdin", "-z", "--no-index", "--verbose", "--non-matching")
	if err != nil {
		var ge *Error
		if !errors.As(err, &ge) || ge.Code != 1 {
			return nil, err
		}
	}
	fields := bytes.Split(out, []byte{0})
	if len(fields) == 0 || len(fields[len(fields)-1]) != 0 || (len(fields)-1)%4 != 0 {
		return nil, errors.New("malformed Git ignore output")
	}
	result := map[string]IgnoreMatch{}
	for i := 0; i < len(fields)-1; i += 4 {
		pattern := fields[i+2]
		line, _ := strconv.Atoi(string(fields[i+1]))
		result[strings.TrimPrefix(string(fields[i+3]), "./")] = IgnoreMatch{Ignored: len(pattern) > 0 && pattern[0] != '!', Source: string(fields[i]), Line: line, Pattern: string(pattern)}
	}
	for _, path := range paths {
		if _, ok := result[path]; !ok {
			return nil, errors.New("missing Git ignore result")
		}
	}
	return result, nil
}

// IgnoreCase reports the effective core.ignorecase of the checkout at root.
// Git then matches ignore patterns case-insensitively, except inside bracket
// classes.
func (r *Runner) IgnoreCase(ctx context.Context, root, admin string) (bool, error) {
	out, err := r.Main(ctx, root, admin, "config", "--null", "--list")
	if err != nil {
		return false, err
	}
	result := false
	for _, record := range bytes.Split(out, []byte{0}) {
		key, value, hasValue := strings.Cut(string(record), "\n")
		if strings.ToLower(key) != "core.ignorecase" {
			continue
		}
		if !hasValue {
			result = true
			continue
		}
		switch strings.ToLower(value) {
		case "true", "yes", "on", "1":
			result = true
		case "false", "no", "off", "0", "":
			result = false
		default:
			n, err := strconv.ParseInt(value, 0, 64)
			if err != nil {
				return false, fmt.Errorf("invalid core.ignorecase value %q", value)
			}
			result = n != 0
		}
	}
	return result, nil
}

// HistoryBlob streams only a literal object ID through the hardened private-store runner.
func (r *Runner) HistoryBlob(ctx context.Context, store, id string, output io.Writer) error {
	if len(id) != 40 && len(id) != 64 {
		return errors.New("invalid blob ID")
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return errors.New("invalid blob ID")
		}
	}
	if output == nil {
		return errors.New("missing blob output")
	}
	if err := r.CheckCapabilities(ctx); err != nil {
		return err
	}
	if err := ValidateStore(store); err != nil {
		return err
	}
	_, err := r.runOutput(ctx, store, true, nil, []string{"--git-dir=" + store}, nil, output, "cat-file", "blob", id)
	return err
}
