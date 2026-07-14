/*
 * Copyright (c) 2020. Ant Group. All rights reserved.
 * Copyright (c) 2022. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package mount

import (
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pkg/errors"

	"github.com/containerd/nydus-snapshotter/pkg/errdefs"
	"github.com/containerd/nydus-snapshotter/pkg/utils/retry"
)

type Interface interface {
	Umount(target string) error
}

type Mounter struct {
}

func (m *Mounter) Umount(target string) error {
	if mounted, err := IsMountpoint(target); err == nil {
		if !mounted {
			return errors.New("not mounted")
		}
	} else {
		// A dead FUSE mount (the daemon exited without unmounting) fails the
		// mountpoint check with ENOTCONN before the mount table is even
		// consulted. Only a lazy detach can remove such a mount.
		if errors.Is(err, syscall.ENOTCONN) {
			return syscall.Unmount(target, syscall.MNT_DETACH)
		}
		return err
	}

	// return syscall.Unmount(target, syscall.MNT_FORCE)
	return syscall.Unmount(target, 0)
}

// DetachIfDeadMount detects a dead FUSE mount at path - a mount whose FUSE
// daemon exited without unmounting, so stat(2) on it fails with ENOTCONN
// ("transport endpoint is not connected") - and lazily detaches it so that
// the path becomes a regular directory again. It returns true if a dead
// mount was detected, no matter whether the detach succeeded.
func DetachIfDeadMount(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil || !errors.Is(err, syscall.ENOTCONN) {
		return false, nil
	}

	if err := syscall.Unmount(path, syscall.MNT_DETACH); err != nil {
		return true, errors.Wrapf(err, "lazily detach dead mount %s", path)
	}

	return true, nil
}

func NormalizePath(path string) (realPath string, err error) {
	if realPath, err = filepath.Abs(path); err != nil {
		return "", errors.Wrapf(err, "get absolute path for %s", path)
	}
	if realPath, err = filepath.EvalSymlinks(realPath); err != nil {
		return "", errors.Wrapf(err, "canonicalise path for %s", path)
	}
	if _, err := os.Stat(realPath); err != nil {
		return "", errors.Wrapf(err, "stat target of %s", path)
	}
	return realPath, nil
}

// return value `true` means the path is mounted
func IsMountpoint(path string) (bool, error) {
	realPath, err := NormalizePath(path)
	if err != nil {
		return false, err
	}

	if path == "/" {
		return true, nil
	}

	stat, err := os.Stat(realPath)
	if err != nil {
		return false, err
	}

	parentStat, err := os.Stat(filepath.Dir(realPath))
	if err != nil {
		return false, err
	}

	// If the directory has a different device as parent, then it is a mountpoint.
	if stat.Sys().(*syscall.Stat_t).Dev != parentStat.Sys().(*syscall.Stat_t).Dev {
		return true, nil
	}

	return false, nil
}

func WaitUntilUnmounted(path string) error {
	return retry.Do(func() error {
		// A dead FUSE mount (the daemon exited without unmounting) never becomes
		// "unmounted" on its own: stat(2) on it keeps failing with ENOTCONN
		// forever, so blindly waiting and giving up (as below) leaves it behind
		// permanently. Detach it immediately instead.
		if detached, err := DetachIfDeadMount(path); detached {
			return err
		}

		mounted, err := IsMountpoint(path)
		if err != nil {
			return err
		}

		if mounted {
			return errdefs.ErrDeviceBusy
		}

		return nil
	},
		retry.Attempts(20), // totally wait for 1 seconds, should be enough
		retry.LastErrorOnly(true),
		retry.Delay(50*time.Millisecond),
	)
}
