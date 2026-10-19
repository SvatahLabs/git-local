// Run with: go run ./scripts/release-binaries.go (the version comes from VERSION)
// This prepares local artifacts; it does not publish a release.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/SvatahLabs/git-local/internal/gitexec"
)

type target struct{ OS, Arch string }

var releaseTargets = []target{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

type artifact struct {
	Target string `json:"target"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	SchemaVersion  int        `json:"schemaVersion"`
	Version        string     `json:"version"`
	MinimumGit     string     `json:"minimumGit"`
	GoVersion      string     `json:"goVersion"`
	SourceRevision string     `json:"sourceRevision,omitempty"`
	Artifacts      []artifact `json:"artifacts"`
}

func main() {
	stopOnInterrupt()
	root, err := os.Getwd()
	if err == nil {
		err = run(root, os.Args[1:])
	}
	if interrupted.Err() != nil {
		fmt.Fprintln(os.Stderr, "release builder stopped; staging removed")
		os.Exit(130)
	}
	if err != nil {
		fail(err)
	}
}

// run builds the release from the module root. It returns errors instead of
// exiting, so the staging directory is always removed.
func run(root string, args []string) error {
	flags := flag.NewFlagSet("release-binaries", flag.ContinueOnError)
	output := flags.String("out", "", "new artifact directory (default dist/binaries/v<VERSION>)")
	hostOnly := flags.Bool("host-only", false, "build and smoke-test only the current OS and architecture")
	smokeExisting := flags.String("smoke-existing", "", "smoke-test a previously built native artifact without a Go toolchain")
	goBinary := flags.String("go", "go", "Go toolchain executable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *smokeExisting != "" {
		if flags.NArg() != 0 || *output != "" || *hostOnly {
			return errors.New("-smoke-existing cannot be combined with build options")
		}
		if err := smokeExistingArtifact(*smokeExisting); err != nil {
			return err
		}
		fmt.Printf("Native artifact smoke test passed: %s\n", *smokeExisting)
		return nil
	}
	if flags.NArg() != 0 {
		return errors.New("usage: go run ./scripts/release-binaries.go [-host-only] [-out new-directory]")
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return errors.New("run the release builder from the module root")
	}
	version, err := readVersion(root)
	if err != nil {
		return err
	}
	if *output == "" {
		*output = filepath.Join(root, "dist", "binaries", "v"+version)
	}
	final, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("artifact directory already exists: %s", final)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0755); err != nil {
		return err
	}
	stage, err := makeStaging(filepath.Dir(final), ".gitlocal-release-")
	if err != nil {
		return err
	}
	defer removeStaging(stage)
	goVersion, err := goEnv(*goBinary, root, "GOVERSION")
	if err != nil {
		return err
	}
	selected := releaseTargets
	if *hostOnly {
		selected = []target{{runtime.GOOS, runtime.GOARCH}}
	}
	info := manifest{SchemaVersion: 1, Version: version, MinimumGit: gitexec.MinimumGit, GoVersion: goVersion}
	if info.SourceRevision, err = gitRevision(root, stage); err != nil {
		return fmt.Errorf("read the source commit: %w", err)
	}
	for _, platform := range selected {
		name := artifactName(version, platform)
		path := filepath.Join(stage, name)
		cmd := exec.CommandContext(interrupted, *goBinary, "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid= -X github.com/SvatahLabs/git-local/internal/cli.Version="+version+" -X github.com/SvatahLabs/git-local/internal/cli.Commit="+info.SourceRevision, "-o", path, "./cmd/git-local")
		cmd.Dir = root
		cmd.Env = buildEnv(platform)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s/%s: %w\n%s", platform.OS, platform.Arch, err, output)
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		info.Artifacts = append(info.Artifacts, artifact{platform.OS + "/" + platform.Arch, name, digest})
	}
	if err := smokeHost(stage, info); err != nil {
		return err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(stage, "release.json"), data, 0644); err != nil {
		return err
	}
	var sums strings.Builder
	for _, item := range info.Artifacts {
		fmt.Fprintf(&sums, "%s  %s\n", item.SHA256, item.File)
	}
	manifestHash := sha256.Sum256(data)
	fmt.Fprintf(&sums, "%x  release.json\n", manifestHash)
	if err := os.WriteFile(filepath.Join(stage, "SHA256SUMS"), []byte(sums.String()), 0644); err != nil {
		return err
	}
	if err := interrupted.Err(); err != nil {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	fmt.Printf("Built %d binaries in %s\n", len(info.Artifacts), final)
	return nil
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

// makeStaging creates a staging directory, which run removes when it
// returns, also after an interrupt.
func makeStaging(dir, pattern string) (string, error) { return os.MkdirTemp(dir, pattern) }

func removeStaging(stage string) { os.RemoveAll(stage) }

// interrupted is canceled when the builder is interrupted or terminated: Go's
// default signal handling would exit without running deferred calls (the
// staging cleanup) while child builds kept writing into staging. Builds and
// smoke tests run under it, so they stop, run returns, and its deferred calls
// remove the staging directory before main exits.
var interrupted, interrupt = context.WithCancel(context.Background())

// stopOnInterrupt cancels interrupted at the first SIGINT or SIGTERM; a
// second one ends the builder at once.
func stopOnInterrupt() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		signal.Stop(signals)
		interrupt()
	}()
}

func artifactName(version string, platform target) string {
	name := fmt.Sprintf("git-local_v%s_%s_%s", version, platform.OS, platform.Arch)
	if platform.OS == "windows" {
		name += ".exe"
	}
	return name
}

func buildEnv(platform target) []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key != "GOOS" && key != "GOARCH" && key != "CGO_ENABLED" && key != "GOWORK" {
			env = append(env, item)
		}
	}
	// An ignored go.work could change what is built without marking it dirty.
	return append(env, "GOOS="+platform.OS, "GOARCH="+platform.Arch, "CGO_ENABLED=0", "GOWORK=off")
}

func goEnv(goBinary, root, key string) (string, error) {
	cmd := exec.CommandContext(interrupted, goBinary, "env", key)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read Go version: %w\n%s", err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

// readVersion reads the release version from the VERSION file, its only
// source.
func readVersion(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return "", fmt.Errorf("VERSION must hold major.minor.patch, not %q", version)
	}
	return version, nil
}

// gitRevision is the source commit, marked +dirty when the checkout has
// uncommitted changes or untracked files (Go compiles every file in a package
// directory) other than the staging directory. root must be the checkout
// itself, not a copy inside another repository, whose commit would be
// recorded instead.
func gitRevision(root, stage string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s isn't a Git checkout: %w", root, err)
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err != nil {
		return "", err
	} else if top != resolved {
		return "", fmt.Errorf("%s isn't a Git checkout (it's inside %s)", root, top)
	}
	cmd = exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = root
	if output, err = cmd.Output(); err != nil {
		return "", err
	}
	revision := strings.TrimSpace(string(output))
	args := []string{"status", "--porcelain", "--", "."}
	if resolved, err := filepath.EvalSymlinks(stage); err == nil {
		if rel, err := filepath.Rel(top, resolved); err == nil && filepath.IsLocal(rel) {
			args = append(args, ":(exclude)"+filepath.ToSlash(rel))
		}
	}
	cmd = exec.Command("git", args...)
	cmd.Dir = root
	status, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(status)) != 0 {
		revision += "+dirty"
	}
	return revision, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func smokeExistingArtifact(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return err
	}
	var info manifest
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}
	if info.SchemaVersion != 1 || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(info.Version) {
		return errors.New("invalid release manifest")
	}
	host := runtime.GOOS + "/" + runtime.GOARCH
	var native *artifact
	for i := range info.Artifacts {
		if info.Artifacts[i].Target != host {
			continue
		}
		if native != nil {
			return errors.New("duplicate native artifact in release manifest")
		}
		native = &info.Artifacts[i]
	}
	if native == nil {
		return fmt.Errorf("release manifest has no native artifact for %s", host)
	}
	wantName := artifactName(info.Version, target{runtime.GOOS, runtime.GOARCH})
	if native.File != wantName {
		return errors.New("release manifest has an invalid native artifact name")
	}
	source := filepath.Join(dir, wantName)
	gotHash, err := fileSHA256(source)
	if err != nil {
		return err
	}
	if gotHash != native.SHA256 {
		return errors.New("native artifact does not match release manifest checksum")
	}
	stage, err := makeStaging("", "gitlocal-release-smoke-")
	if err != nil {
		return err
	}
	defer removeStaging(stage)
	image, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, wantName), image, 0755); err != nil {
		return err
	}
	return smokeHost(stage, info)
}

func smokeHost(stage string, info manifest) error {
	host := runtime.GOOS + "/" + runtime.GOARCH
	index := slices.IndexFunc(info.Artifacts, func(a artifact) bool { return a.Target == host })
	if index < 0 {
		return fmt.Errorf("no native artifact for smoke test on %s", host)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	git, err = filepath.Abs(git)
	if err != nil {
		return err
	}
	bin := filepath.Join(stage, "smoke-bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		return err
	}
	name := "git-local"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	image, err := os.ReadFile(filepath.Join(stage, info.Artifacts[index].File))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(bin, name), image, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(bin)
	paths := []string{bin, filepath.Dir(git)}
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		paths = append(paths, filepath.Join(root, "System32"), root)
	} else {
		paths = append(paths, "/usr/bin", "/bin")
	}
	// /usr/bin holds git and sh, and on some systems (GitHub's Ubuntu runners)
	// go too. A go first on PATH that records its use and fails shadows it, so
	// the smoke test still proves the binary never runs a Go toolchain.
	goUsed := filepath.Join(stage, "smoke-go-used")
	if runtime.GOOS != "windows" {
		shadow := "#!/bin/sh\necho \"$@\" >> '" + goUsed + "'\nexit 97\n"
		if err := os.WriteFile(filepath.Join(bin, "go"), []byte(shadow), 0755); err != nil {
			return err
		}
	}
	for _, dir := range paths[1:] {
		for _, name := range []string{"go", "go.exe"} {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() && runtime.GOOS == "windows" {
				return fmt.Errorf("smoke-test PATH still contains a Go toolchain: %s", filepath.Join(dir, name))
			}
		}
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if upper != "PATH" && upper != "HOME" && upper != "USERPROFILE" && upper != "XDG_CONFIG_HOME" && !strings.HasPrefix(upper, "GIT_") {
			env = append(env, item)
		}
	}
	home := filepath.Join(stage, "smoke-home")
	if err := os.Mkdir(home, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(home)
	env = append(env, "PATH="+strings.Join(paths, string(os.PathListSeparator)), "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home)
	run := func(dir string, command ...string) ([]byte, error) {
		cmd := exec.CommandContext(interrupted, command[0], command[1:]...)
		cmd.Dir = dir
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("native smoke test %v failed: %w\n%s", command, err, output)
		}
		return output, nil
	}
	for _, command := range [][]string{{filepath.Join(bin, name), "--version"}, {git, "local", "--version"}} {
		output, err := run(stage, command...)
		if err != nil {
			return err
		}
		if !strings.Contains(string(output), info.Version) || !strings.Contains(string(output), info.SourceRevision) {
			return fmt.Errorf("native smoke test %v reported wrong version or commit: %s", command, output)
		}
	}
	repo := filepath.Join(stage, "smoke-repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(repo)
	for _, command := range [][]string{
		{git, "init", "-q", "-b", "main", "--template="},
		{git, "config", "user.name", "Release Smoke"},
		{git, "config", "user.email", "smoke@localhost"},
		// Git for Windows sets core.autocrlf=true, which overlays refuse.
		{git, "config", "core.autocrlf", "false"},
	} {
		if _, err := run(repo, command...); err != nil {
			return err
		}
	}
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), base, 0600); err != nil {
		return err
	}
	for _, command := range [][]string{{git, "add", "app.txt"}, {git, "commit", "-qm", "initial"}} {
		if _, err := run(repo, command...); err != nil {
			return err
		}
	}
	beforeHead, err := run(repo, git, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.local"), []byte("saved by packaged binary\n"), 0600); err != nil {
		return err
	}
	for _, command := range [][]string{
		{git, "local", "init"},
		{git, "local", "track", "notes.local"},
		{git, "local", "commit", "-m", "release smoke"},
	} {
		if _, err := run(repo, command...); err != nil {
			return err
		}
	}
	content, err := run(repo, git, "local", "show", "latest:notes.local")
	if err != nil || string(content) != "saved by packaged binary\n" {
		return fmt.Errorf("packaged binary could not read its snapshot: %v %q", err, content)
	}
	afterHead, err := run(repo, git, "rev-parse", "HEAD")
	if err != nil || string(afterHead) != string(beforeHead) {
		return fmt.Errorf("packaged binary changed main HEAD: %v", err)
	}
	if _, err := run(repo, git, "local", "hook", "install"); err != nil {
		return err
	}
	if _, err := run(repo, git, "add", "-f", "notes.local"); err != nil {
		return err
	}
	guarded := exec.CommandContext(interrupted, git, "commit", "-qm", "must not leak")
	guarded.Dir, guarded.Env = repo, env
	guardOutput, guardErr := guarded.CombinedOutput()
	if guardErr == nil || !bytes.Contains(guardOutput, []byte("notes.local")) {
		return fmt.Errorf("packaged commit guard did not reject the selected file: %v\n%s", guardErr, guardOutput)
	}
	if current, err := run(repo, git, "rev-parse", "HEAD"); err != nil || string(current) != string(beforeHead) {
		return fmt.Errorf("rejected commit advanced main HEAD: %v", err)
	}
	if _, err := run(repo, git, "restore", "--staged", "--", "notes.local"); err != nil {
		return err
	}
	if _, err := run(repo, git, "local", "auto", "install"); err != nil {
		return err
	}
	localApp := bytes.Replace(base, []byte("port = 80"), []byte("port = 8080"), 1)
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), localApp, 0600); err != nil {
		return err
	}
	if _, err := run(repo, git, "local", "overlay", "add", "port", "--", "app.txt"); err != nil {
		return err
	}
	if _, err := run(repo, git, "add", "app.txt"); err != nil {
		return err
	}
	if staged, err := run(repo, git, "cat-file", "blob", ":app.txt"); err != nil || !bytes.Equal(staged, base) {
		return fmt.Errorf("packaged overlay entered the main index: %v %q", err, staged)
	}
	if _, err := run(repo, git, "checkout", "--", "app.txt"); err != nil {
		return err
	}
	if working, err := os.ReadFile(filepath.Join(repo, "app.txt")); err != nil || !bytes.Equal(working, localApp) {
		return fmt.Errorf("packaged overlay did not reapply after checkout: %v %q", err, working)
	}
	updatedNote := []byte("saved automatically by packaged binary\n")
	if err := os.WriteFile(filepath.Join(repo, "notes.local"), updatedNote, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("ordinary mainline edit\n"), 0600); err != nil {
		return err
	}
	for _, command := range [][]string{{git, "add", "feature.txt"}, {git, "commit", "-qm", "ordinary change"}} {
		if _, err := run(repo, command...); err != nil {
			return err
		}
	}
	if recorded, err := run(repo, git, "local", "show", "latest:notes.local"); err != nil || !bytes.Equal(recorded, updatedNote) {
		return fmt.Errorf("packaged automatic snapshot did not capture selected changes: %v %q", err, recorded)
	}
	if committed, err := run(repo, git, "show", "HEAD:app.txt"); err != nil || !bytes.Equal(committed, base) {
		return fmt.Errorf("packaged overlay entered a mainline commit: %v %q", err, committed)
	}
	if used, err := os.ReadFile(goUsed); err == nil {
		return fmt.Errorf("packaged binary ran a Go toolchain: go %s", used)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
