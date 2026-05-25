//go:build linux

package core

import (
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

var externalPagePrefaultSink atomic.Uint32

func warmContentCacheRegion(path string, offset int64, length int) {
	if path == "" || offset < 0 || length <= 0 {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	_ = unix.Fadvise(int(file.Fd()), offset, int64(length), unix.FADV_WILLNEED)
	_, _, _ = unix.Syscall(unix.SYS_READAHEAD, file.Fd(), uintptr(offset), uintptr(length))
}

func adviseMappedContentCache(data []byte) {
	if len(data) == 0 {
		return
	}
	_ = unix.Madvise(data, unix.MADV_SEQUENTIAL)
	_ = unix.Madvise(data, unix.MADV_WILLNEED)
}

func prefaultMappedContentCache(data []byte) {
	if len(data) == 0 {
		return
	}
	pageSize := os.Getpagesize()
	var sum uint32
	for offset := 0; offset < len(data); offset += pageSize {
		sum += uint32(data[offset])
	}
	sum += uint32(data[len(data)-1])
	externalPagePrefaultSink.Store(sum)
}
