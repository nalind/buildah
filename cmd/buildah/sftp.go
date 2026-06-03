//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.podman.io/buildah"
	"go.podman.io/buildah/pkg/parse"
	"go.podman.io/buildah/util"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
	"golang.org/x/sys/unix"
)

const (
	serveSftpInSubprocess    = "buildah-serve-sftp-chroot"
	serveSftp                = "serve-sftp"
	servingSftpFragment      = "serving-sftp"
	serveSftpCheckInterval   = 100 * time.Millisecond
	serveSftpCheckIterations = 100
)

var (
	serveSftpDescription = "\n  Serves a container or image's rootfs using SFTP over stdio."
	serveSftpCommand     = &cobra.Command{
		Use:     serveSftp,
		Short:   "Serve a container or image's rootfs using SFTP over stdio",
		Long:    serveSftpDescription,
		RunE:    serveSftpCmd,
		Example: `buildah serve-sftp containerID|imageID [ro]`,
		GroupID: groupSystem,
		Args:    cobra.RangeArgs(1, 2),
	}
	serveSftpReadOnly bool
)

func init() {
	reexec.Register(serveSftpInSubprocess, serveSftpSubprocessMain)
}

func sftpInit() {
	serveSftpCommand.SetUsageTemplate(UsageTemplate())
	flags := serveSftpCommand.Flags()
	flags.SetInterspersed(false)
	flags.BoolVar(&serveSftpReadOnly, "read-only", serveSftpReadOnly, "serve SFTP in read-only mode")
	rootCmd.AddCommand(serveSftpCommand)
}

type pipeReadWriteCloser struct {
	io.ReadCloser
	io.WriteCloser
}

func (p pipeReadWriteCloser) Close() error {
	return errors.Join(p.ReadCloser.Close(), p.WriteCloser.Close())
}

// serveSftpCmd runs an SFTP service on stdio pointed at the container or
// image's rootfs.  Because it mounts a filesystem, we need to be running as
// root or in the user's user namespace, hopefully with the right ID mappings
// for the container.
func serveSftpCmd(c *cobra.Command, args []string) error {
	systemContext, err := parse.SystemContextFromOptions(c)
	if err != nil {
		return fmt.Errorf("initializing system context: %w", err)
	}
	store, err := getStore(c)
	if err != nil {
		return fmt.Errorf("opening storage: %w", err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			logrus.Debugf("shutting down storage: %v", err)
		}
	}()

	toMount := args[0]
	var mountPoint string
	readOnly := serveSftpReadOnly
	builder, err := buildah.OpenBuilder(store, toMount)
	if err == nil {
		cdir, err := store.ContainerDirectory(builder.ContainerID)
		if err != nil {
			return err
		}
		if err := lockServingSFTP(cdir); err != nil {
			return fmt.Errorf("gaining exclusive access to mount build container %q: %w", toMount, err)
		}
		// mount a builder
		mp, err := builder.Mount("")
		if err != nil {
			return fmt.Errorf("mounting builder %q: %w", toMount, err)
		}
		defer func() {
			if err := builder.Unmount(); err != nil {
				logrus.Debugf("unmounting builder %q: %v", toMount, err)
			}
		}()
		mountPoint = mp
	} else if errors.Is(err, os.ErrNotExist) || errors.Is(err, storage.ErrContainerUnknown) {
		cdir, err := store.ContainerDirectory(toMount)
		if err != nil {
			return err
		}
		if err := lockServingSFTP(cdir); err != nil {
			return fmt.Errorf("gaining exclusive access to mount container %q: %w", toMount, err)
		}
		// mount a random container
		mp, err := store.Mount(toMount, "")
		if err == nil {
			defer func() {
				if _, err := store.Unmount(toMount, false); err != nil {
					logrus.Debugf("unmounting container %q: %v", toMount, err)
				}
			}()
			mountPoint = mp
		} else if errors.Is(err, os.ErrNotExist) || errors.Is(err, storage.ErrLayerUnknown) {
			// mount an image
			readOnly = true
			imageSpec := toMount
			_, img, err := util.FindImage(store, "", systemContext, imageSpec)
			if err != nil {
				return fmt.Errorf("mounting image %q: %w", toMount, err)
			}
			idir, err := store.ImageDirectory(img.ID)
			if err != nil {
				return err
			}
			if err := lockServingSFTP(idir); err != nil {
				return fmt.Errorf("gaining exclusive access to mount image %q: %w", imageSpec, err)
			}
			mp, err := store.MountImage(img.ID, []string{"ro"}, "")
			if err != nil {
				return fmt.Errorf("mounting image %q: %w", toMount, err)
			}
			defer func() {
				if _, err := store.UnmountImage(img.ID, false); err != nil {
					logrus.Debugf("unmounting image %q: %v", toMount, err)
				}
			}()
			mountPoint = mp
		} else {
			// was not a container, and failed to mount an image
			return fmt.Errorf("mounting container %q: %w", toMount, err)
		}
	} else {
		// failed to mount a builder
		return fmt.Errorf("mounting builder %q: %w", toMount, err)
	}

	logrus.Debugf("mounted %q, serving sftp", mountPoint)
	sftpServerArgs := []string{serveSftpInSubprocess, mountPoint}
	if readOnly {
		sftpServerArgs = append(sftpServerArgs, "ro")
	}
	subprocess := reexec.Command(sftpServerArgs...)
	subprocess.Stdin = os.Stdin
	subprocess.Stdout = os.Stdout
	subprocess.Stderr = os.Stderr
	subprocess.Dir = "/"
	err = subprocess.Start()
	if err != nil {
		return fmt.Errorf("starting sftp server: %w", err)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for received := range signals {
			if err := subprocess.Process.Signal(received); err != nil && !errors.Is(err, os.ErrProcessDone) {
				logrus.Errorf("sending signal to sftp subprocess: %v", err)
			}
		}
	}()
	return subprocess.Wait()
}

// serveSftpSubprocessMain chroots into a mountpoint (os.Args[1]) and runs an
// SFTP service on stdio.  Subsequent arguments are nominally SFTP server
// options, but any value than "ro" is currently discarded.
func serveSftpSubprocessMain() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "expected directory name and optional flags")
		os.Exit(1)
	}
	mountPoint := os.Args[1]

	var flags []string
	if len(os.Args) > 2 {
		flags = os.Args[2:]
	}
	var sftpServerOptions []sftp.ServerOption
	for _, flag := range flags {
		switch flag {
		case "ro":
			sftpServerOptions = append(sftpServerOptions, sftp.ReadOnly())
		default:
			fmt.Fprintln(os.Stderr, "unrecognized flag", flag)
			os.Exit(1)
		}
	}

	sftpPipe := pipeReadWriteCloser{
		ReadCloser:  os.Stdin,
		WriteCloser: os.Stdout,
	}
	sftpServer, err := sftp.NewServer(&sftpPipe, sftpServerOptions...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "initializing sftp server:", err)
		os.Exit(1)
	}

	if err := unix.Chdir(mountPoint); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := unix.Chroot(mountPoint); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := unix.Chdir("/"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	signals := make(chan os.Signal, 10)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	var whichSig os.Signal
	go func() {
		whichSig = <-signals
		switch whichSig {
		case syscall.SIGHUP, syscall.SIGTERM:
			os.Exit(0)
		default:
			fmt.Fprintln(os.Stderr, "sftp server:", whichSig)
			os.Exit(1)
		}
	}()
	err = sftpServer.Serve()
	if err != nil && whichSig != syscall.SIGHUP && whichSig != syscall.SIGTERM {
		fmt.Fprintln(os.Stderr, "sftp server:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// flagServingSFTP returns an error if it looks like an image or container is
// already mounted for use by an sftp server.  That would create an overlay
// mount in its own mount namespace, and operations that would also try to
// mount the filesystem will either fail outright because the kernel won't
// allow its upper directory to be mounted more than once at a time, or will
// fail in maddening ways because the kernel allows it but overlay doesn't
// guarantee that anything will work right if you use the same upper directory
// for more than one mount at the same time.
func flagServingSFTP(datadir string) error {
	lockfile := filepath.Join(datadir, servingSftpFragment)
	fd, err := unix.Open(lockfile, unix.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return &os.PathError{Op: "open", Path: lockfile, Err: err}
	}
	defer func() {
		if err := unix.Close(fd); err != nil {
			logrus.Debugf("closing %q: %v", lockfile, err)
		}
	}()
	for i := range serveSftpCheckIterations {
		if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			break
		}
		if i < serveSftpCheckIterations-1 {
			time.Sleep(serveSftpCheckInterval)
		}
	}
	if err != nil {
		return &os.PathError{Op: "flock", Path: lockfile, Err: err}
	}
	return nil
}

// lockServingSFTP takes a lock on a file to signal to other processes that the
// filesystem is mounted for use by an sftp server.
func lockServingSFTP(datadir string) error {
	lockfile := filepath.Join(datadir, servingSftpFragment)
	fd, err := unix.Open(lockfile, unix.O_RDWR|unix.O_TRUNC|unix.O_CREAT, 0o600)
	if err != nil {
		return &os.PathError{Op: "open", Path: lockfile, Err: err}
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return &os.PathError{Op: "flock", Path: lockfile, Err: err}
	}
	return nil
}
