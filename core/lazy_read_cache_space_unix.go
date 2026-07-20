//go:build !windows

package core

import "golang.org/x/sys/unix"

func lazyReadStageLimit(path string) uint64 {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil || stat.Bavail <= 0 || stat.Bsize <= 0 {
		return lazyReadFallbackStageLimitBytes
	}
	blocks := uint64(stat.Bavail)
	blockSize := uint64(stat.Bsize)
	available := ^uint64(0)
	if blocks <= available/blockSize {
		available = blocks * blockSize
	}
	limit := available / 4
	if limit > lazyReadDefaultStageLimitBytes {
		limit = lazyReadDefaultStageLimitBytes
	}
	return limit
}
