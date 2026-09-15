package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	encconfig "github.com/containers/ocicrypt/config"
	digest "github.com/opencontainers/go-digest"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/blobcache"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// PullOptions can be used to alter how an image is copied in from somewhere.
type PullOptions struct {
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
	// BlobDirectory is the name of a directory in which we'll attempt to
	// store copies of layer blobs that we pull down, if any.  It should
	// already exist.
	BlobDirectory string
	// AllTags is a boolean value that determines if all tagged images
	// will be downloaded from the repository. The default is false.
	AllTags bool
	// RemoveSignatures causes any existing signatures for the image to be
	// discarded when pulling it.
	RemoveSignatures bool
	// MaxRetries is the maximum number of attempts we'll make to pull any
	// one image from the external registry if the first attempt fails.
	MaxRetries int
	// RetryDelay is how long to wait before retrying a pull attempt.
	RetryDelay time.Duration
	// OciDecryptConfig contains the config that can be used to decrypt an image if it is
	// encrypted if non-nil. If nil, it does not attempt to decrypt an image.
	OciDecryptConfig *encconfig.DecryptConfig
	// PullPolicy takes the value PullIfMissing, PullAlways, PullIfNewer, or PullNever.
	PullPolicy define.PullPolicy
	// SourceLookupReference provides a function to modify or replace
	// source references.
	SourceLookupReferenceFunc libimage.LookupReferenceFunc
	// DestinationLookupReference provides a function to modify or replace
	// destination references.
	DestinationLookupReferenceFunc libimage.LookupReferenceFunc
	// IgnoreSourceName specifies whether or not we try to name a local
	// image after the remote one.
	IgnoreSourceName bool
}

// Pull copies the contents of the image from somewhere else to local storage.  Returns the
// ID of the local image or an error.
func Pull(ctx context.Context, imageName string, options PullOptions) (imageID string, err error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	libimageOptions := &libimage.PullOptions{}
	libimageOptions.SignaturePolicyPath = options.SignaturePolicyPath
	libimageOptions.Writer = options.ReportWriter
	libimageOptions.RemoveSignatures = options.RemoveSignatures
	libimageOptions.OciDecryptConfig = options.OciDecryptConfig
	libimageOptions.AllTags = options.AllTags
	libimageOptions.RetryDelay = &options.RetryDelay
	libimageOptions.PolicyAllowStorage = true
	sourceImageID := ""
	libimageOptions.SourceLookupReferenceFunc = func(ref types.ImageReference) (types.ImageReference, error) {
		if ref == nil {
			return nil, errors.New("source lookup callback was passed a nil reference")
		}
		if options.SourceLookupReferenceFunc != nil {
			if ref, err = options.SourceLookupReferenceFunc(ref); err != nil {
				return nil, err
			}
		}
		srcTransport := ref.Transport()
		if srcTransport == nil {
			return nil, errors.New("source lookup callback was passed a reference with no identified transport")
		}
		srcTransportName := srcTransport.Name()
		if srcTransportName == "" {
			return nil, errors.New("source lookup callback was passed a reference with an unnamed transport")
		}
		if options.IgnoreSourceName {
			if options.AllTags {
				return nil, errors.New("can't compute image IDs for all tags")
			}
			srcImage, err := ref.NewImageSource(ctx, options.SystemContext)
			if err != nil {
				return nil, fmt.Errorf("opening image %q to work out its ID: %w", transports.ImageName(ref), err)
			}
			defer srcImage.Close()
			var instanceDigest *digest.Digest
			manifestBytes, manifestType, err := srcImage.GetManifest(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("reading manifest from %q to work out its ID: %w", transports.ImageName(ref), err)
			}
			if manifest.MIMETypeIsMultiImage(manifestType) {
				list, err := manifest.ListFromBlob(manifestBytes, manifestType)
				if err != nil {
					return nil, fmt.Errorf("parsing manifest from %q to find which image in its list we're using, to work out its ID: %w", transports.ImageName(ref), err)
				}
				chosen, err := list.ChooseInstance(options.SystemContext)
				if err != nil {
					return nil, fmt.Errorf("selecting an image from list in %q, to work out its ID: %w", transports.ImageName(ref), err)
				}
				instanceDigest = &chosen
				manifestBytes, manifestType, err = srcImage.GetManifest(ctx, instanceDigest)
				if err != nil {
					return nil, fmt.Errorf("reading manifest from %q to work out its ID: %w", transports.ImageName(ref), err)
				}
			}
			unparsedImage := image.UnparsedInstance(srcImage, instanceDigest)
			img, err := image.FromUnparsedImage(ctx, options.SystemContext, unparsedImage)
			if err != nil {
				return nil, fmt.Errorf("reading info for image %q, to work out its ID: %w", transports.ImageName(ref), err)
			}
			parsedManifest, err := manifest.FromBlob(manifestBytes, manifestType)
			if err != nil {
				return nil, fmt.Errorf("parsing manifest for %q, to work out its ID: %w", transports.ImageName(ref), err)
			}
			config, err := img.OCIConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("reading config blob for %q, to work out its ID: %w", transports.ImageName(ref), err)
			}
			// set a "force this image ID" for use by the
			// destination callback, depending on the destination
			// to not modify or transform the image metadata
			sourceImageID, err = parsedManifest.ImageID(config.RootFS.DiffIDs)
			if err != nil {
				return nil, fmt.Errorf("computing image ID for %q: %w", transports.ImageName(ref), err)
			}
		}
		return ref, nil
	}
	libimageOptions.DestinationLookupReferenceFunc = func(ref types.ImageReference) (types.ImageReference, error) {
		if ref == nil {
			return nil, errors.New("destination lookup callback was passed a nil reference")
		}
		destTransport := ref.Transport()
		if destTransport == nil {
			return nil, errors.New("destination lookup callback was passed a reference with no identified transport")
		}
		destTransportName := destTransport.Name()
		if destTransportName == "" {
			return nil, errors.New("destination lookup callback was passed a reference with an unnamed transport")
		}
		if options.IgnoreSourceName && destTransportName == imagestorage.Transport.Name() {
			if sourceImageID == "" {
				return nil, errors.New("need to write image using just its ID, but did not determine its ID (yet?)")
			}
			if ref, err = destTransport.ParseReference("@" + sourceImageID); err != nil {
				return nil, err
			}
		}
		if options.DestinationLookupReferenceFunc != nil {
			if ref, err = options.DestinationLookupReferenceFunc(ref); err != nil {
				return nil, err
			}
		}
		if options.BlobDirectory != "" {
			if ref, err = blobcache.NewBlobCache(ref, options.BlobDirectory, types.PreserveOriginal); err != nil {
				return nil, err
			}
		}
		return ref, err
	}

	if options.MaxRetries > 0 {
		retries := uint(options.MaxRetries)
		libimageOptions.MaxRetries = &retries
	}

	pullPolicy, err := config.ParsePullPolicy(options.PullPolicy.String())
	if err != nil {
		return "", err
	}

	runtime, err := libimage.RuntimeFromStore(options.Store, &libimage.RuntimeOptions{SystemContext: options.SystemContext})
	if err != nil {
		return "", err
	}

	pulledImages, err := runtime.Pull(ctx, imageName, pullPolicy, libimageOptions)
	if err != nil {
		return "", err
	}

	if len(pulledImages) == 0 {
		return "", fmt.Errorf("internal error pulling %s: no image pulled and no error", imageName)
	}

	return pulledImages[0].ID(), nil
}
