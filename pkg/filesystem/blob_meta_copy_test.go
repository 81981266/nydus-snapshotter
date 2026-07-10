/*
 * Copyright (c) 2026. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Regression test: a second mount attempt used to fall back from hardlink to
// copyFile when the cache already contained a hardlink of the blob.meta.
// copyFile opened the destination with O_TRUNC, which truncated the shared
// inode and zeroed the source blob.meta in the snapshot directory, poisoning
// the snapshot permanently.
func TestCopyBlobMetaFilesRepeatedMountKeepsContent(t *testing.T) {
	fs := &Filesystem{}

	bootstrapDir := t.TempDir()
	cacheDir := t.TempDir()
	bootstrap := filepath.Join(bootstrapDir, "image.boot")

	content := []byte("blob compression context table")
	metaPath := filepath.Join(bootstrapDir, "digest.blob.meta")
	assert.NoError(t, os.WriteFile(metaPath, content, 0644))

	// First mount: creates a hardlink into the cache directory.
	assert.NoError(t, fs.copyBlobMetaFiles(bootstrap, cacheDir))
	cachedPath := filepath.Join(cacheDir, "digest.blob.meta")
	srcInfo, err := os.Stat(metaPath)
	assert.NoError(t, err)
	dstInfo, err := os.Stat(cachedPath)
	assert.NoError(t, err)
	assert.True(t, os.SameFile(srcInfo, dstInfo))

	// Second mount (e.g. retry after nydusd died): must not truncate anything.
	assert.NoError(t, fs.copyBlobMetaFiles(bootstrap, cacheDir))

	got, err := os.ReadFile(metaPath)
	assert.NoError(t, err)
	assert.Equal(t, content, got, "source blob.meta must keep its content")
	got, err = os.ReadFile(cachedPath)
	assert.NoError(t, err)
	assert.Equal(t, content, got, "cached blob.meta must keep its content")
}

// When the cache holds a stale file with the same name but different inode,
// it should be replaced by a fresh hardlink of the source.
func TestCopyBlobMetaFilesReplacesStaleCacheFile(t *testing.T) {
	fs := &Filesystem{}

	bootstrapDir := t.TempDir()
	cacheDir := t.TempDir()
	bootstrap := filepath.Join(bootstrapDir, "image.boot")

	content := []byte("fresh blob meta")
	metaPath := filepath.Join(bootstrapDir, "digest.blob.meta")
	assert.NoError(t, os.WriteFile(metaPath, content, 0644))

	cachedPath := filepath.Join(cacheDir, "digest.blob.meta")
	assert.NoError(t, os.WriteFile(cachedPath, []byte("stale"), 0644))

	assert.NoError(t, fs.copyBlobMetaFiles(bootstrap, cacheDir))

	got, err := os.ReadFile(cachedPath)
	assert.NoError(t, err)
	assert.Equal(t, content, got)
	got, err = os.ReadFile(metaPath)
	assert.NoError(t, err)
	assert.Equal(t, content, got)
}

// copyFile itself must be a no-op when src and dst are the same inode.
func TestCopyFileSameInodeIsNoop(t *testing.T) {
	fs := &Filesystem{}

	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	content := []byte("payload")
	assert.NoError(t, os.WriteFile(src, content, 0644))
	assert.NoError(t, os.Link(src, dst))

	assert.NoError(t, fs.copyFile(src, dst))

	got, err := os.ReadFile(src)
	assert.NoError(t, err)
	assert.Equal(t, content, got)
}
