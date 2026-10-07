package configtx

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAbandonNamesPathsItStopsIgnoring: a track stopped after installing its
// transitional block but before writing the selection. Abandon reinstalls the
// block for the live selection, so the path being tracked is no longer
// ignored; it must say so, and report that it rewrote the block.
func TestAbandonNamesPathsItStopsIgnoring(t *testing.T) {
	r, g, _ := fixture(t)
	write(t, r.Root, "local")
	ctx := context.Background()
	result, err := change(ctx, g, r.Root, true, func(s string) error {
		if s == "union" {
			return errors.New("stop")
		}
		return nil
	}, "local")
	if err == nil {
		t.Fatal("fault missed")
	}
	abandoned, err := Abandon(ctx, g, r.Root, result.TransactionID)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if !abandoned.ExcludeRewritten || len(abandoned.Warnings) != 1 || !strings.Contains(abandoned.Warnings[0], "/local") {
		t.Fatalf("abandon didn't name the un-ignored path or the rewrite: %+v", abandoned)
	}
}
