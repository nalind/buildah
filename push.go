package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	encconfig "github.com/containers/ocicrypt/config"
	digest "github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
	"go.podman.io/buildah/pkg/blobcache"
	"go.podman.io/common/libimage"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/archive"
)

// PushOptions can be used to alter how an image is copied somewhere.
type PushOptions struct {
	// Compression specifies the type of compression which is applied to
	// layer blobs.  The default is to not use compression, but
	// archive.Gzip is recommended.
	// OBSOLETE: Use CompressionFormat instead.
	Compression archive.Compression
	// SignaturePolicyPath specifies an override location for the signature
	// policy which should be used for verifying the new image as it is
	// being written.  Except in specific circumstances, no value should be
	// specified, indicating that the shared, system-wide default policy
	// should be used.
	SignaturePolicyPath string
	// ReportWriter is an io.Writer which will be used to log the writing
	// of the new image.
	ReportWriter io.Writer
	// Store is the local storage store which holds the source image.
	Store storage.Store
	// github.com/containers/image/types SystemContext to hold credentials
	// and other authentication/authorization information.
	SystemContext *types.SystemContext
	// ManifestType is the format to use
	// possible options are oci, v2s1, and v2s2
	ManifestType string
	// BlobDirectory is the name of a directory in which we'll look for
	// prebuilt copies of layer blobs that we might otherwise need to
	// regenerate from on-disk layers, substituting them in the list of
	// blobs to copy whenever possible.
	BlobDirectory string
	// Quiet is a boolean value that determines if minimal output to
	// the user will be displayed, this is best used for logging.
	// The default is false.
	Quiet bool
	// SignBy is the fingerprint of a GPG key to use for signing the image.
	SignBy string
	// RemoveSignatures causes any existing signatures for the image to be
	// discarded for the pushed copy.
	RemoveSignatures bool
	// MaxRetries is the maximum number of attempts we'll make to push any
	// one image to the external registry if the first attempt fails.
	MaxRetries int
	// RetryDelay is how long to wait before retrying a push attempt.
	RetryDelay time.Duration
	// OciEncryptConfig when non-nil indicates that an image should be encrypted.
	// The encryption options is derived from the construction of EncryptConfig object.
	OciEncryptConfig *encconfig.EncryptConfig
	// OciEncryptLayers represents the list of layers to encrypt.
	// If nil, don't encrypt any layers.
	// If non-nil and len==0, denotes encrypt all layers.
	// integers in the slice represent 0-indexed layer indices, with support for negative
	// indexing. i.e. 0 is the first layer, -1 is the last (top-most) layer.
	OciEncryptLayers *[]int
	// SourceLookupReference provides a function to modify or replace
	// source references.
	SourceLookupReferenceFunc libimage.LookupReferenceFunc
	// DestinationLookupReference provides a function to modify or replace
	// destination references.
	DestinationLookupReferenceFunc libimage.LookupReferenceFunc

	// CompressionFormat is the format to use for the compression of the blobs
	CompressionFormat *compression.Algorithm
	// CompressionLevel specifies what compression level is used
	CompressionLevel *int
	// ForceCompressionFormat ensures that the compression algorithm set in
	// CompressionFormat is used exclusively, and blobs of other compression
	// algorithms are not reused.
	ForceCompressionFormat bool
}

// Push copies the contents of the image to a new location.
func Push(ctx context.Context, image string, dest types.ImageReference, options PushOptions) (reference.Canonical, digest.Digest, error) {
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	default:
	}

	libimageOptions := &libimage.PushOptions{}
	libimageOptions.SignaturePolicyPath = options.SignaturePolicyPath
	libimageOptions.Writer = options.ReportWriter
	libimageOptions.ManifestMIMEType = options.ManifestType
	libimageOptions.SignBy = options.SignBy
	libimageOptions.RemoveSignatures = options.RemoveSignatures
	libimageOptions.RetryDelay = &options.RetryDelay
	libimageOptions.OciEncryptConfig = options.OciEncryptConfig
	libimageOptions.OciEncryptLayers = options.OciEncryptLayers
	libimageOptions.CompressionFormat = options.CompressionFormat
	libimageOptions.CompressionLevel = options.CompressionLevel
	libimageOptions.ForceCompressionFormat = options.ForceCompressionFormat
	libimageOptions.PolicyAllowStorage = true

	if options.Quiet {
		libimageOptions.Writer = nil
	}

	compress := types.PreserveOriginal
	if options.Compression != archive.Uncompressed {
		compress = types.Compress
	}
	libimageOptions.SourceLookupReferenceFunc = func(ref types.ImageReference) (types.ImageReference, error) {
		var err error
		if ref == nil {
			return nil, errors.New("source lookup callback was passed a nil reference")
		}
		if options.SourceLookupReferenceFunc != nil {
			if ref, err = options.SourceLookupReferenceFunc(ref); err != nil {
				return nil, err
			}
		}
		if options.BlobDirectory != "" {
			var cacheOpts []blobcache.Option
			if options.CompressionFormat != nil {
				cacheOpts = append(cacheOpts, blobcache.WithCompressAlgorithm(options.CompressionFormat))
			}
			if ref, err = blobcache.NewBlobCache(ref, options.BlobDirectory, compress, cacheOpts...); err != nil {
				return nil, err
			}
		}
		return ref, err
	}
	libimageOptions.DestinationLookupReferenceFunc = func(ref types.ImageReference) (types.ImageReference, error) {
		var err error
		if options.DestinationLookupReferenceFunc != nil {
			if ref, err = options.DestinationLookupReferenceFunc(ref); err != nil {
				return nil, err
			}
		}
		return ref, nil
	}

	runtime, err := libimage.RuntimeFromStore(options.Store, &libimage.RuntimeOptions{SystemContext: options.SystemContext})
	if err != nil {
		return nil, "", err
	}

	destString := fmt.Sprintf("%s:%s", dest.Transport().Name(), dest.StringWithinTransport())
	manifestBytes, err := runtime.Push(ctx, image, destString, libimageOptions)
	if err != nil {
		return nil, "", err
	}

	manifestDigest, err := manifest.Digest(manifestBytes)
	if err != nil {
		return nil, "", fmt.Errorf("computing digest of manifest of new image %q: %w", transports.ImageName(dest), err)
	}

	var ref reference.Canonical
	if name := dest.DockerReference(); name != nil {
		ref, err = reference.WithDigest(name, manifestDigest)
		if err != nil {
			logrus.Warnf("error generating canonical reference with name %q and digest %s: %v", name, manifestDigest.String(), err)
		}
	}

	return ref, manifestDigest, nil
}
