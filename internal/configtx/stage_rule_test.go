package configtx

import (
	"bytes"
	"context"
	"os"
	"testing"

	"gitlocal/internal/exclude"
	"gitlocal/internal/storage"
)

func TestStageRuleSurvivesUntrackingEverything(t *testing.T) {
	r, g, w := fixture(t)
	ctx := context.Background()
	write(t, r.Root, "notes/a")
	if _, err := Change(ctx, g, r.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, g, r.Root, false, "notes"); err != nil {
		t.Fatal(err)
	}
	token, err := storage.UUID()
	if err != nil {
		t.Fatal(err)
	}
	stage := "notes/.gitlocal-tmp-" + token
	write(t, r.Root, stage)
	if got := ignored(t, g, w, stage, "notes/a"); !got[stage] || got["notes/a"] {
		data, _ := os.ReadFile(w.Exclude)
		t.Fatalf("after untracking everything: %v\n%s", got, data)
	}
	if err := ReadDiagnostic(ctx, g, r.Root, func(d DiagnosticState) error {
		if d.ConfigErr != nil || d.IgnoreErr != nil || d.IgnoreOutdated {
			t.Fatalf("diagnosis: %v %v %v", d.ConfigErr, d.IgnoreErr, d.IgnoreOutdated)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptySelectionWithoutBlockFromEarlierReleaseIsAccepted(t *testing.T) {
	r, g, w := fixture(t)
	ctx := context.Background()
	write(t, r.Root, "notes/a")
	if _, err := Change(ctx, g, r.Root, true, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, g, r.Root, false, "notes"); err != nil {
		t.Fatal(err)
	}
	// Earlier releases removed the block entirely with the last selection.
	data, err := os.ReadFile(w.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	user, err := exclude.UserBytes(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.Exclude, user, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReadDiagnostic(ctx, g, r.Root, func(d DiagnosticState) error {
		if d.ConfigErr != nil || d.IgnoreErr != nil {
			t.Fatalf("legacy empty selection rejected: %v %v", d.ConfigErr, d.IgnoreErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, g, r.Root, true, "notes"); err != nil {
		t.Fatalf("track after a legacy empty block: %v", err)
	}
	if data, _ := os.ReadFile(w.Exclude); !bytes.Contains(data, []byte(exclude.StagePattern)) {
		t.Fatalf("block not rewritten:\n%s", data)
	}
}
