package cli

import (
	"os"
	"testing"

	"github.com/SvatahLabs/git-local/internal/testutil"
)

// GL-132: the package's tests run a second time on the reftable ref backend.
// A child process for callCLI runs its command once.
func TestMain(m *testing.M) {
	_, child := os.LookupEnv(childArgs)
	os.Exit(testutil.MainWithReftable(m, child))
}

// The fixtures use the ref backend of the current pass, so the reftable pass
// can't silently test the files backend again.
func TestFixturesUseTheRefFormatOfThePass(t *testing.T) {
	t.Parallel()
	testutil.CheckFixtureRefFormats(t)
}
