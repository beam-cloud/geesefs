// Copyright 2015 - 2017 Ka-Hing Cheung
// Copyright 2021 Yandex LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !windows

package core

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
	"golang.org/x/sys/unix"
)

const (
	externalPageMmapWindowBytes       = 64 * 1024 * 1024
	externalPageMmapMaxBytes          = 2 * 1024 * 1024 * 1024
	externalPagePrefetchAheadBytes    = 1024 * 1024 * 1024
	externalPagePrefetchMaxConcurrent = 8
)

type externalPageMmapEntry struct {
	key  string
	data []byte
	refs int
	elem *list.Element
}

type externalPageCachedRegion struct {
	cacheKey  string
	fileStart uint64
	fileEnd   uint64
	mapOffset int
	entry     *externalPageMmapEntry
}

type externalPageMappedRegion struct {
	cacheKey  string
	fileStart uint64
	fileEnd   uint64
	mapOffset int
	entry     *externalPageMmapEntry
}

type externalPageMmapCache struct {
	mu          sync.Mutex
	entries     map[string]*externalPageMmapEntry
	regions     []externalPageCachedRegion
	lru         *list.List
	prefetching map[string]struct{}
	prefetchSem chan struct{}
	mappedBytes int64
	maxBytes    int64
	closed      bool
}

func (fh *FileHandle) ReadFileWithCallback(sOffset int64, sLen int64) (data [][]byte, bytesRead int, callback func(), err error) {
	offset := uint64(sOffset)
	size := uint64(sLen)

	fh.inode.logFuse("ReadFile", offset, size)
	defer func() {
		fh.inode.logFuse("< ReadFile", bytesRead, err)
		if err == io.EOF {
			err = nil
		}
	}()

	if fh.shouldRetrieveHash() {
		fh.retrieveHashMetadata()
	}

	data, _, err = fh.inode.buffers.GetData(offset, size, false)
	if err == nil {
		atomic.AddInt64(&fh.inode.fs.stats.readBufferHits, 1)
		atomic.AddInt64(&fh.inode.fs.stats.readBufferBytes, int64(size))
		return data, int(size), nil, nil
	}

	data, bytesRead, callback, ok, err := fh.tryReadExternalCachePages(offset, size)
	if ok || err != nil {
		if callback != nil {
			path := fh.inode.FullName()
			hash := fh.inode.cacheHashForLog()
			cleanup := callback
			callback = func() {
				started := time.Now()
				cleanup()
				if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
					log.Warnf("geesefs external page cleanup slow: path=%q hash=%q offset=%d size=%d elapsed=%s", path, hash, offset, size, elapsed.Truncate(time.Millisecond))
				}
			}
		}
		return data, bytesRead, callback, err
	}

	data, bytesRead, err = fh.readFileAfterHash(sOffset, sLen)
	return data, bytesRead, nil, err
}

func (fh *FileHandle) tryReadExternalCachePages(offset, size uint64) (data [][]byte, bytesRead int, callback func(), ok bool, err error) {
	pageCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	if !ok || pageCache == nil {
		return nil, 0, nil, false, nil
	}
	atomic.AddInt64(&fh.inode.fs.stats.externalPageAttempts, 1)
	started := time.Now()
	path := fh.inode.FullName()

	fh.inode.mu.Lock()
	fileSize := fh.inode.Attributes.Size
	var hash string
	if fh.inode.userMetadata != nil {
		hash = string(fh.inode.userMetadata[fh.inode.fs.flags.HashAttr])
	}
	if offset >= fileSize {
		fh.inode.mu.Unlock()
		fh.recordExternalPageMiss(path, hash, offset, size, "offset_past_eof", started, nil)
		return nil, 0, nil, false, nil
	}
	if offset+size > fileSize {
		size = fileSize - offset
	}
	if size == 0 || fh.inode.StagedFile != nil || fh.inode.buffers.AnyUnclean() {
		fh.inode.mu.Unlock()
		fh.recordExternalPageMiss(path, "", offset, size, "not_cacheable_state", started, nil)
		return nil, 0, nil, false, nil
	}
	if hash == "" {
		fh.inode.mu.Unlock()
		fh.recordExternalPageMiss(path, "", offset, size, "missing_hash", started, nil)
		return nil, 0, nil, false, nil
	}
	sequential := offset == fh.lastReadEnd
	fh.trackRead(offset, size)
	fh.inode.mu.Unlock()

	mmapCache := fh.inode.fs.externalPageCache()
	if data, callback, ok := mmapCache.lookup(hash, offset, size); ok {
		atomic.AddInt64(&fh.inode.fs.stats.readHits, 1)
		hitCount := atomic.AddInt64(&fh.inode.fs.stats.externalPageHits, 1)
		atomic.AddInt64(&fh.inode.fs.stats.externalPageBytes, int64(size))
		if sequential {
			fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache)
		}
		fh.logExternalPageHit(path, hash, offset, size, 0, "mmap_cache", started, time.Time{}, hitCount)
		return data, int(size), callback, true, nil
	}

	windowOffset := externalPageWindowStart(offset)
	windowEnd := externalPageWindowEnd(offset + size)
	if windowEnd > fileSize {
		windowEnd = fileSize
	}
	if windowEnd <= windowOffset {
		fh.recordExternalPageMiss(path, hash, offset, size, "empty_window", started, nil)
		return fh.tryReadExternalCacheInto(path, hash, offset, size, fileSize, sequential, started)
	}
	windowSize := windowEnd - windowOffset

	views, err := pageCache.ClientLocalPageFileViews(hash, int64(windowOffset), int64(windowSize), struct{ RoutingKey string }{RoutingKey: hash})
	lookupElapsed := time.Since(started)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageLookupCount, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageLookupNanos, lookupElapsed.Nanoseconds())
	if err != nil || len(views) == 0 {
		fh.recordExternalPageMiss(path, hash, offset, size, "no_client_local_page_file", started, err)
		fh.queueExternalCacheReadThrough(hash)
		return fh.tryReadExternalCacheInto(path, hash, offset, size, fileSize, sequential, started)
	}

	mmapStarted := time.Now()
	err = mmapCache.insertWindow(hash, windowOffset, views)
	mmapElapsed := time.Since(mmapStarted)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageMmapCount, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageMmapNanos, mmapElapsed.Nanoseconds())
	if err != nil {
		atomic.AddInt64(&fh.inode.fs.stats.externalPageMmapFailures, 1)
		log.Warnf(
			"geesefs external page mmap failed: path=%q hash=%q offset=%d size=%d views=%d lookup_elapsed=%s mmap_elapsed=%s err=%v",
			path,
			hash,
			offset,
			size,
			len(views),
			lookupElapsed.Truncate(time.Millisecond),
			mmapElapsed.Truncate(time.Millisecond),
			err,
		)
		return fh.tryReadExternalCacheInto(path, hash, offset, size, fileSize, sequential, started)
	}
	data, callback, ok = mmapCache.lookup(hash, offset, size)
	if !ok {
		atomic.AddInt64(&fh.inode.fs.stats.externalPageMmapFailures, 1)
		data, bytesRead, callback, fallbackOK, fallbackErr := fh.tryReadExternalCacheInto(path, hash, offset, size, fileSize, sequential, started)
		if fallbackOK || fallbackErr != nil {
			return data, bytesRead, callback, fallbackOK, fallbackErr
		}
		return nil, 0, nil, false, syscall.EIO
	}

	atomic.AddInt64(&fh.inode.fs.stats.readHits, 1)
	hitCount := atomic.AddInt64(&fh.inode.fs.stats.externalPageHits, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageBytes, int64(size))
	if sequential {
		fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache)
	}
	fh.logExternalPageHit(path, hash, offset, size, len(views), "client_local_page_file", started, mmapStarted, hitCount)
	return data, int(size), callback, true, nil
}

func (fh *FileHandle) prefetchExternalCachePagesOnOpen() {
	pageCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	if !ok || pageCache == nil {
		return
	}

	fh.inode.mu.Lock()
	if fh.inode.StagedFile != nil || fh.inode.buffers.AnyUnclean() || fh.inode.userMetadata == nil || fh.inode.Attributes.Size == 0 {
		fh.inode.mu.Unlock()
		return
	}
	hash := string(fh.inode.userMetadata[fh.inode.fs.flags.HashAttr])
	fileSize := fh.inode.Attributes.Size
	fh.inode.mu.Unlock()
	if hash == "" {
		return
	}

	fh.scheduleExternalPagePrefetch(hash, 0, fileSize, pageCache)
}

func (fh *FileHandle) scheduleExternalPagePrefetch(hash string, start, fileSize uint64, pageCache cfg.ContentCacheClientLocalPageFileViews) {
	if hash == "" || pageCache == nil || start >= fileSize {
		return
	}

	start = externalPageWindowStart(start)
	target := start + externalPagePrefetchAheadBytes
	if target > fileSize {
		target = fileSize
	}

	fh.externalPrefetchMu.Lock()
	if fh.externalPrefetchHash != hash {
		fh.externalPrefetchHash = hash
		fh.externalPrefetchNext = start
	}
	next := fh.externalPrefetchNext
	fh.externalPrefetchMu.Unlock()

	cache := fh.inode.fs.externalPageCache()
	for next < target {
		windowSize := uint64(externalPageMmapWindowBytes)
		if next+windowSize > fileSize {
			windowSize = fileSize - next
		}
		if !cache.prefetchWindow(hash, next, windowSize, fileSize, pageCache) {
			return
		}
		next += windowSize

		fh.externalPrefetchMu.Lock()
		if fh.externalPrefetchHash == hash && next > fh.externalPrefetchNext {
			fh.externalPrefetchNext = next
		}
		fh.externalPrefetchMu.Unlock()
	}
}

func (fh *FileHandle) tryReadExternalCacheInto(path, hash string, offset, size, fileSize uint64, sequential bool, started time.Time) (data [][]byte, bytesRead int, callback func(), ok bool, err error) {
	readIntoCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheReadInto)
	if !ok || readIntoCache == nil {
		return nil, 0, nil, false, nil
	}
	if size == 0 || size > uint64(int(^uint(0)>>1)) {
		return nil, 0, nil, false, nil
	}

	atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoAttempts, 1)
	accounted := int64(size)
	if err := fh.inode.fs.bufferPool.Use(accounted, false); err != nil {
		atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoMisses, 1)
		fh.recordExternalPageMiss(path, hash, offset, size, "read_into_memory_limit", started, err)
		return nil, 0, nil, false, nil
	}

	buf := make([]byte, int(size))
	n, readErr := readIntoCache.ReadContentInto(context.Background(), hash, int64(offset), buf, struct{ RoutingKey string }{RoutingKey: hash})
	if readErr != nil || n != int64(size) {
		fh.inode.fs.bufferPool.Use(-accounted, false)
		atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoMisses, 1)
		if readErr != nil {
			fh.recordExternalPageMiss(path, hash, offset, size, "read_into_miss", started, readErr)
		}
		fh.queueExternalCacheReadThrough(hash)
		return nil, 0, nil, false, nil
	}

	atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoHits, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoBytes, n)
	atomic.AddInt64(&fh.inode.fs.stats.readHits, 1)
	hitCount := atomic.AddInt64(&fh.inode.fs.stats.externalPageHits, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageBytes, n)

	if sequential {
		pageCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
		if ok {
			fh.scheduleExternalPagePrefetch(hash, offset, fileSize, pageCache)
			fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache)
		}
	}

	released := int32(0)
	callback = func() {
		if atomic.CompareAndSwapInt32(&released, 0, 1) {
			fh.inode.fs.bufferPool.Use(-accounted, false)
		}
	}
	fh.logExternalPageHit(path, hash, offset, size, 0, "read_into", started, time.Time{}, hitCount)
	return [][]byte{buf[:n]}, int(n), callback, true, nil
}

func (fh *FileHandle) queueExternalCacheReadThrough(hash string) {
	if hash == "" {
		return
	}
	fh.inode.fs.CacheFileInExternalCache(fh.inode)
}

func (fh *FileHandle) logExternalPageHit(path, hash string, offset, size uint64, views int, source string, started, mmapStarted time.Time, globalHitCount int64) {
	handleHitCount := atomic.AddUint64(&fh.externalPageHitLogCount, 1)
	if handleHitCount > 8 && globalHitCount > 16 && globalHitCount%1024 != 0 && time.Since(started) <= 100*time.Millisecond {
		return
	}
	mmapElapsed := time.Duration(0)
	if !mmapStarted.IsZero() {
		mmapElapsed = time.Since(mmapStarted)
	}
	log.Debugf(
		"geesefs external page hit: source=%s path=%q hash=%q offset=%d size=%d views=%d lookup_elapsed=%s mmap_elapsed=%s total_elapsed=%s hit_count=%d handle_hit_count=%d",
		source,
		path,
		hash,
		offset,
		size,
		views,
		time.Since(started).Truncate(time.Millisecond),
		mmapElapsed.Truncate(time.Millisecond),
		time.Since(started).Truncate(time.Millisecond),
		globalHitCount,
		handleHitCount,
	)
}

func (fs *Goofys) externalPageCache() *externalPageMmapCache {
	fs.externalPageMmapCacheMu.Lock()
	defer fs.externalPageMmapCacheMu.Unlock()
	if fs.externalPageMmapCache == nil {
		fs.externalPageMmapCache = newExternalPageMmapCache(externalPageMmapMaxBytes)
	}
	return fs.externalPageMmapCache
}

func (fs *Goofys) closeExternalPageMmapCache() {
	fs.externalPageMmapCacheMu.Lock()
	cache := fs.externalPageMmapCache
	fs.externalPageMmapCache = nil
	fs.externalPageMmapCacheMu.Unlock()
	if cache != nil {
		cache.close()
	}
}

func newExternalPageMmapCache(maxBytes int64) *externalPageMmapCache {
	if maxBytes <= 0 {
		maxBytes = externalPageMmapMaxBytes
	}
	return &externalPageMmapCache{
		entries:     make(map[string]*externalPageMmapEntry),
		regions:     make([]externalPageCachedRegion, 0, 128),
		lru:         list.New(),
		prefetching: make(map[string]struct{}),
		prefetchSem: make(chan struct{}, externalPagePrefetchMaxConcurrent),
		maxBytes:    maxBytes,
	}
}

func externalPageWindowStart(offset uint64) uint64 {
	return (offset / externalPageMmapWindowBytes) * externalPageMmapWindowBytes
}

func externalPageWindowEnd(offset uint64) uint64 {
	if offset == 0 {
		return externalPageMmapWindowBytes
	}
	return ((offset + externalPageMmapWindowBytes - 1) / externalPageMmapWindowBytes) * externalPageMmapWindowBytes
}

func (c *externalPageMmapCache) lookup(cacheKey string, offset, size uint64) (data [][]byte, callback func(), ok bool) {
	if size == 0 {
		return nil, nil, true
	}
	if cacheKey == "" {
		return nil, nil, false
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, false
	}

	current := offset
	end := offset + size
	entries := make([]*externalPageMmapEntry, 0, 1)
	for current < end {
		idx := sort.Search(len(c.regions), func(i int) bool {
			if c.regions[i].cacheKey < cacheKey {
				return false
			}
			if c.regions[i].cacheKey > cacheKey {
				return true
			}
			return c.regions[i].fileEnd > current
		})
		if idx >= len(c.regions) {
			c.releaseLocked(entries)
			c.mu.Unlock()
			return nil, nil, false
		}
		region := c.regions[idx]
		if region.cacheKey != cacheKey || current < region.fileStart || current >= region.fileEnd {
			c.releaseLocked(entries)
			c.mu.Unlock()
			return nil, nil, false
		}

		entry := region.entry
		entry.refs++
		c.lru.MoveToBack(entry.elem)
		entries = append(entries, entry)

		entryOffset := region.mapOffset + int(current-region.fileStart)
		readLength := int(region.fileEnd - current)
		if remaining := int(end - current); readLength > remaining {
			readLength = remaining
		}
		data = append(data, entry.data[entryOffset:entryOffset+readLength])
		current += uint64(readLength)
	}
	c.mu.Unlock()

	callback = func() {
		c.release(entries)
	}
	return data, callback, true
}

func (c *externalPageMmapCache) prefetchWindow(cacheKey string, offset, size, fileSize uint64, pageCache cfg.ContentCacheClientLocalPageFileViews) bool {
	if cacheKey == "" || pageCache == nil || size == 0 || offset >= fileSize {
		return true
	}
	if offset+size > fileSize {
		size = fileSize - offset
	}
	end := offset + size
	prefetchKey := fmt.Sprintf("%s:%d:%d", cacheKey, offset, end)

	c.mu.Lock()
	if c.closed || c.hasRangeLocked(cacheKey, offset, end) {
		c.mu.Unlock()
		return true
	}
	if _, ok := c.prefetching[prefetchKey]; ok {
		c.mu.Unlock()
		return true
	}
	c.prefetching[prefetchKey] = struct{}{}
	c.mu.Unlock()

	select {
	case c.prefetchSem <- struct{}{}:
	default:
		c.mu.Lock()
		delete(c.prefetching, prefetchKey)
		c.mu.Unlock()
		return false
	}

	go func() {
		defer func() {
			<-c.prefetchSem
			c.mu.Lock()
			delete(c.prefetching, prefetchKey)
			c.mu.Unlock()
		}()

		views, err := pageCache.ClientLocalPageFileViews(cacheKey, int64(offset), int64(size), struct{ RoutingKey string }{RoutingKey: cacheKey})
		if err != nil || len(views) == 0 {
			return
		}
		if err := c.insertWindow(cacheKey, offset, views); err != nil {
			return
		}
		if data, cleanup, ok := c.lookup(cacheKey, offset, size); ok {
			for _, segment := range data {
				prefaultMappedContentCache(segment)
			}
			if cleanup != nil {
				cleanup()
			}
		}
	}()
	return true
}

func (c *externalPageMmapCache) hasRangeLocked(cacheKey string, start, end uint64) bool {
	current := start
	for current < end {
		idx := sort.Search(len(c.regions), func(i int) bool {
			if c.regions[i].cacheKey < cacheKey {
				return false
			}
			if c.regions[i].cacheKey > cacheKey {
				return true
			}
			return c.regions[i].fileEnd > current
		})
		if idx >= len(c.regions) {
			return false
		}
		region := c.regions[idx]
		if region.cacheKey != cacheKey || current < region.fileStart || current >= region.fileEnd {
			return false
		}
		current = region.fileEnd
	}
	return true
}

func (c *externalPageMmapCache) insertWindow(cacheKey string, offset uint64, views []cfg.ClientLocalPageFileView) error {
	if len(views) == 0 {
		return syscall.ENOENT
	}
	if cacheKey == "" {
		return syscall.EINVAL
	}

	mapped := make([]externalPageMappedRegion, 0, len(views))
	current := offset
	for _, view := range views {
		warmContentCacheRegion(view.Path, view.Offset, view.Length)
		entry, err := c.getOrMap(view.Path)
		if err != nil {
			for _, r := range mapped {
				c.release([]*externalPageMmapEntry{r.entry})
			}
			return err
		}
		if view.Offset < 0 || view.Length <= 0 || int(view.Offset)+view.Length > len(entry.data) {
			c.release([]*externalPageMmapEntry{entry})
			for _, r := range mapped {
				c.release([]*externalPageMmapEntry{r.entry})
			}
			return syscall.EINVAL
		}
		mapped = append(mapped, externalPageMappedRegion{
			cacheKey:  cacheKey,
			fileStart: current,
			fileEnd:   current + uint64(view.Length),
			mapOffset: int(view.Offset),
			entry:     entry,
		})
		current += uint64(view.Length)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.releaseLocked(mappedEntries(mapped))
		return syscall.EBADF
	}
	c.removeRegionsLocked(cacheKey, offset, current)
	for _, region := range mapped {
		c.regions = append(c.regions, externalPageCachedRegion(region))
	}
	sort.Slice(c.regions, func(i, j int) bool {
		if c.regions[i].cacheKey != c.regions[j].cacheKey {
			return c.regions[i].cacheKey < c.regions[j].cacheKey
		}
		return c.regions[i].fileStart < c.regions[j].fileStart
	})
	c.releaseLocked(mappedEntries(mapped))
	c.evictLocked()
	return nil
}

func mappedEntries(regions []externalPageMappedRegion) []*externalPageMmapEntry {
	entries := make([]*externalPageMmapEntry, 0, len(regions))
	for _, region := range regions {
		entries = append(entries, region.entry)
	}
	return entries
}

func (c *externalPageMmapCache) getOrMap(path string) (*externalPageMmapEntry, error) {
	if path == "" {
		return nil, syscall.EINVAL
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, syscall.EBADF
	}
	if entry := c.entries[path]; entry != nil {
		entry.refs++
		c.lru.MoveToBack(entry.elem)
		c.mu.Unlock()
		return entry, nil
	}
	c.mu.Unlock()

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Size() <= 0 {
		_ = file.Close()
		return nil, syscall.EINVAL
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, int(info.Size()), unix.PROT_READ, unix.MAP_SHARED)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	adviseMappedContentCache(mapped)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = unix.Munmap(mapped)
		return nil, syscall.EBADF
	}
	if entry := c.entries[path]; entry != nil {
		entry.refs++
		c.lru.MoveToBack(entry.elem)
		_ = unix.Munmap(mapped)
		return entry, nil
	}

	entry := &externalPageMmapEntry{key: path, data: mapped, refs: 1}
	entry.elem = c.lru.PushBack(entry)
	c.entries[path] = entry
	c.mappedBytes += int64(len(mapped))
	return entry, nil
}

func (c *externalPageMmapCache) release(entries []*externalPageMmapEntry) {
	if len(entries) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(entries)
	c.evictLocked()
}

func (c *externalPageMmapCache) releaseLocked(entries []*externalPageMmapEntry) {
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if entry.refs > 0 {
			entry.refs--
		}
	}
}

func (c *externalPageMmapCache) removeRegionsLocked(cacheKey string, start, end uint64) {
	if start >= end || len(c.regions) == 0 {
		return
	}
	dst := c.regions[:0]
	for _, region := range c.regions {
		if region.cacheKey == cacheKey && region.fileStart < end && region.fileEnd > start {
			continue
		}
		dst = append(dst, region)
	}
	c.regions = dst
}

func (c *externalPageMmapCache) evictLocked() {
	if c.maxBytes <= 0 {
		return
	}
	for c.mappedBytes > c.maxBytes {
		elem := c.lru.Front()
		if elem == nil {
			return
		}
		entry := elem.Value.(*externalPageMmapEntry)
		if entry.refs > 0 {
			c.lru.MoveToBack(elem)
			if c.allEntriesReferencedLocked() {
				return
			}
			continue
		}
		c.removeEntryLocked(entry)
	}
}

func (c *externalPageMmapCache) allEntriesReferencedLocked() bool {
	for elem := c.lru.Front(); elem != nil; elem = elem.Next() {
		if elem.Value.(*externalPageMmapEntry).refs == 0 {
			return false
		}
	}
	return true
}

func (c *externalPageMmapCache) removeEntryLocked(entry *externalPageMmapEntry) {
	delete(c.entries, entry.key)
	c.mappedBytes -= int64(len(entry.data))
	if entry.elem != nil {
		c.lru.Remove(entry.elem)
		entry.elem = nil
	}
	dst := c.regions[:0]
	for _, region := range c.regions {
		if region.entry != entry {
			dst = append(dst, region)
		}
	}
	c.regions = dst
	_ = unix.Munmap(entry.data)
	entry.data = nil
}

func (c *externalPageMmapCache) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	entries := make([]*externalPageMmapEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}
	c.entries = make(map[string]*externalPageMmapEntry)
	c.regions = nil
	c.prefetching = make(map[string]struct{})
	c.lru.Init()
	c.mappedBytes = 0
	c.mu.Unlock()

	for _, entry := range entries {
		if entry.data != nil {
			_ = unix.Munmap(entry.data)
		}
	}
}

func (fh *FileHandle) recordExternalPageMiss(path, hash string, offset, size uint64, reason string, started time.Time, err error) {
	missCount := atomic.AddInt64(&fh.inode.fs.stats.externalPageMisses, 1)
	if missCount <= 16 || missCount%1024 == 0 || time.Since(started) > 100*time.Millisecond || (err != nil && reason != "no_client_local_page_file") {
		log.Debugf(
			"geesefs external page miss: path=%q hash=%q offset=%d size=%d reason=%s elapsed=%s miss_count=%d err=%v",
			path,
			hash,
			offset,
			size,
			reason,
			time.Since(started).Truncate(time.Millisecond),
			missCount,
			err,
		)
	}
}

func mmapContentCacheViews(views []cfg.ClientLocalPageFileView, wantLength int) (data [][]byte, cleanup func(), err error) {
	if wantLength < 0 {
		return nil, nil, syscall.EINVAL
	}

	pageSize := int64(os.Getpagesize())
	maps := make([][]byte, 0, len(views))
	total := 0
	cleanup = func() {
		for _, mapped := range maps {
			_ = unix.Munmap(mapped)
		}
	}

	for _, view := range views {
		if view.Path == "" || view.Offset < 0 || view.Length <= 0 {
			cleanup()
			return nil, nil, syscall.EINVAL
		}

		mapOffset := view.Offset - view.Offset%pageSize
		mapDelta := int(view.Offset - mapOffset)
		mapLength := mapDelta + view.Length

		file, openErr := os.Open(view.Path)
		if openErr != nil {
			cleanup()
			return nil, nil, openErr
		}
		mapped, mmapErr := unix.Mmap(int(file.Fd()), mapOffset, mapLength, unix.PROT_READ, unix.MAP_SHARED)
		_ = file.Close()
		if mmapErr != nil {
			cleanup()
			return nil, nil, mmapErr
		}

		maps = append(maps, mapped)
		data = append(data, mapped[mapDelta:mapLength])
		total += view.Length
	}

	if total != wantLength {
		cleanup()
		return nil, nil, syscall.EIO
	}

	return data, cleanup, nil
}
