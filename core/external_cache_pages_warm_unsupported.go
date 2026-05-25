//go:build !linux

package core

func warmContentCacheRegion(path string, offset int64, length int) {}

func adviseMappedContentCache(data []byte) {}

func prefaultMappedContentCache(data []byte) {}
