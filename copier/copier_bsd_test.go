//go:build netbsd || freebsd || darwin

package copier

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// assertCtimeMatches asserts that fi1 and fi2 have the same ctime.
func assertCtimeMatches(t *testing.T, fi1, fi2 os.FileInfo) {
	t.Helper()
	st1 := fi1.Sys().(*syscall.Stat_t)
	st2 := fi2.Sys().(*syscall.Stat_t)
	assert.Equal(t, st1.Ctimespec, st2.Ctimespec)
}
