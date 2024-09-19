package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/containers/buildah"
	"github.com/containers/buildah/internal/reshuffle"
	cp "github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/unshare"
	"golang.org/x/exp/slices"
)

func main() {
	if buildah.InitReexec() {
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: shuffle [-l] input [output [layer1item,... [...]]]\n")
		os.Exit(1)
	}
	unshare.MaybeReexecUsingUserNamespace(true)
	ctx := context.TODO()
	sys := &types.SystemContext{}
	args := slices.Clone(os.Args)
	long := false
	sortBySize := false
	sortByDate := false
	preserveHistory := false
	if i := slices.Index(args[1:], "-l"); i != -1 {
		i++
		long = true
		args = append(args[:i], args[i+1:]...)
	}
	if i := slices.Index(args[1:], "-S"); i != -1 {
		i++
		sortBySize = true
		args = append(args[:i], args[i+1:]...)
	}
	if i := slices.Index(args[1:], "-t"); i != -1 {
		i++
		sortByDate = true
		args = append(args[:i], args[i+1:]...)
	}
	if i := slices.Index(args[1:], "-H"); i != -1 {
		i++
		preserveHistory = true
		args = append(args[:i], args[i+1:]...)
	}
	if len(args) < 3 {
		var shuffler reshuffle.Reshuffler
		if _, err := os.Stat(args[1]); err == nil {
			// first argument is a rootfs tarball
			f, err := os.Open(args[1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			defer f.Close()
			if shuffler, err = reshuffle.FromArchive(ctx, sys, f, true, reshuffle.FromArchiveOptions{}); err != nil {
				fmt.Fprintf(os.Stderr, "building reshuffler from image %q: %v\n", args[1], err)
				os.Exit(1)
			}
		} else {
			// first argument is an image reference
			ref, err := alltransports.ParseImageName(args[1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "parsing image spec %q: %v\n", args[1], err)
				os.Exit(1)
			}
			if shuffler, err = reshuffle.FromImage(ctx, sys, ref, nil, reshuffle.FromImageOptions{}); err != nil {
				fmt.Fprintf(os.Stderr, "building reshuffler from image %q: %v\n", args[1], err)
				os.Exit(1)
			}
		}
		// what's in the input?
		paths, infos, err := shuffler.Paths()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if sortByDate {
			sort.Slice(paths, func(i, j int) bool {
				return infos[paths[i]].ModTime().Before(infos[paths[j]].ModTime())
			})
		}
		if sortBySize {
			sort.Slice(paths, func(i, j int) bool {
				return infos[paths[i]].Size() < infos[paths[j]].Size()
			})
		}
		for _, p := range paths {
			if long {
				fmt.Printf("%s\n", fs.FormatFileInfo(infos[p]))
				continue
			}
			fmt.Printf("%s\n", p)
		}
		if err = shuffler.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	} else {
		// reshuffle and write an output image
		dest, err := alltransports.ParseImageName(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "parsing output image name %q: %v\n", args[2], err)
			os.Exit(1)
		}
		// build lists of items to put into layers
		var layers [][]string
		for _, arg := range args[3:] {
			if arg == "" {
				continue
			}
			paths := strings.Split(arg, ",")
			paths = slices.DeleteFunc(paths, func(s string) bool { return s == "" })
			if len(paths) > 0 {
				layers = append(layers, paths)
			}
		}
		// set up to read the output image
		srcRef, err := alltransports.ParseImageName(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "parsing image spec %q: %v\n", args[1], err)
			os.Exit(1)
		}
		// get ready to remix it
		shuffledRef, err := reshuffle.ReferenceWithOptions(srcRef, reshuffle.ReshuffleOptions{
			Layers:          layers,
			PreserveHistory: preserveHistory,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "shuffling image %q: %v\n", args[2], err)
			os.Exit(1)
		}
		policy, err := signature.DefaultPolicy(sys)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading default signature policy: %v\n", err)
			os.Exit(1)
		}
		policyContext, err := signature.NewPolicyContext(policy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "creating new signature policy context: %v\n", err)
			os.Exit(1)
		}
		options := cp.Options{}
		if _, err = cp.Image(ctx, policyContext, dest, shuffledRef, &options); err != nil {
			fmt.Fprintf(os.Stderr, "writing new image %q: %v\n", args[2], err)
			os.Exit(1)
		}
	}
}
