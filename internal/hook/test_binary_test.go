package hook_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func testBinary(t *testing.T) string {
	t.Helper()
	name := "git-local"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if prebuilt := os.Getenv("GITLOCAL_TEST_BINARY"); prebuilt != "" {
		if !filepath.IsAbs(prebuilt) {
			t.Fatalf("prebuilt git-local test binary is not absolute: %q", prebuilt)
		}
		source, err := os.Open(prebuilt)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		destination, err := os.OpenFile(bin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(destination, source)
		closeErr := destination.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("copy prebuilt git-local: %v %v", copyErr, closeErr)
		}
		return bin
	}
	build := exec.Command("go", "build", "-o", bin, "../../cmd/git-local")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build git-local: %v\n%s", err, output)
	}
	return bin
}
