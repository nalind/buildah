//go:build !linux

package main

const (
	mountSshfs  = "mount-sshfs"
	umountSshfs = "umount-sshfs"
)

func sftpInit() {
}

func flagServingSFTP(string) error {
	return nil
}
