package teardown_test

import (
	"os"
	"testing"

	"gitlocal/internal/testutil"
)

// GL-132: deinit's end-to-end tests run a second time on the reftable ref
// backend.
func TestMain(m *testing.M) { os.Exit(testutil.MainWithReftable(m, false)) }

// The fixtures use the ref backend of the current pass, so the reftable pass
// can't silently test the files backend again.
func TestFixturesUseTheRefFormatOfThePass(t *testing.T) {
	t.Parallel()
	testutil.CheckFixtureRefFormats(t)
}
