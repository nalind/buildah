//go:build never
// +build never

package main

import (
	"fmt"

	"github.com/containers/buildah/internal/reshuffle"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/buildah/util"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/shortnames"
	storageTransport "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	squashFromImageOptions reshuffle.FromImageOptions
	squashReshuffleOptions reshuffle.ReshuffleOptions
)

func init() {
	squashImageDescription := `Squashes an image, producing another image with the same filesystem contents in a single layer.`
	squashImageCommand := &cobra.Command{
		Use:   "squash-image",
		Short: "Squash an image",
		Long:  squashImageDescription,
		RunE:  squashImageCmd,
		Example: `buildah squash-image imagename
  buildah squash-image imagename squashedimagename`,
		Args:   cobra.RangeArgs(1, 2),
		Hidden: true,
	}
	squashImageCommand.SetUsageTemplate(UsageTemplate())

	flags := squashImageCommand.Flags()
	flags.SetInterspersed(false)
	flags.StringVarP(&squashReshuffleOptions.ForceManifestMIMEType, "format", "f", "", "`format` of the new image's manifest and metadata (defaults to the manifest type of the original image)")
	rootCmd.AddCommand(squashImageCommand)
}

func squashImageCmd(c *cobra.Command, args []string) error {
	ctx := getContext()
	systemContext, err := parse.SystemContextFromOptions(c)
	if err != nil {
		return fmt.Errorf("building system context: %w", err)
	}
	store, err := getStore(c)
	if err != nil {
		return err
	}

	imageSpec := args[0]
	ref, err := alltransports.ParseImageName(imageSpec)
	if err != nil {
		if ref, err = alltransports.ParseImageName(util.DefaultTransport + imageSpec); err != nil {
			// check if the local image exists
			if ref, _, err = util.FindImage(store, "", systemContext, imageSpec); err != nil {
				return err
			}
		}
	}
	refLocal, _, err := util.FindImage(store, "", systemContext, imageSpec)
	if err == nil {
		// Found local image so use that.
		ref = refLocal
	}

	var dest types.ImageReference
	if len(args) > 1 {
		outputSpec := args[1]
		if dest, err = alltransports.ParseImageName(outputSpec); err != nil {
			candidates, err2 := shortnames.ResolveLocally(systemContext, outputSpec)
			if err2 != nil {
				return err2
			}
			if len(candidates) == 0 {
				return fmt.Errorf("parsing target image name %q", outputSpec)
			}
			dest2, err2 := storageTransport.Transport.ParseStoreReference(store, candidates[0].String())
			if err2 != nil {
				return fmt.Errorf("parsing target image name %q: %w", outputSpec, err)
			}
			dest = dest2
		}
	}

	reshuffler, err := reshuffle.FromImage(ctx, systemContext, ref, nil, squashFromImageOptions)
	if err != nil {
		return fmt.Errorf("setting up to reshuffle %q: %w", transports.ImageName(ref), err)
	}
	defer reshuffler.Close()

	rawManifest, _, err := reshuffler.WriteImage(ctx, dest, squashReshuffleOptions)
	decodedManifest, err2 := manifest.FromBlob(rawManifest, manifest.GuessMIMEType(rawManifest))
	if err2 != nil {
		logrus.Warnf("%v\n", err2)
	} else {
		configInfo := decodedManifest.ConfigInfo()
		if configInfo.Digest != "" {
			fmt.Printf("%s\n", configInfo.Digest.Encoded())
		}
	}
	return err
}
