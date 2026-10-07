//go:build !linux

package agent

import "errors"

type unsupportedFilesystemProxyMounter struct{}

func newFilesystemProxyMounter() FilesystemProxyMounter {
	return unsupportedFilesystemProxyMounter{}
}

func (unsupportedFilesystemProxyMounter) Mount(string, string) error {
	return errors.New("filesystem proxy mounts are unsupported on this platform")
}

func (unsupportedFilesystemProxyMounter) Unmount(string) error {
	return errors.New("filesystem proxy mounts are unsupported on this platform")
}

func (unsupportedFilesystemProxyMounter) Mounted(string) (bool, error) {
	return false, errors.New("filesystem proxy mounts are unsupported on this platform")
}
