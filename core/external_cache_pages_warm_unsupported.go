//go:build !linux

package core

import "syscall"

func warmContentCacheRegion(path string, offset int64, length int) error { return syscall.ENOTSUP }

func adviseMappedContentCache(data []byte) {}

func prefaultMappedContentCache(data []byte) {}
