package reshuffle

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/platforms"
	cp "github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/directory"
	dockerArchive "github.com/containers/image/v5/docker/archive"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/pkg/blobinfocache/memory"
	"github.com/containers/image/v5/pkg/compression"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/archive"
	digest "github.com/opencontainers/go-digest"
	imgspec "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReshuffle(t *testing.T) {
	testStartedAt := time.Now().Truncate(time.Second)
	longerAgo := time.Unix(1485449953, 0)
	longAgo := time.Unix(1509548487, 0)

	layerSpecs := [][]tar.Header{
		// basic layer
		{
			{
				Name:     "bin/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			},
			{
				Name:     "bin/a",
				Typeflag: tar.TypeReg,
				Size:     48,
				Mode:     0o644,
			},
			{
				Name:     "bin/b",
				Typeflag: tar.TypeReg,
				Size:     47,
				Mode:     0o644,
			},
			{
				Name:     "etc/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			},
			{
				Name:     "etc/a",
				Typeflag: tar.TypeReg,
				Size:     46,
				Mode:     0o644,
			},
			{
				Name:     "etc/b",
				Typeflag: tar.TypeReg,
				Size:     45,
				Mode:     0o644,
			},
		},
		// layer that changes what the previous layer put down
		{
			{
				Name:     "bin/",
				Typeflag: tar.TypeDir,
				Mode:     0o775,
			},
			{
				Name:     "bin/" + archive.WhiteoutPrefix + "a", // remove "bin/a"
				Typeflag: tar.TypeReg,
			},
			{
				Name:     "bin/b", // overwrite "bin/b"
				Typeflag: tar.TypeReg,
				Size:     44,
				Mode:     0o640,
			},
			{
				Name:     "bin/c", // add "bin/c"
				Typeflag: tar.TypeReg,
				Size:     43,
				Mode:     0o600,
			},
			{
				Name:     "etc/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			},
			{
				Name:     "etc/" + archive.WhiteoutOpaqueDir, // remove "etc/a" and "etc/b"
				Typeflag: tar.TypeReg,
				Size:     42,
				Mode:     0o644,
			},
			{
				Name:     "etc/c",
				Typeflag: tar.TypeReg,
				Size:     41,
				Mode:     0o644,
			},
		},
	}

	// options that put everything into a single layer
	defaultOptions := ReshuffleOptions{}

	// the list of paths we expect in a single-layer image
	defaultOutputPaths := [][]string{
		{
			"bin/",
			"bin/b",
			"bin/c",
			"etc/",
			"etc/c",
		},
	}

	// options that put bin/b in one layer, and everything else in an additional layer
	twoLayerOptions := ReshuffleOptions{Layers: []ReshuffleLayerInfo{{Contents: []string{"/bin/b", "bin"}}}}

	// the list of paths we expect in a two-layer image - note that the
	// parent directories are expected to be repeated if they contain an
	// entry for this layer
	twoLayerOutputPaths := [][]string{
		{
			"bin/",
			"bin/b",
		},
		{
			"bin/",
			"bin/c",
			"etc/",
			"etc/c",
		},
	}

	// cache the contents of randomly-generated files
	writtenItems := make(map[int][]byte)

	// writeItem writes an entry to the tar.Writer, adding hdr.Size bytes
	// of random data as "contents"
	writeItem := func(hdr *tar.Header, tw *tar.Writer) error {
		if hdr.ModTime.IsZero() {
			withNow := *hdr
			withNow.ModTime = time.Now()
			hdr = &withNow
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Size != 0 {
			var buf bytes.Buffer
			n, err := io.CopyN(tw, io.TeeReader(rand.Reader, &buf), hdr.Size)
			if err != nil {
				return fmt.Errorf("writing payload for file %q: %w", hdr.Name, err)
			}
			if n != hdr.Size {
				return fmt.Errorf("error writing %d bytes: wrote %d bytes", hdr.Size, n)
			}
			if buf.Len() > 0 {
				writtenItems[buf.Len()] = buf.Bytes()
			}
		}
		return nil
	}

	// build layer blobs in memory
	layers := make([]bytes.Buffer, len(layerSpecs))
	compressedLayers := make([]bytes.Buffer, len(layerSpecs))
	for i, specs := range layerSpecs {
		compressedLayer, err := compression.CompressStream(&compressedLayers[i], compression.Gzip, nil)
		require.NoError(t, err, "compressing layer")
		tw := tar.NewWriter(io.MultiWriter(&layers[i], compressedLayer))
		for _, hdr := range specs {
			require.NoError(t, writeItem(&hdr, tw))
		}
		require.NoError(t, tw.Close())
		require.NoError(t, compressedLayer.Close())
	}

	// build and serialize the image config
	imageConfig := imgspecv1.Image{
		Platform: platforms.DefaultSpec(),
		RootFS: imgspecv1.RootFS{
			Type: "layers",
		},
		History: []imgspecv1.History{
			{
				CreatedBy: "add first layer in /",
			},
			{
				CreatedBy: "add second layer in /",
			},
		},
	}
	for i := range layers {
		imageConfig.RootFS.DiffIDs = append(imageConfig.RootFS.DiffIDs, digest.Canonical.FromBytes(layers[i].Bytes()))
	}
	configBytes, err := json.Marshal(&imageConfig)
	assert.NoError(t, err)

	// build and serialize the image manifest
	imageManifest := imgspecv1.Manifest{
		Versioned: imgspec.Versioned{
			SchemaVersion: 2,
		},
		MediaType: imgspecv1.MediaTypeImageManifest,
		Config: imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageConfig,
			Digest:    digest.Canonical.FromBytes(configBytes),
			Size:      int64(len(configBytes)),
		},
	}
	compressedImageManifest := imageManifest
	for i := range imageConfig.RootFS.DiffIDs {
		imageManifest.Layers = append(imageManifest.Layers, imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageLayer,
			Digest:    imageConfig.RootFS.DiffIDs[i],
			Size:      int64(layers[i].Len()),
		})
		compressedImageManifest.Layers = append(compressedImageManifest.Layers, imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageLayerGzip,
			Digest:    digest.Canonical.FromBytes(compressedLayers[i].Bytes()),
			Size:      int64(compressedLayers[i].Len()),
		})
	}
	manifestBytes, err := json.Marshal(&imageManifest)
	assert.NoError(t, err)
	compressedManifestBytes, err := json.Marshal(&compressedImageManifest)
	assert.NoError(t, err)

	// build and serialize the image index for the OCI layout
	imageIndex := imgspecv1.Index{
		Versioned: imgspec.Versioned{
			SchemaVersion: 2,
		},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{{
			MediaType: imgspecv1.MediaTypeImageManifest,
			Digest:    digest.Canonical.FromBytes(manifestBytes),
			Size:      int64(len(manifestBytes)),
		}},
	}
	indexBytes, err := json.Marshal(&imageIndex)
	assert.NoError(t, err)
	imageIndex.Manifests[0] = imgspecv1.Descriptor{
		MediaType: imgspecv1.MediaTypeImageManifest,
		Digest:    digest.Canonical.FromBytes(compressedManifestBytes),
		Size:      int64(len(compressedManifestBytes)),
	}
	compressedIndexBytes, err := json.Marshal(&imageIndex)
	assert.NoError(t, err)

	// build and serialize the "yeah, this is an OCI layout" file's contents
	imageLayout := imgspecv1.ImageLayout{
		Version: imgspecv1.ImageLayoutVersion,
	}
	layoutBytes, err := json.Marshal(&imageLayout)
	assert.NoError(t, err)

	// write the OCI layout
	ociDirUncompressed := t.TempDir()
	ociDirCompressed := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(ociDirUncompressed, imgspecv1.ImageBlobsDir), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(ociDirCompressed, imgspecv1.ImageBlobsDir), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(ociDirUncompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String()), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(ociDirCompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String()), 0o755))
	for i := range layers {
		require.NoError(t, os.WriteFile(filepath.Join(ociDirUncompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String(), digest.Canonical.FromBytes(layers[i].Bytes()).Encoded()), layers[i].Bytes(), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(ociDirCompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String(), digest.Canonical.FromBytes(compressedLayers[i].Bytes()).Encoded()), compressedLayers[i].Bytes(), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(ociDirUncompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String(), digest.Canonical.FromBytes(configBytes).Encoded()), configBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirCompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String(), digest.Canonical.FromBytes(configBytes).Encoded()), configBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirUncompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String(), digest.Canonical.FromBytes(manifestBytes).Encoded()), manifestBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirCompressed, imgspecv1.ImageBlobsDir, digest.Canonical.String(), digest.Canonical.FromBytes(compressedManifestBytes).Encoded()), compressedManifestBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirUncompressed, imgspecv1.ImageIndexFile), indexBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirCompressed, imgspecv1.ImageIndexFile), compressedIndexBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirUncompressed, imgspecv1.ImageLayoutFile), layoutBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ociDirCompressed, imgspecv1.ImageLayoutFile), layoutBytes, 0o644))

	// build a reference for reading the OCI layout
	uncompressedImageReference, err := layout.ParseReference(ociDirUncompressed)
	require.NoErrorf(t, err, "parsing reference for oci:%q", ociDirUncompressed)
	compressedImageReference, err := layout.ParseReference(ociDirCompressed)
	require.NoErrorf(t, err, "parsing reference for oci:%q", ociDirCompressed)

	// use our testing policy and a lot of image library defaults
	ctx := context.Background()
	sys := &types.SystemContext{}
	policy, err := signature.NewPolicyFromFile(filepath.Join("..", "..", "tests", "policy.json"))
	require.NoError(t, err)
	policyContext, err := signature.NewPolicyContext(policy)
	require.NoError(t, err)
	defer policyContext.Destroy()

	// requireLayers inspects an image and asserts that it has the
	// specified number of layers with the provided set of contents
	requireLayers := func(t *testing.T, ref types.ImageReference, expect int, contents [][]string, imageDate, contentsDate *time.Time, preservedHistory bool, createdBy []string) {
		// inspect the image to count the layers and get their diff
		// IDs, which should work as blob IDs since we didn't trigger
		// any compression
		img, err := ref.NewImage(ctx, sys)
		require.NoError(t, err)
		defer img.Close()
		inspectData, err := img.Inspect(ctx)
		require.NoError(t, err)
		require.NotNil(t, inspectData.Created)
		if imageDate != nil {
			assert.WithinRange(t, *inspectData.Created, imageDate.Add(-time.Second), imageDate.Add(time.Second), "image date not as expected")
		} else {
			assert.WithinRange(t, *inspectData.Created, testStartedAt, time.Now().Add(time.Second).Truncate(time.Second), "image date not as expected")
		}
		assert.Equal(t, expect, len(inspectData.Layers))
		// set up to walk the list of layers
		src, err := ref.NewImageSource(ctx, sys)
		require.NoError(t, err)
		defer src.Close()
		for i := range contents {
			// read the nth layer blob
			blobInfo := types.BlobInfo{
				Digest: inspectData.LayersData[i].Digest,
				Size:   inspectData.LayersData[i].Size,
			}
			blob, _, err := src.GetBlob(ctx, blobInfo, memory.New())
			require.NoError(t, err, "reading blob", i, blobInfo.Digest.String())
			defer blob.Close()
			// iterate through all of the entries and read their names
			tr := tar.NewReader(blob)
			var contentNames []string
			hdr, err := tr.Next()
			require.NoError(t, err)
			for hdr != nil {
				contentNames = append(contentNames, hdr.Name)
				hdr, err = tr.Next()
				if err != nil && errors.Is(err, io.EOF) {
					continue
				}
				require.NoError(t, err)
				if contentsDate != nil {
					assert.WithinRangef(t, hdr.ModTime, contentsDate.Add(-time.Second), contentsDate.Add(time.Second), "timestamp not set correctly on %q", hdr.Name)
				} else {
					now := time.Now()
					assert.WithinRange(t, hdr.ModTime, testStartedAt, now.Add(time.Second).Truncate(time.Second), "timestamp not set correctly on %q", hdr.Name)
				}
			}
			require.ErrorIs(t, err, io.EOF)
			// the list should match our expectations
			require.Equal(t, contents[i], contentNames)
		}
		ociConfig, err := img.OCIConfig(ctx)
		require.NoError(t, err)
		var ourHistoryStart int
		if preservedHistory {
			// history should be one for each layer, plus the two from the input image, which should be marked as empty
			ourHistoryStart = 2
		} else {
			// history should be 1:1 with layers
			ourHistoryStart = 0
		}
		assert.Equal(t, ourHistoryStart+len(inspectData.Layers), len(ociConfig.History), "number of history entries")
		for i := range ourHistoryStart {
			assert.Truef(t, ociConfig.History[i].EmptyLayer, "original history entry %d should now be marked empty", i)
		}
		for i, history := range ociConfig.History[ourHistoryStart:] {
			assert.Containsf(t, history.CreatedBy, createdBy[i], "history created-by %q should contain %q", history.CreatedBy, createdBy[i])
		}
	}

	// assertTypes inspects an image and asserts that it has the expected
	// manifest/config/image media type values
	assertTypes := func(t *testing.T, ref types.ImageReference, expectManifestType string, expectConfigType string, expectLayerType string) {
		// read parts of the image
		img, err := ref.NewImage(ctx, sys)
		require.NoError(t, err)
		defer img.Close()
		// parse the manifest to check its type
		manifestBytes, manifestType, err := img.Manifest(ctx)
		assert.Equal(t, expectManifestType, manifestType, "image manifest type was wrong")
		m, err := manifest.FromBlob(manifestBytes, manifestType)
		require.NoError(t, err)
		configInfo := m.ConfigInfo()
		assert.Equal(t, expectConfigType, configInfo.MediaType, "image config type was wrong")
		// inspect the image to check its layers' types
		inspectData, err := img.Inspect(ctx)
		require.NoError(t, err)
		assert.Greater(t, len(inspectData.LayersData), 0, "number of layers")
		for i := range len(inspectData.LayersData) {
			assert.Equalf(t, expectLayerType, inspectData.LayersData[i].MIMEType, "layer %d/%d had wrong MIME type", i+1, len(inspectData.LayersData))
		}
	}

	withLayerConfig := func(t *testing.T, imageReference types.ImageReference, fromOptions FromImageOptions, reshuffleOptions ReshuffleOptions, expectLayers int, expectPaths [][]string) {
		var createdBys []string
		for _, layer := range reshuffleOptions.Layers {
			createdBys = append(createdBys, layer.CreatedBy)
		}
		createdBys = append(createdBys, reshuffleOptions.FinalCreatedBy)
		t.Run("write", func(t *testing.T) {
			reshuffleOptions := reshuffleOptions

			// we're going to test writing to dir:, oci:, and docker-archive: locations
			dirDest, err := directory.NewReference(t.TempDir())
			require.NoError(t, err)

			dockerDest, err := dockerArchive.ParseReference(filepath.Join(t.TempDir(), "docker-archive"))
			require.NoError(t, err)

			ociDest, err := layout.ParseReference(t.TempDir())
			require.NoError(t, err)

			// test reading and writing using a Reshuffler
			reshuffler, err := FromImage(ctx, sys, imageReference, nil, fromOptions)
			require.NoError(t, err)

			_, _, err = reshuffler.WriteImage(ctx, dirDest, reshuffleOptions)
			assert.NoError(t, err)
			requireLayers(t, dirDest, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)

			reshuffleOptions.ForceManifestMIMEType = manifest.DockerV2Schema2MediaType
			_, _, err = reshuffler.WriteImage(ctx, dockerDest, reshuffleOptions)
			assert.NoError(t, err)
			requireLayers(t, dockerDest, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)

			reshuffleOptions.ForceManifestMIMEType = imgspecv1.MediaTypeImageManifest
			_, _, err = reshuffler.WriteImage(ctx, ociDest, reshuffleOptions)
			assert.NoError(t, err)
			requireLayers(t, ociDest, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)

			hdr, rc, err := reshuffler.Read(ctx, "/bin/b/")
			require.NoError(t, err)
			defer rc.Close()
			var buf bytes.Buffer
			io.Copy(&buf, rc)
			assert.Equal(t, int64(44), int64(hdr.Size), "reported length of a file in the image")
			assert.Equal(t, writtenItems[44], buf.Bytes(), "contents of a file in the image")
		})

		referenceWithTypes := func(t *testing.T, imageReference types.ImageReference, manifestType, configType, layerType string, reshuffleOptions ReshuffleOptions) {
			// we're going to test writing to dir:, oci:, and docker-archive: locations
			dirDest, err := directory.NewReference(t.TempDir())
			require.NoError(t, err)

			dockerDest, err := dockerArchive.ParseReference(filepath.Join(t.TempDir(), "docker-archive"))
			require.NoError(t, err)

			ociDest, err := layout.ParseReference(t.TempDir())
			require.NoError(t, err)

			// test reading using a reference filter
			cpOptions := cp.Options{
				DestinationCtx: &types.SystemContext{
					CompressionFormat:           nil,
					OCIAcceptUncompressedLayers: true,
				},
			}

			reshuffleOptions.ForceManifestMIMEType = manifestType
			src, err := ReferenceWithOptions(imageReference, reshuffleOptions)
			require.NoError(t, err)
			requireLayers(t, src, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)
			if manifestType != "" {
				assertTypes(t, src, manifestType, configType, layerType)
			}

			_, err = cp.Image(ctx, policyContext, dirDest, src, &cpOptions)
			require.NoError(t, err)
			requireLayers(t, dirDest, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)

			_, err = cp.Image(ctx, policyContext, dockerDest, src, &cpOptions)
			require.NoError(t, err)
			requireLayers(t, dockerDest, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)

			_, err = cp.Image(ctx, policyContext, ociDest, src, &cpOptions)
			require.NoError(t, err)
			requireLayers(t, ociDest, expectLayers, expectPaths, reshuffleOptions.ImageCreatedDate, reshuffleOptions.ContentCreatedDate, reshuffleOptions.PreserveHistory, createdBys)
		}

		t.Run("reference", func(t *testing.T) {
			t.Run("default", func(t *testing.T) {
				referenceWithTypes(t, imageReference, "", "", "", reshuffleOptions)
			})
			t.Run("oci", func(t *testing.T) {
				referenceWithTypes(t, imageReference, imgspecv1.MediaTypeImageManifest, imgspecv1.MediaTypeImageConfig, imgspecv1.MediaTypeImageLayer, reshuffleOptions)
			})
			t.Run("docker", func(t *testing.T) {
				referenceWithTypes(t, imageReference, manifest.DockerV2Schema2MediaType, manifest.DockerV2Schema2ConfigMediaType, manifest.DockerV2SchemaLayerMediaTypeUncompressed, reshuffleOptions)
			})
		})
	}

	withTemporaryDirectory := func(t *testing.T, imageReference types.ImageReference, blobCacheDirectory string) {
		fromImageOptions := FromImageOptions{
			BlobCacheDirectory: blobCacheDirectory,
		}
		reshuffleOptions := defaultOptions
		reshuffleOptions.BlobCacheDirectory = fromImageOptions.BlobCacheDirectory
		t.Run("one-layer", func(t *testing.T) {
			t.Run("default-dates", func(t *testing.T) {
				withLayerConfig(t, imageReference, fromImageOptions, reshuffleOptions, 1, defaultOutputPaths)
			})
			t.Run("image-date", func(t *testing.T) {
				reshuffleOptions.ImageCreatedDate = &longAgo
				withLayerConfig(t, imageReference, fromImageOptions, reshuffleOptions, 1, defaultOutputPaths)
			})
			t.Run("preserve-history", func(t *testing.T) {
				reshuffleOptions.PreserveHistory = true
				withLayerConfig(t, imageReference, fromImageOptions, reshuffleOptions, 1, defaultOutputPaths)
			})
		})
		reshuffleOptions = twoLayerOptions
		reshuffleOptions.BlobCacheDirectory = fromImageOptions.BlobCacheDirectory
		t.Run("two-layers", func(t *testing.T) {
			t.Run("image-date", func(t *testing.T) {
				reshuffleOptions.ImageCreatedDate = &longAgo
				withLayerConfig(t, imageReference, fromImageOptions, reshuffleOptions, 2, twoLayerOutputPaths)
			})
			t.Run("both-dates", func(t *testing.T) {
				reshuffleOptions.ContentCreatedDate = &longerAgo
				withLayerConfig(t, imageReference, fromImageOptions, reshuffleOptions, 2, twoLayerOutputPaths)
			})
			t.Run("preserve-history", func(t *testing.T) {
				reshuffleOptions.PreserveHistory = true
				withLayerConfig(t, imageReference, fromImageOptions, reshuffleOptions, 2, twoLayerOutputPaths)
			})
		})
	}

	withReference := func(t *testing.T, imageReference types.ImageReference) {
		t.Run("with-temp", func(t *testing.T) {
			withTemporaryDirectory(t, imageReference, t.TempDir())
		})
		t.Run("without-temp", func(t *testing.T) {
			withTemporaryDirectory(t, imageReference, "")
		})
	}

	t.Run("compressed", func(t *testing.T) {
		withReference(t, compressedImageReference)
	})
	t.Run("uncompressed", func(t *testing.T) {
		withReference(t, uncompressedImageReference)
	})
}
