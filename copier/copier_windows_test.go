//go:build windows

package copier

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func checkStatInfoOwnership(t *testing.T, result *StatForItem) {
	t.Helper()
	require.EqualValues(t, -1, result.UID, "expected the owning user to not be supported")
	require.EqualValues(t, -1, result.GID, "expected the owning group to not be supported")
}

// assertCtimeMatches asserts that fi1 and fi2 have the same ctime.
func assertCtimeMatches(t *testing.T, fi1, fi2 os.FileInfo) {
	// We don’t know, so don’t fail.
}
