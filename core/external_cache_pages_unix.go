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
	externalPagePrefetchMaxQueued     = 8
	externalPagePrefetchMaxWait       = 250 * time.Millisecond
)

type externalPageMmapEntry struct {
	key  string
	data []byte
	refs int
	mmap bool
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

type externalPagePrefetchKey struct {
	cacheKey string
	offset   uint64
	end      uint64
}

type externalPagePrefetchState struct {
	done chan struct{}
}

type externalPagePrefetchJob struct {
	fs            *Goofys
	key           externalPagePrefetchKey
	state         *externalPagePrefetchState
	pageCache     cfg.ContentCacheClientLocalPageFileViews
	readIntoCache cfg.ContentCacheReadInto
}

type externalPageMmapCache struct {
	mu             sync.Mutex
	refsChanged    *sync.Cond
	entries        map[string]*externalPageMmapEntry
	regions        []externalPageCachedRegion
	lru            *list.List
	prefetching    map[externalPagePrefetchKey]*externalPagePrefetchState
	prefetchQueue  []externalPagePrefetchJob
	prefetchActive int
	prefetchWG     sync.WaitGroup
	mappedBytes    int64
	inflightBytes  int64
	maxBytes       int64
	memoryPool     *BufferPool
	closed         bool
	closeOnce      sync.Once
}

func (fh *FileHandle) ReadFileWithCallback(sOffset int64, sLen int64) (data [][]byte, bytesRead int, callback func(), err error) {
	if sOffset < 0 || sLen < 0 {
		fh.abandonLazyRead("negative read offset or length", syscall.EINVAL)
		return nil, 0, nil, syscall.EINVAL
	}
	offset := uint64(sOffset)
	size := uint64(sLen)
	started := time.Now()
	var hashMetadataElapsed time.Duration
	var bufferLookupElapsed time.Duration
	var externalElapsed time.Duration
	var fallbackElapsed time.Duration

	fh.inode.logFuse("ReadFile", offset, size)
	defer func() {
		fh.inode.logFuse("< ReadFile", bytesRead, err)
		if err == io.EOF {
			err = nil
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			log.Debugf(
				"geesefs read stage timing: path=%q hash=%q offset=%d size=%d bytes=%d err=%v total=%s hash_metadata=%s buffer_lookup=%s external_cache=%s fallback=%s",
				fh.inode.FullName(),
				fh.inode.cacheHashForLog(),
				offset,
				size,
				bytesRead,
				err,
				elapsed.Truncate(time.Microsecond),
				hashMetadataElapsed.Truncate(time.Microsecond),
				bufferLookupElapsed.Truncate(time.Microsecond),
				externalElapsed.Truncate(time.Microsecond),
				fallbackElapsed.Truncate(time.Microsecond),
			)
		}
	}()
	defer func() {
		fh.recordLazyRead(offset, size, data, bytesRead, err)
	}()

	if fh.shouldRetrieveHash() {
		hashMetadataStarted := time.Now()
		fh.retrieveHashMetadata()
		hashMetadataElapsed = time.Since(hashMetadataStarted)
		atomic.AddInt64(&fh.inode.fs.stats.readHashMetadataCount, 1)
		atomic.AddInt64(&fh.inode.fs.stats.readHashMetadataNanos, hashMetadataElapsed.Nanoseconds())
	}

	bufferLookupStarted := time.Now()
	data, _, err = fh.inode.buffers.GetData(offset, size, false)
	bufferLookupElapsed = time.Since(bufferLookupStarted)
	atomic.AddInt64(&fh.inode.fs.stats.readBufferLookupCount, 1)
	atomic.AddInt64(&fh.inode.fs.stats.readBufferLookupNanos, bufferLookupElapsed.Nanoseconds())
	if err == nil {
		atomic.AddInt64(&fh.inode.fs.stats.readBufferHits, 1)
		atomic.AddInt64(&fh.inode.fs.stats.readBufferBytes, int64(size))
		return data, int(size), nil, nil
	}

	externalStarted := time.Now()
	data, bytesRead, callback, ok, err := fh.tryReadExternalCachePages(offset, size)
	externalElapsed = time.Since(externalStarted)
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

	fallbackStarted := time.Now()
	data, bytesRead, err = fh.readFileAfterHash(sOffset, sLen)
	fallbackElapsed = time.Since(fallbackStarted)
	atomic.AddInt64(&fh.inode.fs.stats.readFallbackCount, 1)
	atomic.AddInt64(&fh.inode.fs.stats.readFallbackNanos, fallbackElapsed.Nanoseconds())
	return data, bytesRead, nil, err
}

func (fh *FileHandle) tryReadExternalCachePages(offset, size uint64) (data [][]byte, bytesRead int, callback func(), ok bool, err error) {
	pageCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	if !ok {
		pageCache = nil
	}
	readIntoCache, _ := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheReadInto)
	if pageCache == nil && readIntoCache == nil {
		return nil, 0, nil, false, nil
	}
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
		return nil, 0, nil, true, nil
	}
	if offset+size > fileSize {
		size = fileSize - offset
	}
	if size == 0 {
		fh.inode.mu.Unlock()
		return nil, 0, nil, true, nil
	}
	atomic.AddInt64(&fh.inode.fs.stats.externalPageAttempts, 1)
	if fh.inode.StagedFile != nil || fh.inode.buffers.AnyUnclean() {
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
			fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache, readIntoCache)
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
	prefetchEnd := windowOffset + externalPageMmapWindowBytes
	if prefetchEnd > fileSize {
		prefetchEnd = fileSize
	}
	if offset+size <= prefetchEnd {
		prefetchDone := mmapCache.prefetchDone(hash, windowOffset, prefetchEnd)
		joined := prefetchDone != nil
		finished := false
		if joined {
			atomic.AddInt64(&fh.inode.fs.stats.externalPrefetch.waitCount, 1)
			waitStarted := time.Now()
			waitTimeout := fh.inode.fs.externalPagePrefetchWaitTimeout()
			if waitTimeout > 0 {
				timer := time.NewTimer(waitTimeout)
				select {
				case <-prefetchDone:
					finished = true
				case <-timer.C:
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			atomic.AddInt64(&fh.inode.fs.stats.externalPrefetch.waitNanos, time.Since(waitStarted).Nanoseconds())
			if !finished {
				atomic.AddInt64(&fh.inode.fs.stats.externalPrefetch.waitTimeouts, 1)
			}
		}

		// A prefetch may finish between the first mmap lookup and finding its
		// in-flight record. Always retry once before issuing a duplicate cache
		// request for the same window.
		if data, callback, ok := mmapCache.lookup(hash, offset, size); ok {
			if joined {
				atomic.AddInt64(&fh.inode.fs.stats.externalPrefetch.waitHits, 1)
			}
			atomic.AddInt64(&fh.inode.fs.stats.readHits, 1)
			hitCount := atomic.AddInt64(&fh.inode.fs.stats.externalPageHits, 1)
			atomic.AddInt64(&fh.inode.fs.stats.externalPageBytes, int64(size))
			if sequential {
				fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache, readIntoCache)
			}
			source := "mmap_cache_race"
			if joined {
				source = "mmap_prefetch_wait"
			}
			fh.logExternalPageHit(path, hash, offset, size, 0, source, started, time.Time{}, hitCount)
			return data, int(size), callback, true, nil
		}
	}

	if pageCache == nil {
		return fh.tryReadExternalCacheInto(path, hash, offset, size, fileSize, sequential, started)
	}

	viewStarted := time.Now()
	views, err := fh.inode.fs.externalCacheClientLocalPageFileViews(pageCache, hash, int64(windowOffset), int64(windowSize))
	viewElapsed := time.Since(viewStarted)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageViewCount, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageViewNanos, viewElapsed.Nanoseconds())
	lookupElapsed := time.Since(started)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageLookupCount, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageLookupNanos, lookupElapsed.Nanoseconds())
	if viewElapsed > 100*time.Millisecond {
		log.Debugf(
			"geesefs external page-view lookup slow: path=%q hash=%q offset=%d size=%d window_offset=%d window_size=%d views=%d err=%v elapsed=%s",
			path,
			hash,
			offset,
			size,
			windowOffset,
			windowSize,
			len(views),
			err,
			viewElapsed.Truncate(time.Millisecond),
		)
	}
	if err != nil || len(views) == 0 {
		fh.recordExternalPageMiss(path, hash, offset, size, "no_client_local_page_file", started, err)
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
		fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache, readIntoCache)
	}
	fh.logExternalPageHit(path, hash, offset, size, len(views), "client_local_page_file", started, mmapStarted, hitCount)
	return data, int(size), callback, true, nil
}

func (fh *FileHandle) prefetchExternalCachePagesOnOpen() {
	pageCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	if !ok {
		pageCache = nil
	}
	readIntoCache, _ := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheReadInto)
	if pageCache == nil && readIntoCache == nil {
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

	fh.scheduleExternalPagePrefetch(hash, 0, fileSize, pageCache, readIntoCache)
}

func (fh *FileHandle) scheduleExternalPagePrefetch(hash string, start, fileSize uint64, pageCache cfg.ContentCacheClientLocalPageFileViews, readIntoCache cfg.ContentCacheReadInto) {
	if hash == "" || (pageCache == nil && readIntoCache == nil) || start >= fileSize {
		return
	}

	start = externalPageWindowStart(start)
	cache := fh.inode.fs.externalPageCache()
	aheadBytes := uint64(externalPagePrefetchAheadBytes)
	cache.mu.Lock()
	if cache.maxBytes > 0 && uint64(cache.maxBytes) < aheadBytes {
		aheadBytes = uint64(cache.maxBytes)
	}
	cache.mu.Unlock()
	target := start + aheadBytes
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

	for next < target {
		windowSize := uint64(externalPageMmapWindowBytes)
		if next+windowSize > fileSize {
			windowSize = fileSize - next
		}
		if !cache.prefetchWindow(fh.inode.fs, hash, next, windowSize, fileSize, pageCache, readIntoCache) {
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
	readIntoStarted := time.Now()
	n, readErr := fh.inode.fs.externalCacheReadContentInto(readIntoCache, hash, int64(offset), buf)
	readIntoElapsed := time.Since(readIntoStarted)
	atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoNanos, readIntoElapsed.Nanoseconds())
	if readIntoElapsed > 100*time.Millisecond {
		log.Debugf(
			"geesefs external read-into slow: path=%q hash=%q offset=%d size=%d read=%d err=%v elapsed=%s",
			path,
			hash,
			offset,
			size,
			n,
			readErr,
			readIntoElapsed.Truncate(time.Millisecond),
		)
	}
	if readErr != nil || n != int64(size) {
		fh.inode.fs.bufferPool.Use(-accounted, false)
		atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoMisses, 1)
		if readErr != nil {
			fh.recordExternalPageMiss(path, hash, offset, size, "read_into_miss", started, readErr)
		}
		return nil, 0, nil, false, nil
	}

	atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoHits, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalReadIntoBytes, n)
	atomic.AddInt64(&fh.inode.fs.stats.readHits, 1)
	hitCount := atomic.AddInt64(&fh.inode.fs.stats.externalPageHits, 1)
	atomic.AddInt64(&fh.inode.fs.stats.externalPageBytes, n)

	if sequential {
		pageCache, ok := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
		if !ok {
			pageCache = nil
		}
		readIntoCache, _ := fh.inode.fs.flags.ExternalCacheClient.(cfg.ContentCacheReadInto)
		fh.scheduleExternalPagePrefetch(hash, offset, fileSize, pageCache, readIntoCache)
		fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+size), fileSize, pageCache, readIntoCache)
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
		maxBytes := int64(externalPageMmapMaxBytes)
		if fs.bufferPool != nil && fs.bufferPool.max > 0 && fs.bufferPool.max < maxBytes {
			maxBytes = fs.bufferPool.max
		}
		fs.externalPageMmapCache = newExternalPageMmapCacheWithPool(maxBytes, fs.bufferPool)
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
	return newExternalPageMmapCacheWithPool(maxBytes, nil)
}

func newExternalPageMmapCacheWithPool(maxBytes int64, memoryPool *BufferPool) *externalPageMmapCache {
	if maxBytes <= 0 {
		maxBytes = externalPageMmapMaxBytes
	}
	cache := &externalPageMmapCache{
		entries:       make(map[string]*externalPageMmapEntry),
		regions:       make([]externalPageCachedRegion, 0, 128),
		lru:           list.New(),
		prefetching:   make(map[externalPagePrefetchKey]*externalPagePrefetchState),
		prefetchQueue: make([]externalPagePrefetchJob, 0, externalPagePrefetchMaxQueued),
		maxBytes:      maxBytes,
		memoryPool:    memoryPool,
	}
	cache.refsChanged = sync.NewCond(&cache.mu)
	return cache
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

func (fs *Goofys) externalPagePrefetchWaitTimeout() time.Duration {
	timeout := fs.externalCacheReadTimeout()
	if timeout > externalPagePrefetchMaxWait {
		return externalPagePrefetchMaxWait
	}
	return timeout
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

func (c *externalPageMmapCache) prefetchWindow(fs *Goofys, cacheKey string, offset, size, fileSize uint64, pageCache cfg.ContentCacheClientLocalPageFileViews, readIntoCache cfg.ContentCacheReadInto) bool {
	if cacheKey == "" || (pageCache == nil && readIntoCache == nil) || size == 0 || offset >= fileSize {
		return true
	}
	if offset+size > fileSize {
		size = fileSize - offset
	}
	key := externalPagePrefetchKey{cacheKey: cacheKey, offset: offset, end: offset + size}

	c.mu.Lock()
	if c.closed || c.hasRangeLocked(cacheKey, offset, key.end) {
		c.mu.Unlock()
		return true
	}
	if _, ok := c.prefetching[key]; ok {
		c.mu.Unlock()
		atomic.AddInt64(&fs.stats.externalPrefetch.coalesced, 1)
		return true
	}
	if c.prefetchActive >= externalPagePrefetchMaxConcurrent && len(c.prefetchQueue) >= externalPagePrefetchMaxQueued {
		c.mu.Unlock()
		atomic.AddInt64(&fs.stats.externalPrefetch.queueFull, 1)
		return false
	}

	state := &externalPagePrefetchState{done: make(chan struct{})}
	job := externalPagePrefetchJob{
		fs:            fs,
		key:           key,
		state:         state,
		pageCache:     pageCache,
		readIntoCache: readIntoCache,
	}
	c.prefetching[key] = state
	startNow := c.prefetchActive < externalPagePrefetchMaxConcurrent
	if startNow {
		c.prefetchActive++
		c.prefetchWG.Add(1)
	} else {
		c.prefetchQueue = append(c.prefetchQueue, job)
	}
	c.mu.Unlock()

	atomic.AddInt64(&fs.stats.externalPrefetch.queued, 1)
	if startNow {
		c.startPrefetch(job)
	}
	return true
}

func (c *externalPageMmapCache) startPrefetch(job externalPagePrefetchJob) {
	atomic.AddInt64(&job.fs.stats.externalPrefetch.started, 1)
	go func() {
		defer c.prefetchWG.Done()
		success := c.runPrefetch(job)
		c.finishPrefetch(job, success)
	}()
}

func (c *externalPageMmapCache) runPrefetch(job externalPagePrefetchJob) bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}

	size := job.key.end - job.key.offset
	if job.pageCache != nil {
		views, err := job.fs.externalCacheClientLocalPageFileViews(job.pageCache, job.key.cacheKey, int64(job.key.offset), int64(size))
		if err == nil && len(views) > 0 {
			if err := c.insertWindow(job.key.cacheKey, job.key.offset, views); err != nil {
				return false
			}
			c.prefault(job.key.cacheKey, job.key.offset, size)
			return true
		}
	}
	if job.readIntoCache == nil || size > uint64(int(^uint(0)>>1)) {
		return false
	}
	if !c.reserveBytes(int64(size)) {
		return false
	}
	reserved := true
	defer func() {
		if reserved {
			c.releaseReservedBytes(int64(size), true)
		}
	}()
	buf := make([]byte, int(size))
	n, err := job.fs.externalCacheReadContentInto(job.readIntoCache, job.key.cacheKey, int64(job.key.offset), buf)
	if err != nil || n != int64(size) {
		return false
	}
	if err := c.insertReservedBytesWindow(job.key.cacheKey, job.key.offset, buf[:n], int64(size)); err != nil {
		return false
	}
	reserved = false
	c.prefault(job.key.cacheKey, job.key.offset, size)
	return true
}

func (c *externalPageMmapCache) prefault(cacheKey string, offset, size uint64) {
	if data, cleanup, ok := c.lookup(cacheKey, offset, size); ok {
		for _, segment := range data {
			prefaultMappedContentCache(segment)
		}
		if cleanup != nil {
			cleanup()
		}
	}
}

func (c *externalPageMmapCache) finishPrefetch(job externalPagePrefetchJob, success bool) {
	if success {
		atomic.AddInt64(&job.fs.stats.externalPrefetch.completed, 1)
	} else {
		atomic.AddInt64(&job.fs.stats.externalPrefetch.misses, 1)
	}

	c.mu.Lock()
	if state := c.prefetching[job.key]; state == job.state {
		delete(c.prefetching, job.key)
	}
	close(job.state.done)
	if c.prefetchActive > 0 {
		c.prefetchActive--
	}
	var next *externalPagePrefetchJob
	if !c.closed && len(c.prefetchQueue) > 0 {
		queued := c.prefetchQueue[0]
		c.prefetchQueue[0] = externalPagePrefetchJob{}
		c.prefetchQueue = c.prefetchQueue[1:]
		c.prefetchActive++
		c.prefetchWG.Add(1)
		next = &queued
	}
	c.mu.Unlock()

	if next != nil {
		c.startPrefetch(*next)
	}
}

func (c *externalPageMmapCache) prefetchDone(cacheKey string, offset, end uint64) <-chan struct{} {
	key := externalPagePrefetchKey{cacheKey: cacheKey, offset: offset, end: end}
	c.mu.Lock()
	state := c.prefetching[key]
	c.mu.Unlock()
	if state == nil {
		return nil
	}
	return state.done
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

func (c *externalPageMmapCache) reserveBytes(size int64) bool {
	if size <= 0 {
		return false
	}

	c.mu.Lock()
	if c.closed || (c.maxBytes > 0 && size > c.maxBytes) {
		c.mu.Unlock()
		return false
	}
	for c.maxBytes > 0 && c.mappedBytes+c.inflightBytes+size > c.maxBytes {
		if !c.evictOneLocked() {
			c.mu.Unlock()
			return false
		}
	}
	c.inflightBytes += size
	memoryPool := c.memoryPool
	c.mu.Unlock()

	if memoryPool != nil {
		if err := memoryPool.Use(size, false); err != nil {
			c.releaseReservedBytes(size, false)
			return false
		}
	}

	// close() waits for reservations to drain, so a close racing the pool
	// charge cannot let the caller allocate untracked memory.
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		c.releaseReservedBytes(size, true)
		return false
	}
	return true
}

func (c *externalPageMmapCache) releaseReservedBytes(size int64, charged bool) {
	if size <= 0 {
		return
	}
	c.mu.Lock()
	c.inflightBytes -= size
	c.refsChanged.Broadcast()
	memoryPool := c.memoryPool
	c.mu.Unlock()
	if charged && memoryPool != nil {
		memoryPool.Use(-size, false)
	}
}

func (c *externalPageMmapCache) transferReservedBytesLocked(size int64) {
	c.inflightBytes -= size
	c.refsChanged.Broadcast()
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
		warmContentCacheRegion(view.Path, view.Offset, view.Length)
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

func (c *externalPageMmapCache) insertReservedBytesWindow(cacheKey string, offset uint64, data []byte, reservedBytes int64) error {
	if cacheKey == "" || len(data) == 0 || reservedBytes != int64(len(data)) {
		return syscall.EINVAL
	}
	end := offset + uint64(len(data))
	entryKey := fmt.Sprintf("readinto:%s:%d:%d", cacheKey, offset, end)

	entry := &externalPageMmapEntry{key: entryKey, data: data, refs: 1}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return syscall.EBADF
	}
	if old := c.entries[entryKey]; old != nil {
		c.removeEntryLocked(old)
	}
	entry.elem = c.lru.PushBack(entry)
	c.entries[entryKey] = entry
	c.mappedBytes += int64(len(data))
	c.transferReservedBytesLocked(reservedBytes)
	c.removeRegionsLocked(cacheKey, offset, end)
	c.regions = append(c.regions, externalPageCachedRegion{
		cacheKey:  cacheKey,
		fileStart: offset,
		fileEnd:   end,
		mapOffset: 0,
		entry:     entry,
	})
	sort.Slice(c.regions, func(i, j int) bool {
		if c.regions[i].cacheKey != c.regions[j].cacheKey {
			return c.regions[i].cacheKey < c.regions[j].cacheKey
		}
		return c.regions[i].fileStart < c.regions[j].fileStart
	})
	c.releaseLocked([]*externalPageMmapEntry{entry})
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
	if info.Size() > int64(int(^uint(0)>>1)) || !c.reserveBytes(info.Size()) {
		_ = file.Close()
		return nil, syscall.ENOMEM
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, int(info.Size()), unix.PROT_READ, unix.MAP_SHARED)
	_ = file.Close()
	if err != nil {
		c.releaseReservedBytes(info.Size(), true)
		return nil, err
	}
	adviseMappedContentCache(mapped)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = unix.Munmap(mapped)
		c.releaseReservedBytes(info.Size(), true)
		return nil, syscall.EBADF
	}
	if entry := c.entries[path]; entry != nil {
		entry.refs++
		c.lru.MoveToBack(entry.elem)
		c.mu.Unlock()
		_ = unix.Munmap(mapped)
		c.releaseReservedBytes(info.Size(), true)
		return entry, nil
	}

	entry := &externalPageMmapEntry{key: path, data: mapped, refs: 1, mmap: true}
	entry.elem = c.lru.PushBack(entry)
	c.entries[path] = entry
	c.mappedBytes += int64(len(mapped))
	c.transferReservedBytesLocked(info.Size())
	c.mu.Unlock()
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
	changed := false
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if entry.refs > 0 {
			entry.refs--
			changed = true
		}
	}
	if changed {
		c.refsChanged.Broadcast()
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
	for c.mappedBytes+c.inflightBytes > c.maxBytes {
		if !c.evictOneLocked() {
			return
		}
	}
}

func (c *externalPageMmapCache) evictOneLocked() bool {
	for elem := c.lru.Front(); elem != nil; elem = elem.Next() {
		entry := elem.Value.(*externalPageMmapEntry)
		if entry.refs == 0 {
			c.removeEntryLocked(entry)
			return true
		}
	}
	return false
}

func (c *externalPageMmapCache) removeEntryLocked(entry *externalPageMmapEntry) {
	delete(c.entries, entry.key)
	entryBytes := int64(len(entry.data))
	c.mappedBytes -= entryBytes
	if c.memoryPool != nil {
		c.memoryPool.Use(-entryBytes, false)
	}
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
	if entry.mmap {
		_ = unix.Munmap(entry.data)
	}
	entry.data = nil
}

func (c *externalPageMmapCache) close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		for _, job := range c.prefetchQueue {
			if state := c.prefetching[job.key]; state == job.state {
				delete(c.prefetching, job.key)
			}
			close(job.state.done)
		}
		c.prefetchQueue = nil
		c.mu.Unlock()

		// Active prefetches may be prefaulting borrowed mmap slices.
		c.prefetchWG.Wait()

		c.mu.Lock()
		for c.inflightBytes != 0 || !c.allEntriesUnreferencedLocked() {
			c.refsChanged.Wait()
		}
		entries := make([]*externalPageMmapEntry, 0, len(c.entries))
		for _, entry := range c.entries {
			entries = append(entries, entry)
		}
		mappedBytes := c.mappedBytes
		memoryPool := c.memoryPool
		c.entries = make(map[string]*externalPageMmapEntry)
		c.regions = nil
		c.lru.Init()
		c.mappedBytes = 0
		c.mu.Unlock()

		for _, entry := range entries {
			if entry.mmap && entry.data != nil {
				_ = unix.Munmap(entry.data)
			}
		}
		if memoryPool != nil && mappedBytes > 0 {
			memoryPool.Use(-mappedBytes, false)
		}
	})
}

func (c *externalPageMmapCache) allEntriesUnreferencedLocked() bool {
	for elem := c.lru.Front(); elem != nil; elem = elem.Next() {
		if elem.Value.(*externalPageMmapEntry).refs != 0 {
			return false
		}
	}
	return true
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
