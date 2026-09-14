/*
 * Copyright (c) 2022. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package mount

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUmountWithLazyFallbackOnPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mnt")
	require.NoError(t, os.MkdirAll(target, 0755))

	// A plain directory is not a mountpoint, so nothing is unmounted and no lazy
	// detach is needed.
	lazy, err := UmountWithLazyFallback(target)
	require.NoError(t, err)
	require.False(t, lazy)
}

func TestUmountWithLazyFallbackOnMissingPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "does-not-exist")

	// A snapshot whose directory is already gone must not be reported as an error,
	// otherwise cleanup would keep retrying a path that no longer exists.
	lazy, err := UmountWithLazyFallback(target)
	require.NoError(t, err)
	require.False(t, lazy)
}
