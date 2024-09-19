package reshuffle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containers/buildah/define"
	"github.com/containers/buildah/docker"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/pkg/compression"
	storageTransport "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/ioutils"
	digest "github.com/opencontainers/go-digest"
	imgspec "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
)

// Reshuffler encompasses assorted transformations to how an image's contents
// are distributed through its layers.  If multiple images will be written, or
// the list of contents needs to be retrieved, it is more efficient to use a
// Reshuffler.
type Reshuffler interface {
	Close() error
	// Paths returns a list of all of the items which can be placed into layers, and FileInfo for them.
	Paths() ([]string, map[string]fs.FileInfo, error)
	// Read returns a seekable reader for a particular item in the image which must be closed.
	Read(ctx context.Context, pathSpec string) (*tar.Header, *SectionReadCloser, error)
	// WriteImage writes a new image to the destination reference,
	// returning the digest of its manifest.  If the reference is `nil`, an
	// image will be created in local storage.
	WriteImage(ctx context.Context, dest types.ImageReference, options ReshuffleOptions) ([]byte, digest.Digest, error)
	// writeImageOrSource writes to "dest", or creates a SourceImage in
	// "src" named "ref".  If none of the values are set, an unnamed image
	// will be created in local storage.
	writeImageOrSource(ctx context.Context, dest types.ImageReference, ref types.ImageReference, src **reshuffleSource, options ReshuffleOptions) ([]byte, digest.Digest, error)
}

// SectionReadCloser encapsulates a SectionReader that expects to be closed.
type SectionReadCloser struct {
	*io.SectionReader
	io.Closer
}

// ReshuffleOptions affects how an image that we rework should look when we
// write the new version.
type ReshuffleOptions struct {
	BlobCacheDirectory string               `json:"directory,omitempty"`       // a parent directory for temporary copies of blobs
	Layers             []ReshuffleLayerInfo `json:"layers,omitempty"`          // the nth item in the slice is the nth layer
	FinalCreatedBy     string               `json:"finalCreatedBy,omitempty"`  // information to include in the history entry for the final layer, if one needs to be added, corresponding to the ReshuffleLayerInfo.CreatedBy that could have described it
	RemoveFromLayers   []string             `json:"remove,omitempty"`          // each item in the slice is an item to omit entirely
	ImageCreatedDate   *time.Time           `json:"imageDate,omitempty"`       // timestamp for the new image and history items
	ContentCreatedDate *time.Time           `json:"contentDate,omitempty"`     // timestamp to force for items in layers
	PreserveHistory    bool                 `json:"preserveHistory,omitEmpty"` // append to original history (with EmptyLayer forced to true) instead of clearing it first

	ForceManifestMIMEType string `json:"forceMediaType"` // type of image to attempt to force conversion to, either define.OCIv1ImageManifest, define.OCI, define.Dockerv2ImageManifest, or define.DOCKER
}

// ReshuffleLayerInfo describes the contents of one layer in an image that
// we'll be writing.
type ReshuffleLayerInfo struct {
	CreatedBy string   // information to include in the history entry for the layer
	Contents  []string // each item is the pathname of a single item to include in the layer
}

// readCloserAt is a ReaderAt (which is used to create SectionReaders) that
// can/must also be Closed.  If GetBlob() returns an uncompressed blob which
// implements this interface, then we don't need to cache the blob's contents
// before using it to construct layers for the shuffled image, and that reduces
// the disk space we need by at least a half.
type readCloserAt interface {
	io.ReaderAt
	io.Closer
}

type reshuffler struct {
	imageSource        types.ImageSource           // possibly an open copy of the image reference the reshuffler was created from
	archiveReader      io.Reader                   // possibly an open copy of the archive file the reshuffler was created from
	blobInfoCache      types.BlobInfoCache         // used in conjunction with imageSource
	blobCacheDirectory string                      // directory which holds cached copies of the original layer blobs, if the originals couldn't be used as an uncompressed readCloserAt
	configType         string                      // cached from the original image's manifest, if there is one, or OCI
	configBlob         []byte                      // cached from the original image, if there is one, or an empty default
	diffIDs            []digest.Digest             // layer diffIDs (uncompressed digests), in order of application
	diffs              map[digest.Digest]*diffInfo // layer diffIDs (uncompressed digests) to contents
	rootfs             tree                        // composed rootfs
}

type diffInfo struct {
	uncompressedDigest digest.Digest   // digest of this diff in its entirety
	path               string          // pathname of cached copy of the diff
	paths              []*pathLocation // all of the entries in the diff
}

func (r *reshuffler) Close() error {
	if r.blobCacheDirectory != "" {
		return os.RemoveAll(r.blobCacheDirectory)
	}
	return nil
}

// FromArchiveOptions is the set of options which controls details of parsing
// an archived rootfs.
type FromArchiveOptions struct {
	BlobCacheDirectory string
}

// Create a minimal image from an archived rootfs.  The returned object
// implements the Reshuffler interface and must be closed to free up temporary
// files.
func FromArchive(ctx context.Context, sys *types.SystemContext, reader io.Reader, canReuseBlob bool, options FromArchiveOptions) (Reshuffler, error) {
	blobCacheParentDirectory := options.BlobCacheDirectory
	if blobCacheParentDirectory == "" {
		blobCacheParentDirectory = sys.BigFilesTemporaryDir
	}
	blobCacheDirectory, err := os.MkdirTemp(blobCacheParentDirectory, "blobcache")
	if err != nil {
		return nil, err
	}
	reshuffler := reshuffler{
		blobCacheDirectory: options.BlobCacheDirectory,
		configType:         imgspecv1.MediaTypeImageConfig,
		configBlob:         []byte("{}"),
		diffs:              map[digest.Digest]*diffInfo{},
	}
	diffInfo := diffInfo{}
	willReuseBlob, err := parseArchive(ctx, reader, canReuseBlob, blobCacheDirectory, &diffInfo)
	if err != nil {
		return nil, err
	}
	reshuffler.diffIDs = []digest.Digest{diffInfo.uncompressedDigest}
	reshuffler.diffs[diffInfo.uncompressedDigest] = &diffInfo
	reshuffler.rootfs.applyDiff(&diffInfo)
	if willReuseBlob {
		reshuffler.archiveReader = reader
	}
	return &reshuffler, nil
}

// FromImageOptions is the set of options which controls details of parsing an
// image's rootfs.
type FromImageOptions struct {
	BlobCacheDirectory string
}

// Create an image reshuffler from an image.  The returned object implements
// the Reshuffler interface and must be closed to free up temporary files.
func FromImage(ctx context.Context, sys *types.SystemContext, ref types.ImageReference, instanceDigest *digest.Digest, options FromImageOptions) (Reshuffler, error) {
	if sys == nil {
		sys = &types.SystemContext{}
	}

	blobInfoCache := blobinfocache.DefaultCache(sys)

	// Create a temporary directory in which we might need to store some
	// uncompressed copies of blobs.
	blobCacheParentDirectory := options.BlobCacheDirectory
	if blobCacheParentDirectory == "" {
		blobCacheParentDirectory = sys.BigFilesTemporaryDir
	}
	blobCacheDirectory, err := os.MkdirTemp(blobCacheParentDirectory, "blobcache")
	if err != nil {
		return nil, fmt.Errorf("creating a temporary directory: %w", err)
	}

	// Read the manifest, and decode enough of it to find the config blob.
	// We don't really need the rest at this point.
	source, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		return nil, fmt.Errorf("opening image at %q for reading: %w", transports.ImageName(ref), err)
	}
	sourceToClose := io.Closer(source)
	defer func() {
		if sourceToClose != nil {
			if err = sourceToClose.Close(); err != nil {
				logrus.Warnf("closing image %q: %v", transports.ImageName(ref), err)
			}
		}
	}()

	manifestBytes, manifestType, err := source.GetManifest(ctx, instanceDigest)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	if manifest.MIMETypeIsMultiImage(manifestType) {
		imageIndex, err := manifest.ListFromBlob(manifestBytes, manifestType)
		if err != nil {
			return nil, fmt.Errorf("parsing list manifest: %w", err)
		}
		instance, err := imageIndex.ChooseInstance(sys)
		if err != nil {
			return nil, fmt.Errorf("choosing suitable entry in list manifest: %w", err)
		}
		manifestBytes, manifestType, err = source.GetManifest(ctx, &instance)
		if err != nil {
			return nil, fmt.Errorf("reading manifest: %w", err)
		}
	}
	imageManifest, err := manifest.FromBlob(manifestBytes, manifestType)
	if err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	var decodedManifest struct {
		imgspec.Versioned
		Config imgspecv1.Descriptor `json:"config"`
	}
	if err := json.Unmarshal(manifestBytes, &decodedManifest); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}

	// Read the config blob, and decode the rootfs bits.  We can almost get
	// away with not caring about anything else, but hang on to it, since
	// we want to keep the image config settings for later.
	if decodedManifest.Config.Size == 0 {
		return nil, errors.New("zero-length image configuration found")
	}
	var config struct {
		RootFS imgspecv1.RootFS `json:"rootfs"`
	}
	var configBlob []byte
	if len(decodedManifest.Config.Data) == 0 {
		configInfo := types.BlobInfo{
			Digest: decodedManifest.Config.Digest,
		}
		rc, _, err := source.GetBlob(ctx, configInfo, blobInfoCache)
		if err != nil {
			return nil, fmt.Errorf("reading the image's configuration: %w", err)
		}
		configBlob, err = io.ReadAll(rc)
		closeErr := rc.Close()
		if err != nil {
			return nil, fmt.Errorf("reading image configuration: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing image configuration: %w", closeErr)
		}
	} else {
		configBlob = decodedManifest.Config.Data
	}
	if err = json.Unmarshal(configBlob, &config); err != nil {
		return nil, fmt.Errorf("decoding inlined image configuration: %w", err)
	}

	// We now have the beginnings of our structure.
	reshuffler := reshuffler{
		blobInfoCache:      blobInfoCache,
		blobCacheDirectory: blobCacheDirectory,
		configType:         decodedManifest.Config.MediaType,
		configBlob:         configBlob,
		diffIDs:            slices.Clone(config.RootFS.DiffIDs),
		diffs:              map[digest.Digest]*diffInfo{},
	}

	// Start ingesting layer blobs, one at a time.
	blobInfos, err := source.LayerInfosForCopy(ctx, instanceDigest)
	if err != nil {
		return nil, fmt.Errorf("reading layer info: %w", err)
	}
	if len(blobInfos) == 0 {
		layerInfos := imageManifest.LayerInfos()
		for _, layerInfo := range layerInfos {
			blobInfos = append(blobInfos, layerInfo.BlobInfo)
		}
	}
	seenBlobs := make(map[digest.Digest]struct{})
	keepImageSourceOpen := false // don't keep the source image open unless it's useful to
	for _, blobInfo := range blobInfos {
		if _, ok := seenBlobs[blobInfo.Digest]; ok {
			// it's a repeated blob, so no need to parse it again
			continue
		}
		seenBlobs[blobInfo.Digest] = struct{}{}
		diffInfo, canKeepThisLayerOpen, err := getAndParseBlob(ctx, source, blobInfo, blobInfoCache, blobCacheDirectory)
		if err != nil {
			return nil, fmt.Errorf("ingesting layer: %w", err)
		}
		reshuffler.diffs[diffInfo.uncompressedDigest] = diffInfo
		reshuffler.rootfs.applyDiff(diffInfo)
		keepImageSourceOpen = keepImageSourceOpen || canKeepThisLayerOpen
	}

	if keepImageSourceOpen {
		reshuffler.imageSource = source
		sourceToClose = nil
	}

	// All done!
	return &reshuffler, nil
}

func getAndParseBlob(ctx context.Context, img types.ImageSource, layerInfo types.BlobInfo, blobInfoCache types.BlobInfoCache, blobCacheDirectory string) (*diffInfo, bool, error) {
	// Start reading the blob.
	rc, _, err := img.GetBlob(ctx, layerInfo, blobInfoCache)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			logrus.Warnf("closing blob with digest %q from %q: %v", layerInfo.Digest, transports.ImageName(img.Reference()), err)
		}
	}()

	// Parse its contents.
	diffInfo := diffInfo{}
	keepImageOpen, err := parseArchive(ctx, rc, true, blobCacheDirectory, &diffInfo)
	if err != nil {
		return nil, false, fmt.Errorf("parsing blob with digest %v: %w", layerInfo.Digest, err)
	}
	return &diffInfo, keepImageOpen, nil
}

func parseArchive(_ context.Context, reader io.Reader, canReuseBlob bool, blobCacheDirectory string, diffInfo *diffInfo) (bool, error) {
	// Start to decompress it, if we need to.
	decompressed, wasCompressed, err := compression.AutoDecompress(reader)
	if err != nil {
		return false, fmt.Errorf("decompressing blob: %w", err)
	}
	defer func() {
		if err := decompressed.Close(); err != nil {
			logrus.Warnf("closing decompressed blob: %v", err)
		}
	}()

	// Set up to digest the uncompressed data we're reading back.
	uncompressedDigester := digest.Canonical.Digester()
	uncompressedOffset := ioutils.NewWriteCounter(uncompressedDigester.Hash())
	uncompressed := io.TeeReader(decompressed, uncompressedOffset)

	// If we can just reuse the blob in the form it started in, we don't have to cache a copy.
	if _, ok := reader.(readCloserAt); wasCompressed || !ok {
		canReuseBlob = false
	}

	// Start writing a copy of the uncompressed data to a file.
	var cacheFile *os.File
	if !canReuseBlob {
		cacheFile, err = os.CreateTemp(blobCacheDirectory, "blob")
		if err != nil {
			return false, fmt.Errorf("creating cached blob file: %w", err)
		}
		uncompressed = io.TeeReader(uncompressed, cacheFile)
	}

	// Now walk the layer contents.
	tr := tar.NewReader(uncompressed)
	hdr, err := tr.Next()
	for hdr != nil {
		// Log the offset of this item's contents in the uncompressed
		// stream, and the digest of its contents.
		var contentDigest digest.Digest
		offset := uncompressedOffset.Count
		if hdr.Typeflag == tar.TypeReg {
			digester := digest.Canonical.Digester()
			n, err := io.Copy(digester.Hash(), tr)
			if err != nil && !errors.Is(err, io.EOF) {
				return false, fmt.Errorf("reading contents of entry for %q: %w", hdr.Name, err)
			}
			if n != hdr.Size {
				return false, fmt.Errorf("digesting contents of entry for %q: read %d bytes instead of %d bytes", hdr.Name, n, hdr.Size)
			}
			contentDigest = digester.Digest()
		}
		pathLocation := newPathLocation(hdr, diffInfo, offset, contentDigest)
		diffInfo.paths = append(diffInfo.paths, pathLocation)
		hdr, err = tr.Next()
	}

	// If we hit a weird error parsing the uncompressed data, bail.
	if err != nil && !errors.Is(err, io.EOF) {
		if cacheFile != nil {
			cacheName := cacheFile.Name()
			cacheFile.Close()
			if err := os.Remove(cacheName); err != nil {
				logrus.Warnf("removing %q: %v", cacheName, err)
			}
		}
		return false, fmt.Errorf("reading blob: %w", err)
	}

	// Flush any trailing data.
	if _, err = io.Copy(io.Discard, uncompressed); err != nil && !errors.Is(err, io.EOF) {
		if cacheFile != nil {
			cacheName := cacheFile.Name()
			cacheFile.Close()
			if err := os.Remove(cacheName); err != nil {
				logrus.Warnf("removing %q: %v", cacheName, err)
			}
		}
		return false, fmt.Errorf("reading blob trailer: %w", err)
	}

	// Save the uncompressed digest.
	diffInfo.uncompressedDigest = uncompressedDigester.Digest()

	// Rename the cache file into place.
	if cacheFile != nil {
		cacheName := cacheFile.Name()
		cacheFile.Close()
		newName := filepath.Join(blobCacheDirectory, diffInfo.uncompressedDigest.Encoded())
		if err = os.Rename(cacheName, newName); err != nil {
			return false, fmt.Errorf("saving cached copy of blob: %w", err)
		}
		diffInfo.path = newName
	}

	return diffInfo.path == "", nil
}

// Paths returns the paths of all of the contents of the image, for consumers
// to use when deciding which content to put into which layers.
func (r *reshuffler) Paths() ([]string, map[string]fs.FileInfo, error) {
	return r.rootfs.paths()
}

// Read returns a seekable reader for a particular item in the image which must be closed.
func (r *reshuffler) Read(ctx context.Context, pathSpec string) (*tar.Header, *SectionReadCloser, error) {
	entries, _, err := r.rootfs.entries()
	if err != nil {
		return nil, nil, fmt.Errorf("cataloging contents of image: %w", err)
	}
	path := pathSpec
	if len(path) > 0 {
		path = strings.TrimSuffix(path, "/")
	}
	if len(path) > 0 {
		path = strings.TrimPrefix(path, "/")
	}
	entry, ok := entries[path]
	if !ok {
		entry, ok = entries["/"+path]
	}
	if !ok {
		entry, ok = entries["./"+path]
	}
	if !ok {
		entry, ok = entries[path+"/"]
	}
	if !ok {
		entry, ok = entries["/"+path+"/"]
	}
	if !ok {
		entry, ok = entries["./"+path+"/"]
	}
	if entry == nil {
		return nil, nil, fmt.Errorf("could not find entry in image for %q: %w", path, syscall.ENOENT)
	}
	if entry.self == nil || entry.self.layerDiffInfo == nil {
		return nil, nil, fmt.Errorf("no entry in image for %q: %w", path, syscall.ENOENT)
	}
	if entry.self.layerDiffInfo == nil {
		return nil, nil, fmt.Errorf("no entry in image for %q: %w", path, syscall.ENOENT)
	}
	hdr := entry.self.header("")
	var rca readCloserAt
	if entry.self.layerDiffInfo.path != "" {
		f, err := os.Open(entry.self.layerDiffInfo.path)
		if err != nil {
			return nil, nil, fmt.Errorf("opening diff to read %q: %w", path, err)
		}
		rca = f
	} else {
		blobInfo := types.BlobInfo{
			Digest: entry.self.layerDiffInfo.uncompressedDigest,
		}
		rc, _, err := r.imageSource.GetBlob(ctx, blobInfo, r.blobInfoCache)
		if err != nil {
			return nil, nil, fmt.Errorf("reusing contents of blob with digest %q: %w", blobInfo.Digest, err)
		}
		rca, ok = rc.(readCloserAt)
		if !ok {
			return nil, nil, fmt.Errorf("unexpected error reusing contents of blob with digest %q", blobInfo.Digest)
		}
	}
	readCloserAt := SectionReadCloser{
		SectionReader: io.NewSectionReader(rca, entry.self.layerUncompressedOffset, hdr.Size),
		Closer:        rca,
	}
	return &hdr, &readCloserAt, nil
}

// WriteImage writes a modified version of the original image, preserving most
// of the configuration, replacing its history and rootfs with each layer
// containing the paths from each element of the `layers` slice, with one final
// layer added to include any left overs.
// The parent directories of the items in a layer are always included in that
// layer, so some duplication is still possible across the image as a whole.
func (r *reshuffler) WriteImage(ctx context.Context, dest types.ImageReference, options ReshuffleOptions) ([]byte, digest.Digest, error) {
	return r.writeImageOrSource(ctx, dest, nil, nil, options)
}

// writeImageOrSource writes to "dest", or creates a SourceImage in "src" named
// "ref".  If none of the values are set, an unnamed image will be created in
// local storage.
func (r *reshuffler) writeImageOrSource(ctx context.Context, dest types.ImageReference, ref types.ImageReference, src **reshuffleSource, options ReshuffleOptions) ([]byte, digest.Digest, error) {
	// Get a map from all of the paths in the image to where their contents can be found.
	entries, linkGroups, err := r.rootfs.entries()
	if err != nil {
		return nil, "", fmt.Errorf("cataloging contents of image: %w", err)
	}

	// If the names of items to put into layers were ultimately specified
	// by people, they may not _exactly_ match items returned by Paths().
	// Try to accommodate that.
	findEntryName := func(name string) string {
		if _, found := entries[name]; !found {
			if _, found := entries[name+"/"]; found {
				name += "/"
			} else {
				stripped := strings.TrimPrefix(name, "/")
				if _, found := entries[stripped]; found {
					name = stripped
				} else if _, found := entries[stripped+"/"]; found {
					name = stripped + "/"
				} else {
					if _, found := entries["./"+stripped]; found {
						name = "./" + stripped
					} else if _, found := entries["./"+stripped+"/"]; found {
						name = "./" + stripped + "/"
					}
				}
			}
		}
		return name
	}

	// Make a map of input blobs (either coming directly from the original
	// image, or our cached copies) so that we don't try to open any
	// particular one of them more than once.
	readers := make(map[digest.Digest]readCloserAt)

	// Make a map to track which items we haven't written to any layer yet.
	left := make(map[string]struct{})
	for pathname := range entries {
		left[pathname] = struct{}{}
	}

	// Prune out anything we want to explicitly omit from the image.
	for _, toRemove := range options.RemoveFromLayers {
		delete(left, findEntryName(toRemove))
	}

	// Create a directory for holding the new diffs.
	blobCacheDirectory := filepath.Dir(r.blobCacheDirectory)
	if options.BlobCacheDirectory != "" {
		blobCacheDirectory = options.BlobCacheDirectory
	}
	var dir string
	if ref != nil && src != nil {
		// Create a temporary directory in a location that will be
		// cleaned up with the image source.
		if dir, err = os.MkdirTemp(blobCacheDirectory, "reshuffle"); err != nil {
			return nil, "", fmt.Errorf("creating temporary directory: %w", err)
		}
	} else {
		// We'll be writing the image ourselves, so we can and should
		// clean up the temporary files right away.
		if dir, err = os.MkdirTemp(blobCacheDirectory, "reshuffle"); err != nil {
			return nil, "", fmt.Errorf("creating temporary directory: %w", err)
		}
	}

	// And at last, write the specified layer diffs.
	layerFiles := make([]string, len(options.Layers)+1)
	diffIDs := make([]digest.Digest, len(options.Layers)+1)
	diffSizes := make([]int64, len(options.Layers)+1)
	createdBys := make([]string, len(options.Layers)+1)
	writeErrors := make([]error, len(options.Layers)+1)
	writeLayerFile := func(layerIndex int, layerContents []string) (string, digest.Digest, int64, string, error) {
		// Construct a queue of items to write.
		type queuedHeader struct {
			hdr         tar.Header
			path        string        // only relevant for hdr.Typeflag == TypeReg
			layerDigest digest.Digest // only relevant for hdr.Typeflag == TypeReg
			offset      int64         // only relevant for hdr.Typeflag == TypeReg
			digest      digest.Digest // only relevant for hdr.Typeflag == TypeReg
			tree        *tree
		}
		var queue []queuedHeader
		queuedDirectories := make(map[string]struct{})

		// Walk the list of items to include in this layer.
		queueContents := slices.Clone(layerContents)
		sort.Strings(queueContents)
		queuedLinkGroups := make(map[*tree]struct{})
		for len(queueContents) > 0 {
			var queueContent string
			queueContent, queueContents = queueContents[0], queueContents[1:]
			queueContent = findEntryName(queueContent)
			// Queue any leading directories of this item that
			// haven't already been queued in this layer.
			component, rest, hasParent := strings.Cut(queueContent, "/")
			directory := ""
			for hasParent && rest != "" {
				directory += component + "/"
				if _, queuedDirectory := queuedDirectories[directory]; !queuedDirectory {
					entry, foundEntry := entries[directory]
					if foundEntry {
						hdr := entry.self.header(directory)
						queue = append(queue, queuedHeader{
							hdr:         hdr,
							path:        entry.self.layerDiffInfo.path,
							layerDigest: entry.self.layerDiffInfo.uncompressedDigest,
							offset:      entry.self.layerUncompressedOffset,
							digest:      entry.self.digest,
							tree:        entry,
						})
					}
					queuedDirectories[directory] = struct{}{}
				}
				component, rest, hasParent = strings.Cut(rest, "/")
			}
			// Work on the entry itself.
			if entry, foundEntry := entries[queueContent]; foundEntry {
				// If it's one of multiple links to an item,
				// add everything that's hard-linked to it to
				// this queue to be written, just one time.
				if entry.nlink > 1 {
					if _, queuedLinkGroup := queuedLinkGroups[entry]; !queuedLinkGroup {
						if linkGroup, ok := linkGroups[entry]; ok {
							// Make sure that items that are hard-linked to this one
							// also get queued for this layer.
							queueContents = append(queueContents, linkGroup...)
						}
						queuedLinkGroups[entry] = struct{}{}
					}
				}
				// Queue the entry itself.
				hdr := entry.self.header(queueContent)
				queue = append(queue, queuedHeader{
					hdr:         hdr,
					path:        entry.self.layerDiffInfo.path,
					layerDigest: entry.self.layerDiffInfo.uncompressedDigest,
					offset:      entry.self.layerUncompressedOffset,
					digest:      entry.self.digest,
					tree:        entry,
				})
				// Make sure that we don't queue this directory
				// again in this layer.
				if entry.self.typeFlag == tar.TypeDir {
					queuedDirectories[queueContent] = struct{}{}
				}
			}
		}

		// Sort the list of items that we plan to write.
		sort.Slice(queue, func(i, j int) bool {
			return strings.Compare(queue[i].hdr.Name, queue[j].hdr.Name) < 0
		})

		// Start to write this diff file.
		f, err := os.Create(filepath.Join(dir, strconv.Itoa(layerIndex)))
		if err != nil {
			return "", "", -1, "", err
		}
		defer f.Close()
		digester := digest.Canonical.Digester()
		tw := tar.NewWriter(io.MultiWriter(f, digester.Hash()))

		// Write the items.
		written := make(map[string]struct{})
		linkTargets := make(map[*tree]string)
		for _, item := range queue {
			// If this item has already appeared in another layer,
			// we don't need to write it again unless it's a
			// directory, which we should write anyway because
			// there might be something in that directory later in
			// the queue.
			if _, written := left[item.hdr.Name]; !written && item.hdr.Typeflag != tar.TypeDir {
				continue
			}
			delete(left, item.hdr.Name)
			// Hard links provide us with at least one duplicate
			// entry in this layer.
			if _, alreadyWrote := written[item.hdr.Name]; alreadyWrote {
				continue
			}
			// If this item has multiple links, make every one
			// after the first one a hard link to the first one.
			if item.tree.nlink > 1 {
				if linkTarget, wroteFirstLink := linkTargets[item.tree]; wroteFirstLink {
					item.hdr.Typeflag = tar.TypeLink
					item.hdr.Linkname = linkTarget
					item.hdr.Size = 0
				} else {
					linkTargets[item.tree] = item.hdr.Name
				}
			}
			if options.ContentCreatedDate != nil {
				item.hdr.ModTime = *options.ContentCreatedDate
				item.hdr.AccessTime = *options.ContentCreatedDate
				item.hdr.ChangeTime = *options.ContentCreatedDate
			}
			// Write the header for this item.
			if err := tw.WriteHeader(&item.hdr); err != nil {
				return "", "", -1, "", fmt.Errorf("writing contents for %q: %w", item.hdr.Name, err)
			}
			written[item.hdr.Name] = struct{}{}
			// If the item is a regular file, copy the contents
			// that it should have.
			if item.hdr.Size > 0 {
				// Copy the contents of this file, verifying
				// the digest of its contents to ensure that we
				// don't accidentally corrupt it.
				var original readCloserAt
				if item.path != "" {
					rca, ok := readers[item.layerDigest]
					if !ok {
						rca, err = os.Open(item.path)
						if err != nil {
							return "", "", -1, "", fmt.Errorf("reading contents for %q: %w", item.hdr.Name, err)
						}
						readers[item.layerDigest] = rca
					}
					original = rca
				} else {
					if r.archiveReader != nil {
						rca, ok := readers[""]
						if !ok {
							rca, ok = r.archiveReader.(readCloserAt)
							if !ok {
								return "", "", -1, "", fmt.Errorf("reusing contents of blob: %w", err)
							}
							readers[""] = rca
						}
						original = rca
					} else {
						rca, ok := readers[item.layerDigest]
						if !ok {
							blobInfo := types.BlobInfo{
								Digest: item.layerDigest,
							}
							rc, _, err := r.imageSource.GetBlob(ctx, blobInfo, r.blobInfoCache)
							if err != nil {
								return "", "", -1, "", fmt.Errorf("reusing contents of blob with digest %q: %w", blobInfo.Digest, err)
							}
							rca, ok = rc.(readCloserAt)
							if !ok {
								return "", "", -1, "", fmt.Errorf("internal error: thought blob %q was seekable, but it wasn't really", item.layerDigest)
							}
							readers[item.layerDigest] = rca
						}
						original = rca
					}
				}
				sectionReader := io.NewSectionReader(original, item.offset, item.hdr.Size)
				writer := io.Writer(tw)
				digester := digest.Canonical.Digester()
				writer = io.MultiWriter(writer, digester.Hash())
				n, err := io.Copy(writer, sectionReader)
				if err != nil {
					return "", "", -1, "", fmt.Errorf("reading contents for %q: %w", item.hdr.Name, err)
				}
				if n != item.hdr.Size {
					return "", "", -1, "", fmt.Errorf("internal error writing contents of %q: copied %d bytes instead of %d bytes)", item.hdr.Name, n, item.hdr.Size)
				}
				if digester != nil && digester.Digest() != item.digest {
					return "", "", -1, "", fmt.Errorf("internal error writing contents of %q: digest %q was not preserved, got %q", item.hdr.Name, item.digest, digester.Digest())
				}
			}
		}
		// Finish up: close the archive, read its size, return its size and diff ID.
		tw.Close()
		st, err := f.Stat()
		if err != nil {
			return "", "", -1, "", fmt.Errorf("reading size of %q: %w", f.Name(), err)
		}
		diffID := digester.Digest()
		return f.Name(), diffID, st.Size(), fmt.Sprintf("%d:%s", len(written), diffID.String()), nil
	}

	// Write a layer blob for each slice of content paths.
	for layerIndex, contentList := range options.Layers {
		layerFiles[layerIndex], diffIDs[layerIndex], diffSizes[layerIndex], createdBys[layerIndex], writeErrors[layerIndex] = writeLayerFile(layerIndex, contentList.Contents)
		if writeErrors[layerIndex] != nil {
			if err := os.RemoveAll(dir); err != nil {
				logrus.Warnf("removing temporary directory %q: %v", dir, err)
			}
			return nil, "", fmt.Errorf("buffering new layer %d/%d: %w", layerIndex+1, len(options.Layers), writeErrors[layerIndex])
		}
		createdBys[layerIndex] = contentList.CreatedBy + createdBys[layerIndex]
	}
	if len(left) > 0 {
		// Write one last layer blob with everything that wasn't included in some earlier one.
		contentList := make([]string, 0, len(left))
		for p := range left {
			contentList = append(contentList, p)
		}
		sort.Strings(contentList)
		layerIndex := len(options.Layers)
		layerFiles[layerIndex], diffIDs[layerIndex], diffSizes[layerIndex], createdBys[layerIndex], writeErrors[layerIndex] = writeLayerFile(layerIndex, contentList)
		if writeErrors[layerIndex] != nil {
			if err := os.RemoveAll(dir); err != nil {
				logrus.Warnf("removing temporary directory %q: %v", dir, err)
			}
			return nil, "", fmt.Errorf("buffering new layer %d/%d: %w", layerIndex+1, len(options.Layers), writeErrors[layerIndex])
		}
		createdBys[layerIndex] = options.FinalCreatedBy + createdBys[layerIndex]
	} else {
		// No need for one last layer blob.
		layerFiles = layerFiles[:len(options.Layers)]
		diffIDs = diffIDs[:len(options.Layers)]
		diffSizes = diffSizes[:len(options.Layers)]
		writeErrors = writeErrors[:len(options.Layers)]
		createdBys = createdBys[:len(options.Layers)]
	}

	// Now we can close any reused blobs.
	for diffID, reader := range readers {
		if err := reader.Close(); err != nil {
			return nil, "", fmt.Errorf("closing reused blob %q: %w", diffID, err)
		}
	}

	// Build the new image's history and prune out any layers that ended up
	// without anything in them.
	var history []imgspecv1.History
	created := time.Now()
	if options.ImageCreatedDate != nil {
		created = *options.ImageCreatedDate
	}
	for _, createdBy := range createdBys {
		history = append(history, imgspecv1.History{
			Created:   &created,
			CreatedBy: createdBy,
		})
	}
	i := 0
	emptyLayerDigest := digest.Canonical.FromBytes(func() []byte {
		// this is always the same value
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.Close()
		return buf.Bytes()
	}())
	for i < len(diffIDs) {
		if diffIDs[i] == "" || diffIDs[i] == emptyLayerDigest {
			layerFiles = append(layerFiles[:i], layerFiles[i+1:]...)
			diffIDs = append(diffIDs[:i], diffIDs[i+1:]...)
			diffSizes = append(diffSizes[:i], diffSizes[i+1:]...)
			writeErrors = append(writeErrors[:i], writeErrors[i+1:]...)
			createdBys = append(createdBys[:i], createdBys[i+1:]...)
			history = append(history[:i], history[i+1:]...)
		} else {
			i++
		}
	}
	rootfs := struct {
		RootfsType string          `json:"type"`
		DiffIDs    []digest.Digest `json:"diff_ids"`
	}{
		RootfsType: docker.TypeLayers,
		DiffIDs:    diffIDs,
	}

	// Build a modified config blob with the new history and rootfs info.
	rawConfig := map[string]any{}
	if err := json.Unmarshal(r.configBlob, &rawConfig); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		return nil, "", fmt.Errorf("decoding original image configuration: %w", err)
	}
	oldHistory := rawConfig["history"]
	rawConfig["history"] = history
	rawConfig["rootfs"] = rootfs
	rawConfig["created"] = created
	delete(rawConfig, "docker_version")

	// If we've been asked to preserve as much of the image's original
	// history as possible, try to keep the old history entries around, but
	// only if we can successfully mark all of them as empty layers, so we
	// don't create a mismatch between the number of non-empty layers and
	// the number of diffIDs.
	if options.PreserveHistory {
		var newHistory []any
		preserve := true
		if oldHistory != nil {
			if oldHistorySlice, ok := oldHistory.([]any); ok {
				for i := range oldHistorySlice {
					if entry, ok := oldHistorySlice[i].(map[string]any); ok {
						entry["empty_layer"] = true
						newHistory = append(newHistory, entry)
					} else {
						preserve = false
					}
				}
			} else {
				preserve = false
			}
		}
		if preserve {
			for i := range history {
				newHistory = append(newHistory, &history[i])
			}
			rawConfig["history"] = newHistory
		}
	}

	// Encode the config and record its digest.
	rawConfigBlob, err := json.Marshal(rawConfig)
	if err != nil {
		return nil, "", fmt.Errorf("encoding new image configuration: %w", err)
	}
	if rawConfigBlob, err = convertConfigBlob(rawConfigBlob, options.ForceManifestMIMEType); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		return nil, "", fmt.Errorf("forcing type of image configuration: %w", err)
	}
	rawConfigDigest := digest.Canonical.FromBytes(rawConfigBlob)

	// Build a new manifest to hold the config blob and layers.  Try to
	// keep the image's mediaType the same.
	var manifestType string
	rawManifest := map[string]any{}
	rawManifest["schemaVersion"] = 2
	manifestConfig := imgspecv1.Descriptor{
		Digest: rawConfigDigest,
		Size:   int64(len(rawConfigBlob)),
	}
	manifestLayers := make([]imgspecv1.Descriptor, 0, len(diffIDs))
	for i, diffID := range diffIDs {
		manifestLayers = append(manifestLayers, imgspecv1.Descriptor{
			Digest: diffID,
			Size:   diffSizes[i],
		})
	}
	outputType := options.ForceManifestMIMEType
	if outputType == "" {
		switch r.configType {
		case manifest.DockerV2Schema2ConfigMediaType:
			outputType = manifest.DockerV2Schema2MediaType
		case imgspecv1.MediaTypeImageConfig:
			outputType = imgspecv1.MediaTypeImageManifest
		default:
			return nil, "", fmt.Errorf("unsupported intended image type %q", options.ForceManifestMIMEType)
		}
	}
	switch outputType {
	case manifest.DockerV2Schema2MediaType, define.DOCKER:
		manifestType = manifest.DockerV2Schema2MediaType
		rawManifest["mediaType"] = manifestType
		manifestConfig.MediaType = manifest.DockerV2Schema2ConfigMediaType
		rawManifest["config"] = manifestConfig
		for i := range manifestLayers {
			manifestLayers[i].MediaType = manifest.DockerV2SchemaLayerMediaTypeUncompressed
		}
		rawManifest["layers"] = manifestLayers
	case imgspecv1.MediaTypeImageManifest, define.OCI:
		manifestType = imgspecv1.MediaTypeImageManifest
		rawManifest["mediaType"] = manifestType
		manifestConfig.MediaType = imgspecv1.MediaTypeImageConfig
		rawManifest["config"] = manifestConfig
		for i := range manifestLayers {
			manifestLayers[i].MediaType = imgspecv1.MediaTypeImageLayer
		}
		rawManifest["layers"] = manifestLayers
	default:
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		return nil, "", fmt.Errorf("unsupported intended image type %q", options.ForceManifestMIMEType)
	}

	// Encode the manifest and record its digest.
	rawManifestBlob, err := json.Marshal(rawManifest)
	if err != nil {
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		return nil, "", fmt.Errorf("encoding new image manifest: %w", err)
	}
	if rawManifestBlob, err = convertManifestBlob(rawManifestBlob, options.ForceManifestMIMEType); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		return nil, "", fmt.Errorf("forcing type of image manifest: %w", err)
	}
	manifestDigest, err := manifest.Digest(rawManifestBlob)
	if err != nil {
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		return nil, "", fmt.Errorf("calculating digest of new image manifest: %w", err)
	}

	if ref != nil && src != nil {
		// We're creating a new image source.
		blobFiles := make(map[digest.Digest]string)
		for i := range diffIDs {
			blobFiles[diffIDs[i]] = layerFiles[i]
		}
		configFile := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configFile, rawConfigBlob, 0o600); err != nil {
			return nil, "", fmt.Errorf("writing config blob: %w", err)
		}
		blobFiles[manifestConfig.Digest] = configFile
		*src = &reshuffleSource{
			dir:            dir,
			blobFiles:      blobFiles,
			ref:            ref,
			manifestBytes:  rawManifestBlob,
			manifestDigest: manifestDigest,
			manifestType:   manifestType,
			layerInfos:     []types.BlobInfo{},
		}
	} else {
		// Write all of it to the destination.
		var img types.ImageDestination
		if dest != nil {
			img, err = dest.NewImageDestination(ctx, &types.SystemContext{})
			if err != nil {
				return nil, "", fmt.Errorf("writing new image: %w", err)
			}
		} else {
			imageID := rawConfigDigest.Encoded()
			store := storageTransport.Transport.GetStoreIfSet()
			if store == nil {
				return nil, "", fmt.Errorf("building reference for new image %q: local storage not initialized", imageID)
			}
			dest, err := storageTransport.Transport.NewStoreReference(store, nil, imageID)
			if err != nil {
				return nil, "", fmt.Errorf("building reference for new image %q: %w", imageID, err)
			}
			img, err = dest.NewImageDestination(ctx, &types.SystemContext{})
			if err != nil {
				return nil, "", fmt.Errorf("writing new image: %w", err)
			}
		}
		blobInfoCache := blobinfocache.DefaultCache(&types.SystemContext{})
		for i := range diffIDs {
			f, err := os.Open(layerFiles[i])
			if err != nil {
				return nil, "", fmt.Errorf("reading buffered layer at %q: %w", layerFiles[i], err)
			}
			blobInfo := types.BlobInfo{
				Digest: diffIDs[i],
				Size:   diffSizes[i],
			}
			if _, err := img.PutBlob(ctx, f, blobInfo, blobInfoCache, false); err != nil {
				return nil, "", fmt.Errorf("writing layer %d/%d: %w", i+1, len(diffIDs), err)
			}
		}
		configBlobInfo := types.BlobInfo{
			MediaType: manifestConfig.MediaType,
			Digest:    manifestConfig.Digest,
			Size:      manifestConfig.Size,
		}
		if _, err := img.PutBlob(ctx, bytes.NewReader(rawConfigBlob), configBlobInfo, blobInfoCache, true); err != nil {
			return nil, "", fmt.Errorf("putting config blob %q: %w", string(rawConfigBlob), err)
		}
		if err := img.PutManifest(ctx, rawManifestBlob, nil); err != nil {
			return nil, "", fmt.Errorf("putting manifest blob %q: %w", string(rawManifestBlob), err)
		}
		unparsedWritingImage := &unparsedWritingImage{ref: dest, manifest: rawManifestBlob, manifestType: manifestType}
		if err := img.Commit(ctx, unparsedWritingImage); err != nil {
			return nil, "", fmt.Errorf("committing image: %w", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			logrus.Warnf("removing temporary directory %q: %v", dir, err)
		}
		if err := img.Close(); err != nil {
			return nil, "", fmt.Errorf("cleaning up after destination image: %w", err)
		}
	}

	return rawManifestBlob, manifestDigest, nil
}

func convertConfigBlob(rawConfigBlob []byte, forceManifestMIMEType string) ([]byte, error) {
	switch forceManifestMIMEType {
	case define.OCIv1ImageManifest, define.OCI:
		var i imgspecv1.Image
		if err := json.Unmarshal(rawConfigBlob, &i); err != nil {
			return nil, fmt.Errorf("decoding image configuration blob: %w", err)
		}
		return json.Marshal(i)
	case define.Dockerv2ImageManifest, define.DOCKER:
		var i docker.V2Image
		if err := json.Unmarshal(rawConfigBlob, &i); err != nil {
			return nil, fmt.Errorf("decoding image configuration blob: %w", err)
		}
		if i.Config == nil {
			i.Config = &i.ContainerConfig
		}
		return json.Marshal(i)
	case "":
		return rawConfigBlob, nil
	default:
		return nil, fmt.Errorf("unsupported image manifest type %q while converting image config", forceManifestMIMEType)
	}
}

func convertManifestBlob(rawManifestBlob []byte, forceMIMEType string) ([]byte, error) {
	switch forceMIMEType {
	case imgspecv1.MediaTypeImageManifest, define.OCI:
		var m imgspecv1.Manifest
		if err := json.Unmarshal(rawManifestBlob, &m); err != nil {
			return nil, fmt.Errorf("decoding image manifest blob: %w", err)
		}
		m.MediaType = imgspecv1.MediaTypeImageManifest
		m.Config.MediaType = imgspecv1.MediaTypeImageConfig
		for i := range m.Layers {
			m.Layers[i].MediaType = imgspecv1.MediaTypeImageLayer
		}
		return json.Marshal(m)
	case manifest.DockerV2Schema2MediaType, define.DOCKER:
		var m docker.V2S2Manifest
		if err := json.Unmarshal(rawManifestBlob, &m); err != nil {
			return nil, fmt.Errorf("decoding image manifest blob: %w", err)
		}
		m.MediaType = manifest.DockerV2Schema2MediaType
		m.Config.MediaType = manifest.DockerV2Schema2ConfigMediaType
		for i := range m.Layers {
			m.Layers[i].MediaType = manifest.DockerV2SchemaLayerMediaTypeUncompressed
		}
		return json.Marshal(m)
	case "":
		return rawManifestBlob, nil
	default:
		return nil, fmt.Errorf("unsupported image manifest type %q while converting image config", forceMIMEType)
	}
}

type unparsedWritingImage struct {
	ref          types.ImageReference
	manifest     []byte
	manifestType string
}

func (u *unparsedWritingImage) Reference() types.ImageReference {
	return u.ref
}

func (u *unparsedWritingImage) Manifest(ctx context.Context) ([]byte, string, error) {
	return u.manifest, u.manifestType, nil
}

func (u *unparsedWritingImage) Signatures(ctx context.Context) ([][]byte, error) {
	return nil, nil
}

type reshufflerTransport struct {
	separator string
}

var transport = reshufflerTransport{
	separator: "~",
}

func (r *reshufflerTransport) Name() string {
	return "reshuffle"
}

// Creates a copy of the options in "options" in (base64-encoded, compressed,
// json-encoded) form.
func reshuffleOptionsToString(options ReshuffleOptions) (string, error) {
	var buffer bytes.Buffer
	compressor, err := compression.CompressStream(base64.NewEncoder(base64.RawStdEncoding, &buffer), compression.Gzip, nil)
	if err != nil {
		return "", fmt.Errorf("preparing to compress options: %w", err)
	}
	err = json.NewEncoder(compressor).Encode(options)
	if err := compressor.Close(); err != nil {
		logrus.Warnf("closing compressor: %v", err)
	}
	if err != nil {
		return "", fmt.Errorf("preparing to compress options: %w", err)
	}
	return buffer.String(), nil
}

// ReferenceWithOptions returns a reference to a not-yet-created version of a
// specified image which has had its layers rearranged or otherwise transformed
// while its contents were copied to a destination.
func ReferenceWithOptions(baseReference types.ImageReference, options ReshuffleOptions) (types.ImageReference, error) {
	configSpec, err := reshuffleOptionsToString(options)
	if err != nil {
		return nil, fmt.Errorf("encoding options: %w", err)
	}
	return &reshufflerReference{
		configSpec: configSpec,
		base:       baseReference,
		options:    options,
	}, nil
}

type reshufflerReference struct {
	configSpec string
	base       types.ImageReference
	options    ReshuffleOptions
}

// ParseReference returns a reference to a not-yet-created version of a
// specified image which will have its layers rearranged or otherwise
// transformed when its contents are copied to a destination.
func (r *reshufflerTransport) ParseReference(reference string) (types.ImageReference, error) {
	configSpec, baseSpec, found := strings.Cut(reference, r.separator)
	if !found {
		return alltransports.ParseImageName(reference)
	}
	base, err := alltransports.ParseImageName(baseSpec)
	if err != nil {
		return nil, fmt.Errorf("parsing reference to image %q to reshuffle: %w", baseSpec, err)
	}
	ref := reshufflerReference{
		configSpec: configSpec,
		base:       base,
	}
	uncompressed, _, err := compression.AutoDecompress(base64.NewDecoder(base64.StdEncoding, strings.NewReader(configSpec)))
	if err != nil {
		return nil, fmt.Errorf("decoding and decompressing changes %q: %w", configSpec, err)
	}
	if err := json.NewDecoder(uncompressed).Decode(&ref.options); err != nil {
		return nil, fmt.Errorf("decoding options: %w", err)
	}
	return &ref, nil
}

func (r *reshufflerTransport) ValidatePolicyConfigurationScope(scope string) error {
	return nil
}

func (r *reshufflerReference) Transport() types.ImageTransport {
	return &transport
}

func (r *reshufflerReference) StringWithinTransport() string {
	return r.configSpec + transport.separator + transports.ImageName(r.base)
}

func (r *reshufflerReference) DockerReference() reference.Named {
	return r.base.DockerReference()
}

func (r *reshufflerReference) PolicyConfigurationIdentity() string {
	return ""
}

func (r *reshufflerReference) PolicyConfigurationNamespaces() []string {
	return nil
}

func (r *reshufflerReference) NewImage(ctx context.Context, sys *types.SystemContext) (types.ImageCloser, error) {
	src, err := r.NewImageSource(ctx, sys)
	if err != nil {
		return nil, fmt.Errorf("parsing an image source: %w", err)
	}
	return image.FromSource(ctx, sys, src)
}

func (r *reshufflerReference) NewImageSource(ctx context.Context, sys *types.SystemContext) (types.ImageSource, error) {
	reshuffler, err := FromImage(ctx, sys, r.base, nil, FromImageOptions{
		BlobCacheDirectory: r.options.BlobCacheDirectory,
	})
	var reshuffledSource *reshuffleSource
	_, _, err = reshuffler.writeImageOrSource(ctx, nil, r, &reshuffledSource, r.options)
	if err != nil {
		return nil, fmt.Errorf("building new image %q based on %q: %w", transports.ImageName(r), transports.ImageName(r.base), err)
	}
	if err := reshuffler.Close(); err != nil {
		if err := os.RemoveAll(reshuffledSource.dir); err != nil {
			logrus.Warnf("removing %q: %v", reshuffledSource.dir, err)
		}
		return nil, fmt.Errorf("closing reshuffler: %w", err)
	}
	return reshuffledSource, err
}

func (r *reshufflerReference) NewImageDestination(ctx context.Context, sys *types.SystemContext) (types.ImageDestination, error) {
	return nil, syscall.ENOTSUP
}

func (r *reshufflerReference) DeleteImage(ctx context.Context, sys *types.SystemContext) error {
	return syscall.ENOTSUP
}

func (r *reshuffleSource) Reference() types.ImageReference {
	return r.ref
}

func (r *reshuffleSource) Close() error {
	return os.RemoveAll(r.dir)
}

func (r *reshuffleSource) GetManifest(ctx context.Context, instanceDigest *digest.Digest) ([]byte, string, error) {
	if instanceDigest != nil && *instanceDigest != r.manifestDigest {
		return nil, "", fmt.Errorf("manifest with digest %s not found", *instanceDigest)
	}
	return r.manifestBytes, r.manifestType, nil
}

func (r *reshuffleSource) GetBlob(ctx context.Context, blobInfo types.BlobInfo, bic types.BlobInfoCache) (io.ReadCloser, int64, error) {
	blobFile, ok := r.blobFiles[blobInfo.Digest]
	if !ok {
		return nil, -1, fmt.Errorf("no blob with digest %q", blobInfo.Digest.String())
	}
	f, err := os.Open(blobFile)
	if err != nil {
		return nil, -1, fmt.Errorf("getting blob with digest %q: %w", blobInfo.Digest.String(), err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, -1, fmt.Errorf("getting size of blob with digest %q: %w", blobInfo.Digest.String(), err)
	}
	return f, st.Size(), nil
}

func (r *reshuffleSource) HasThreadSafeGetBlob() bool {
	return true
}

func (r *reshuffleSource) GetSignatures(ctx context.Context, instanceDigest *digest.Digest) ([][]byte, error) {
	return nil, nil
}

type reshuffleSource struct {
	dir            string
	blobFiles      map[digest.Digest]string
	ref            types.ImageReference
	manifestBytes  []byte
	manifestDigest digest.Digest
	manifestType   string
	layerInfos     []types.BlobInfo
}

func (r *reshuffleSource) LayerInfosForCopy(ctx context.Context, instanceDigest *digest.Digest) ([]types.BlobInfo, error) {
	var blobInfos []types.BlobInfo
	for _, layer := range r.layerInfos {
		blobInfos = append(blobInfos, types.BlobInfo{
			MediaType: layer.MediaType,
			Digest:    layer.Digest,
			Size:      layer.Size,
		})
	}
	return blobInfos, nil
}
