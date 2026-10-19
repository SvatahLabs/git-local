package teardown_test

import (
	"os"
	"testing"

	"gitlocal/internal/testutil"
)

// GL-132: deinit's end-to-end tests run a second time on the reftable ref
// backend.
func TestMain(m *testing.M) { os.Exit(testutil.MainWithReftable(m, false)) }
