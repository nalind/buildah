//go:build !seccomp || !linux

package buildah

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupSeccompUnsupported(t *testing.T) {
	tests := []struct {
		name        string
		profilePath string
		expectError bool
	}{
		{name: "default", profilePath: ""},
		{name: "unconfined", profilePath: "unconfined"},
		{name: "explicit profile", profilePath: "/tmp/seccomp.json", expectError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := &specs.Spec{Linux: &specs.Linux{Seccomp: &specs.LinuxSeccomp{}}}
			err := setupSeccomp(spec, test.profilePath)
			if test.expectError {
				require.ErrorContains(t, err, "seccomp support is not enabled in this build")
				return
			}
			require.NoError(t, err)
			assert.Nil(t, spec.Linux.Seccomp)
		})
	}
}
