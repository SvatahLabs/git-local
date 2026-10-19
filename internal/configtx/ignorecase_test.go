package configtx

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/exclude"
	"github.com/SvatahLabs/git-local/internal/selection"
)

func TestExcludedPathsVisibleUnderEitherIgnoreCase(t *testing.T) {
	for _, ignoreCase := range []bool{true, false} {
		t.Run(fmt.Sprintf("ignorecase=%v", ignoreCase), func(t *testing.T) {
			r, g, w := fixture(t)
			r.Git("config", "core.ignorecase", fmt.Sprint(ignoreCase))
			ctx := context.Background()
			for _, p := range []string{"docs/README.md", "docs/Readme2.txt", "docs/other.txt", "docs/Sub/File.txt", "docs/Sub/other.txt", "docs/sub2/x"} {
				write(t, r.Root, p)
			}
			if _, err := Change(ctx, g, r.Root, true, "docs"); err != nil {
				t.Fatal(err)
			}
			if _, err := Change(ctx, g, r.Root, false, "docs/README.md", "docs/Sub/File.txt"); err != nil {
				t.Fatal(err)
			}
			got := ignored(t, g, w, "docs/README.md", "docs/Sub/File.txt", "docs/Readme2.txt", "docs/other.txt", "docs/Sub/other.txt", "docs/sub2/x")
			for p, want := range map[string]bool{"docs/README.md": false, "docs/Sub/File.txt": false, "docs/Readme2.txt": true, "docs/other.txt": true, "docs/Sub/other.txt": true, "docs/sub2/x": true} {
				if got[p] != want {
					block, _ := os.ReadFile(w.Exclude)
					t.Errorf("%s ignored=%v want %v\n%s", p, got[p], want, block)
				}
			}
			status := string(r.Git("status", "--porcelain", "--untracked-files=all"))
			for _, p := range []string{"docs/README.md", "docs/Sub/File.txt"} {
				if !strings.Contains(status, p) {
					t.Errorf("git status hides %s:\n%s", p, status)
				}
			}
		})
	}
}

func TestLegacyCaseSensitiveBlockIsAcceptedFlaggedAndRewritten(t *testing.T) {
	r, g, w := fixture(t)
	r.Git("config", "core.ignorecase", "true")
	ctx := context.Background()
	for _, p := range []string{"docs/README.md", "docs/other.txt", "notes/a"} {
		write(t, r.Root, p)
	}
	if _, err := Change(ctx, g, r.Root, true, "docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, g, r.Root, false, "docs/README.md"); err != nil {
		t.Fatal(err)
	}
	// The block an earlier release wrote: case-sensitive classes.
	legacy, err := exclude.Compile([]selection.State{load(t, w)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.Exclude, exclude.Render(nil, legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if !ignored(t, g, w, "docs/README.md")["docs/README.md"] {
		t.Fatal("fixture: the legacy block should hide README.md")
	}
	if err := ReadDiagnostic(ctx, g, r.Root, func(d DiagnosticState) error {
		if d.ConfigErr != nil || !d.IgnoreOutdated {
			t.Fatalf("legacy block not accepted and flagged: %v %v", d.ConfigErr, d.IgnoreOutdated)
		}
		if d.IgnoreErr == nil || !strings.Contains(d.IgnoreErr.Error(), "docs/README.md") {
			t.Fatalf("verification missed the ignored exclusion: %v", d.IgnoreErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, g, r.Root, true, "notes"); err != nil {
		t.Fatalf("track with a legacy block: %v", err)
	}
	if ignored(t, g, w, "docs/README.md")["docs/README.md"] || !ignored(t, g, w, "docs/other.txt", "notes/a")["notes/a"] {
		t.Fatal("block not rewritten for core.ignorecase")
	}
	if err := ReadDiagnostic(ctx, g, r.Root, func(d DiagnosticState) error {
		if d.IgnoreErr != nil || d.IgnoreOutdated {
			t.Fatalf("after rewrite: %v %v", d.IgnoreErr, d.IgnoreOutdated)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
