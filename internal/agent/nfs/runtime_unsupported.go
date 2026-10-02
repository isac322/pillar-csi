//go:build !linux

package nfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const cleanupTimeout = 15 * time.Second

type unsupportedRuntime struct{}

func newRuntime(Config) runtime { return unsupportedRuntime{} }
func (unsupportedRuntime) identity() (string, error) {
	return "", errors.New("kernel NFS runtime requires Linux")
}
func (unsupportedRuntime) start(context.Context, *diskState, func() error, func(error)) error {
	return errors.New("kernel NFS runtime requires Linux")
}
func (unsupportedRuntime) list(context.Context) ([]entry, error) {
	return nil, errors.New("kernel NFS runtime requires Linux")
}
func (unsupportedRuntime) validateExport(Export) error {
	return errors.New("kernel NFS runtime requires Linux")
}
func (unsupportedRuntime) grant(context.Context, Export, string) error {
	return errors.New("kernel NFS runtime requires Linux")
}
func (unsupportedRuntime) revoke(context.Context, string, string) error {
	return errors.New("kernel NFS runtime requires Linux")
}
func (unsupportedRuntime) health() error { return errors.New("kernel NFS runtime requires Linux") }
func (unsupportedRuntime) close() error  { return nil }
func acquireLock(string) (*os.File, error) {
	return nil, errors.New("kernel NFS runtime requires Linux")
}
func releaseLock(f *os.File) error {
	err := f.Close()
	if err != nil {
		return fmt.Errorf("close NFS ownership lock: %w", err)
	}
	return nil
}
