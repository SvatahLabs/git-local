package overlay

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	p, err = filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStripKeepsOrdinaryEdits(t *testing.T) {
	git := gitPath(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	local := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	working := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = debug\n")
	clean, err := Strip(context.Background(), git, base, local, working)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clean, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = debug\n")) {
		t.Fatalf("unexpected clean content %q", clean)
	}
	clean, err = Strip(context.Background(), git, base, local, local)
	if err != nil || !bytes.Equal(clean, base) {
		t.Fatalf("overlay-only content leaked: %v %q", err, clean)
	}
}

func TestApplyAndConflicts(t *testing.T) {
	git := gitPath(t)
	base := []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	local := []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = info\n")
	applied, err := Apply(context.Background(), git, base, local, []byte("port = 80\na\nb\nc\nd\ne\nf\ng\nlog = warn\n"))
	if err != nil || !bytes.Equal(applied, []byte("port = 8080\na\nb\nc\nd\ne\nf\ng\nlog = warn\n")) {
		t.Fatalf("upstream edit lost: %v %q", err, applied)
	}
	if _, err := Apply(context.Background(), git, base, local, []byte("port = 9090\na\nb\nc\nd\ne\nf\ng\nlog = info\n")); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlap was not rejected: %v", err)
	}
	if _, err := Strip(context.Background(), git, base, local, []byte("port = 7070\na\nb\nc\nd\ne\nf\ng\nlog = info\n")); !errors.Is(err, ErrConflict) {
		t.Fatalf("local overlap was not rejected: %v", err)
	}
}
