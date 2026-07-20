//go:build linux

package core

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

var externalPagePrefaultSink atomic.Uint32

func warmContentCacheRegion(path string, offset int64, length int) error {
	if path == "" || offset < 0 || length <= 0 {
		return syscall.EINVAL
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	fadviseErr := unix.Fadvise(int(file.Fd()), offset, int64(length), unix.FADV_WILLNEED)
	_, _, readaheadErr := unix.Syscall(unix.SYS_READAHEAD, file.Fd(), uintptr(offset), uintptr(length))
	if fadviseErr == nil || readaheadErr == 0 {
		return nil
	}
	return fmt.Errorf("fadvise failed: %v; readahead failed: %w", fadviseErr, readaheadErr)
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
