//go:build !linux

package main

func sftpInit() {
}

func flagServingSFTP(string) error {
	return nil
}
