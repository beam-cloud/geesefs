//go:build windows

package core

func lazyReadStageLimit(path string) uint64 {
	return lazyReadFallbackStageLimitBytes
}
