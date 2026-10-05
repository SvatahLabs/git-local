// Run with: go run ./scripts/release-binaries.go -version 0.1.4
// This prepares local artifacts; it does not publish a release.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
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
	version := flag.String("version", "", "release version in major.minor.patch form")
	output := flag.String("out", "", "new artifact directory (default dist/binaries/v<version>)")
	hostOnly := flag.Bool("host-only", false, "build and smoke-test only the current OS and architecture")
	smokeExisting := flag.String("smoke-existing", "", "smoke-test a previously built native artifact without a Go toolchain")
	goBinary := flag.String("go", "go", "Go toolchain executable")
	flag.Parse()
	if *smokeExisting != "" {
		if flag.NArg() != 0 || *version != "" || *output != "" || *hostOnly {
			fail(errors.New("-smoke-existing cannot be combined with build options"))
		}
		if err := smokeExistingArtifact(*smokeExisting); err != nil {
			fail(err)
		}
		fmt.Printf("Native artifact smoke test passed: %s\n", *smokeExisting)
		return
	}
	if flag.NArg() != 0 || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(*version) {
		fail(errors.New("usage: go run ./scripts/release-binaries.go -version major.minor.patch [-host-only] [-out new-directory]"))
	}
	root, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		fail(errors.New("run the release builder from the module root"))
	}
	if *output == "" {
		*output = filepath.Join(root, "dist", "binaries", "v"+*version)
	}
	final, err := filepath.Abs(*output)
	if err != nil {
		fail(err)
	}
	if _, err := os.Lstat(final); err == nil {
		fail(fmt.Errorf("artifact directory already exists: %s", final))
	} else if !os.IsNotExist(err) {
		fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0755); err != nil {
		fail(err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(final), ".gitlocal-release-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(stage)
	goVersion, err := goEnv(*goBinary, root, "GOVERSION")
	if err != nil {
		fail(err)
	}
	selected := releaseTargets
	if *hostOnly {
		selected = []target{{runtime.GOOS, runtime.GOARCH}}
	}
	info := manifest{SchemaVersion: 1, Version: *version, MinimumGit: "2.50.1", GoVersion: goVersion}
	if revision, err := gitRevision(root); err == nil {
		info.SourceRevision = revision
	}
	for _, platform := range selected {
		name := artifactName(*version, platform)
		path := filepath.Join(stage, name)
		cmd := exec.Command(*goBinary, "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid= -X gitlocal/internal/cli.Version="+*version, "-o", path, "./cmd/git-local")
		cmd.Dir = root
		cmd.Env = buildEnv(platform)
		if output, err := cmd.CombinedOutput(); err != nil {
			fail(fmt.Errorf("build %s/%s: %w\n%s", platform.OS, platform.Arch, err, output))
		}
		digest, err := fileSHA256(path)
		if err != nil {
			fail(err)
		}
		info.Artifacts = append(info.Artifacts, artifact{platform.OS + "/" + platform.Arch, name, digest})
	}
	if err := smokeHost(stage, info); err != nil {
		fail(err)
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		fail(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(stage, "release.json"), data, 0644); err != nil {
		fail(err)
	}
	var sums strings.Builder
	for _, item := range info.Artifacts {
		fmt.Fprintf(&sums, "%s  %s\n", item.SHA256, item.File)
	}
	manifestHash := sha256.Sum256(data)
	fmt.Fprintf(&sums, "%x  release.json\n", manifestHash)
	if err := os.WriteFile(filepath.Join(stage, "SHA256SUMS"), []byte(sums.String()), 0644); err != nil {
		fail(err)
	}
	if err := os.Rename(stage, final); err != nil {
		fail(err)
	}
	fmt.Printf("Built %d binaries in %s\n", len(info.Artifacts), final)
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

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
		if key != "GOOS" && key != "GOARCH" && key != "CGO_ENABLED" {
			env = append(env, item)
		}
	}
	return append(env, "GOOS="+platform.OS, "GOARCH="+platform.Arch, "CGO_ENABLED=0")
}

func goEnv(goBinary, root, key string) (string, error) {
	cmd := exec.Command(goBinary, "env", key)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read Go version: %w\n%s", err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func gitRevision(root string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = root
	output, err := cmd.Output()
	return strings.TrimSpace(string(output)), err
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
	stage, err := os.MkdirTemp("", "gitlocal-release-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
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
	for _, dir := range paths {
		for _, name := range []string{"go", "go.exe"} {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
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
		cmd := exec.Command(command[0], command[1:]...)
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
		if !strings.Contains(string(output), info.Version) {
			return fmt.Errorf("native smoke test %v reported wrong version: %s", command, output)
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
	guarded := exec.Command(git, "commit", "-qm", "must not leak")
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
	return nil
}
