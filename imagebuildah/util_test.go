package imagebuildah

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"go.podman.io/buildah/define"
)

func TestGetFromAndSourceKeysFromMountFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mount    string
		expected mountInfo
	}{
		{
			name:     "type omitted defaults to bind",
			mount:    "--mount=src=/foo,target=/bar",
			expected: mountInfo{Type: define.TypeBind, Source: "/foo"},
		},
		{
			name:     "explicit bind type",
			mount:    "--mount=type=bind,src=/foo,target=/bar",
			expected: mountInfo{Type: "bind", Source: "/foo"},
		},
		{
			name:     "cache type is not treated as bind",
			mount:    "--mount=type=cache,target=/bar",
			expected: mountInfo{Type: "cache"},
		},
		{
			name:     "tmpfs type is not treated as bind",
			mount:    "--mount=type=tmpfs,target=/bar",
			expected: mountInfo{Type: "tmpfs"},
		},
		{
			name:     "from without type defaults to bind",
			mount:    "--mount=from=builder,src=/foo,target=/bar",
			expected: mountInfo{Type: define.TypeBind, Source: "/foo", From: "builder"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, getFromAndSourceKeysFromMountFlag(tt.mount))
		})
	}
}

func TestGeneratePathChecksum(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	tempFile, err := os.CreateTemp(tempDir, "testfile")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer tempFile.Close()

	// Write some data to the file
	data := []byte("Hello, world!")
	if _, err := tempFile.Write(data); err != nil {
		t.Fatalf("Failed to write data to temp file: %v", err)
	}

	// Generate the checksum for the directory
	checksum, err := generatePathChecksum(tempDir)
	if err != nil {
		t.Fatalf("Failed to generate checksum: %v", err)
	}

	digester := digest.SHA256.Digester()
	tarWriter := tar.NewWriter(digester.Hash())

	err = filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		// Zero out timestamp fields to match the modified generatePathChecksum function
		header.ModTime = time.Time{}
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tarWriter, file)
		return err
	})
	if err != nil {
		tarWriter.Close()
		t.Fatalf("Failed to manually generate checksum: %v", err)
	}

	tarWriter.Close()
	expectedChecksum := digester.Digest().String()

	// Compare the generated checksum to the expected checksum
	assert.Equal(t, expectedChecksum, checksum, "didn't get expected checksum over a sample directory")
}
