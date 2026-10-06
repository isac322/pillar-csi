//go:build linux

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package devidle

import (
	"errors"
	"fmt"
	"syscall"
)

// fdClaim is a Claim holding one open file descriptor.
type fdClaim struct {
	fd   int
	rdev uint64
	path string
}

// Rdev is the kernel device number of the held descriptor.
func (c *fdClaim) Rdev() uint64 { return c.rdev }

// Release closes the held descriptor, dropping the exclusive claim.
func (c *fdClaim) Release() error {
	if c.fd < 0 {
		return nil
	}
	err := syscall.Close(c.fd)
	c.fd = -1
	if err != nil {
		return fmt.Errorf("close exclusive claim on %q: %w", c.path, err)
	}
	return nil
}

// claimDevice opens path O_RDONLY|O_EXCL|O_CLOEXEC and fstats the held
// descriptor so the claim also proves which kernel device the path named.
func claimDevice(path string) (Claim, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_EXCL|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("exclusive open %q: %w", path, ErrDeviceHeld)
		}
		return nil, fmt.Errorf("exclusive open %q: %w", path, err)
	}
	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	if err != nil {
		closeErr := syscall.Close(fd)
		if closeErr != nil {
			return nil, fmt.Errorf("fstat exclusive claim %q: %w; close: %w", path, err, closeErr)
		}
		return nil, fmt.Errorf("fstat exclusive claim %q: %w", path, err)
	}
	return &fdClaim{fd: fd, rdev: st.Rdev, path: path}, nil
}

// statDevice resolves path to its kernel device number.
func statDevice(path string) (uint64, error) {
	var st syscall.Stat_t
	err := syscall.Stat(path, &st)
	if err != nil {
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return st.Rdev, nil
}
