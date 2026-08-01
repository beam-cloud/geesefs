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
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestExternalCacheClientLocalPageFileViewReadUsesMmap(t *testing.T) {
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, []byte("abcdef"), 0644); err != nil {
		t.Fatal(err)
	}

	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || length != 3 {
				t.Fatalf("unexpected client-local page-file request: hash=%q routing=%q offset=%d length=%d", hash, opts.RoutingKey, offset, length)
			}
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 1, Length: 3}}, nil
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 3
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("expected mmap cleanup callback")
	}
	defer cleanup()
	if bytesRead != 3 {
		t.Fatalf("expected 3 bytes read, got %d", bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("bcd")) {
		t.Fatalf("unexpected data: %q", got)
	}
}

func TestExternalCacheClientLocalPageFileViewReadUsesForegroundRange(t *testing.T) {
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, []byte("abcdef"), 0644); err != nil {
		t.Fatal(err)
	}

	var calls []struct {
		offset int64
		length int64
	}
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			calls = append(calls, struct {
				offset int64
				length int64
			}{offset: offset, length: length})
			if hash != "hash" || opts.RoutingKey != "hash" {
				t.Fatalf("unexpected client-local page-file request: hash=%q routing=%q offset=%d length=%d", hash, opts.RoutingKey, offset, length)
			}
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: offset, Length: int(length)}}, nil
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 6
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)
	defer fs.closeExternalPageMmapCache()

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != 3 {
		t.Fatalf("expected 3 bytes read, got %d", bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("unexpected data: %q", got)
	}
	if cleanup != nil {
		cleanup()
	}

	data, bytesRead, cleanup, err = fh.ReadFileWithCallback(3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != 3 {
		t.Fatalf("expected 3 bytes read, got %d", bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("def")) {
		t.Fatalf("unexpected data: %q", got)
	}
	if cleanup != nil {
		cleanup()
	}
	if len(calls) != 1 {
		t.Fatalf("expected one windowed foreground client-local page-file lookup, got %d", len(calls))
	}
	if calls[0].offset != 0 || calls[0].length != 6 {
		t.Fatalf("unexpected foreground page-file lookups: %+v", calls)
	}
}

func TestExternalCacheClientLocalPageFileViewTimeoutFallsBackToCloud(t *testing.T) {
	want := []byte("hello world")
	flags := cfg.DefaultFlags()
	flags.ExternalCacheReadTimeout = 10 * time.Millisecond
	releaseCache := make(chan struct{})
	cacheStarted := make(chan struct{})
	var closeStarted atomic.Bool
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if closeStarted.CompareAndSwap(false, true) {
				close(cacheStarted)
			}
			<-releaseCache
			return nil, errContentNotFound
		},
	}
	defer close(releaseCache)

	fs := newUnitFS(flags)
	backend := &TestBackend{}
	var getBlobCalls int32
	backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
		atomic.AddInt32(&getBlobCalls, 1)
		return &GetBlobOutput{
			Body: io.NopCloser(bytes.NewReader(want[param.Start : param.Start+param.Count])),
			HeadBlobOutput: HeadBlobOutput{
				BlobItemOutput: BlobItemOutput{Metadata: map[string]*string{}},
			},
		}, nil
	}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.knownSize = uint64(len(want))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	started := time.Now()
	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, int64(len(want)))
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("page-file timeout fallback took too long: %s", elapsed)
	}
	select {
	case <-cacheStarted:
	default:
		t.Fatal("expected client-local page-file lookup")
	}
	if bytesRead != len(want) {
		t.Fatalf("expected %d bytes read, got %d", len(want), bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("fallback data mismatch: got %q want %q", got, want)
	}
	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one S3 fallback read, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalCacheTimeouts); got != 1 {
		t.Fatalf("expected one external cache timeout, got %d", got)
	}
}

func TestExternalCachePageFileMissDoesNotQueueWholeObjectS3ReadThrough(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if hash != "hash" || opts.RoutingKey != "hash" {
				t.Fatalf("unexpected client-local page-file request: hash=%q routing=%q", hash, opts.RoutingKey)
			}
			return nil, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 4
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	_, _, _, ok, err := fh.tryReadExternalCachePages(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected page-file miss")
	}

	select {
	case event := <-fs.cacheEventChan:
		t.Fatalf("unexpected cache event before foreground EOF: %+v", event)
	default:
	}
}

func TestExternalCacheReadIntoMissDoesNotQueueWholeObjectS3ReadThrough(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || len(dst) != 4 {
				t.Fatalf("unexpected read-into request: hash=%q routing=%q offset=%d len=%d", hash, opts.RoutingKey, offset, len(dst))
			}
			return 0, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 4
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	_, _, _, ok, err := fh.tryReadExternalCacheInto("file", "hash", 0, 4, 4, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected read-into miss")
	}

	select {
	case event := <-fs.cacheEventChan:
		t.Fatalf("unexpected cache event before foreground EOF: %+v", event)
	default:
	}
}

func TestExternalCacheReadIntoUnavailableIsCacheMiss(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			return 0, errors.New("selected cache host unavailable")
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 4
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	_, _, _, ok, err := fh.tryReadExternalCacheInto("file", "hash", 0, 4, 4, false, time.Now())
	if err != nil {
		t.Fatalf("expected unavailable cache to degrade to miss, got %v", err)
	}
	if ok {
		t.Fatal("unexpected read-into hit")
	}
}

func TestExternalCachePrefetchPrefersReadInto(t *testing.T) {
	flags := cfg.DefaultFlags()
	payload := []byte("read-into wins")
	var pageCalls atomic.Int64
	var readIntoCalls atomic.Int64
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			pageCalls.Add(1)
			return nil, errContentNotFound
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			readIntoCalls.Add(1)
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || len(dst) != len(payload) {
				return 0, errors.New("unexpected read-into prefetch request")
			}
			copy(dst, payload)
			return int64(len(payload)), nil
		},
	}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()
	inode := NewInode(fs, nil, "file")
	fh := NewFileHandle(inode)

	fh.scheduleExternalPagePrefetch("hash", 0, uint64(len(payload)), flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), flags.ExternalCacheClient.(cfg.ContentCacheReadInto))

	deadline := time.After(2 * time.Second)
	for {
		data, cleanup, ok := fs.externalPageCache().lookup("hash", 0, uint64(len(payload)))
		if ok {
			if cleanup != nil {
				defer cleanup()
			}
			if got := bytes.Join(data, nil); !bytes.Equal(got, payload) {
				t.Fatalf("unexpected prefetched data")
			}
			if got := readIntoCalls.Load(); got != 1 {
				t.Fatalf("unexpected read-into calls: %d", got)
			}
			if got := pageCalls.Load(); got != 0 {
				t.Fatalf("page-file lookup ran despite successful read-into: calls=%d", got)
			}
			return
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for read-into prefetch")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestExternalCachePrefetchFallsBackToPageFilesWhenReadIntoFails(t *testing.T) {
	payload := []byte("page-file fallback")
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, payload, 0644); err != nil {
		t.Fatal(err)
	}

	flags := cfg.DefaultFlags()
	var pageCalls atomic.Int64
	var readIntoCalls atomic.Int64
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			pageCalls.Add(1)
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || length != int64(len(payload)) {
				return nil, errors.New("unexpected page-file prefetch request")
			}
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: len(payload)}}, nil
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			readIntoCalls.Add(1)
			return 0, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()
	inode := NewInode(fs, nil, "file")
	fh := NewFileHandle(inode)

	fh.scheduleExternalPagePrefetch("hash", 0, uint64(len(payload)), flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), flags.ExternalCacheClient.(cfg.ContentCacheReadInto))

	deadline := time.After(2 * time.Second)
	for {
		data, cleanup, ok := fs.externalPageCache().lookup("hash", 0, uint64(len(payload)))
		if ok {
			if cleanup != nil {
				defer cleanup()
			}
			if got := bytes.Join(data, nil); !bytes.Equal(got, payload) {
				t.Fatalf("unexpected prefetched data: %q", got)
			}
			if got := readIntoCalls.Load(); got != 1 {
				t.Fatalf("unexpected read-into calls: %d", got)
			}
			if got := pageCalls.Load(); got != 1 {
				t.Fatalf("unexpected page-file calls: %d", got)
			}
			return
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for page-file fallback prefetch")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestExternalCacheClientLocalPageFileViewEOFDoesNotCountAsMiss(t *testing.T) {
	var calls int64
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			atomic.AddInt64(&calls, 1)
			return nil, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 4
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	data, bytesRead, cleanup, ok, err := fh.tryReadExternalCachePages(4, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected EOF to be handled without falling back to cloud")
	}
	if data != nil || bytesRead != 0 || cleanup != nil {
		t.Fatalf("unexpected EOF read result: data=%v bytes=%d cleanup_present=%t", data, bytesRead, cleanup != nil)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("expected no cache client lookup beyond EOF, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPageAttempts); got != 0 {
		t.Fatalf("expected EOF not to count as page attempt, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPageMisses); got != 0 {
		t.Fatalf("expected EOF not to count as page miss, got %d", got)
	}
}

func TestExternalCacheClientLocalPageFileViewWindowSeparatesHashes(t *testing.T) {
	dir := t.TempDir()
	pageA := filepath.Join(dir, "page-a")
	pageB := filepath.Join(dir, "page-b")
	if err := os.WriteFile(pageA, []byte("aaaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pageB, []byte("bbbb"), 0644); err != nil {
		t.Fatal(err)
	}

	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			path := pageA
			if hash == "hash-b" {
				path = pageB
			}
			return []cfg.ClientLocalPageFileView{{Path: path, Offset: 0, Length: int(length)}}, nil
		},
	}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()

	inodeA := NewInode(fs, nil, "file-a")
	inodeA.Attributes.Size = 4
	inodeA.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash-a")}
	fhA := NewFileHandle(inodeA)

	inodeB := NewInode(fs, nil, "file-b")
	inodeB.Attributes.Size = 4
	inodeB.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash-b")}
	fhB := NewFileHandle(inodeB)

	data, _, cleanup, err := fhA.ReadFileWithCallback(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("aaaa")) {
		t.Fatalf("unexpected data for hash-a: %q", got)
	}

	data, _, cleanup, err = fhB.ReadFileWithCallback(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("bbbb")) {
		t.Fatalf("unexpected data for hash-b: %q", got)
	}
}

func TestExternalPageReadSequenceToleratesReorderedFuseRequests(t *testing.T) {
	flags := cfg.DefaultFlags()
	fs := newUnitFS(flags)
	fh := NewFileHandle(NewInode(fs, nil, "file"))

	const mib = uint64(1024 * 1024)
	for i, offset := range []uint64{0, 2 * mib, mib, 3 * mib} {
		if !fh.observeExternalPageRead("hash", offset, mib) {
			t.Fatalf("read %d at offset %d was not treated as sequential", i, offset)
		}
	}

	if got, want := fh.externalReadHighWater, 4*mib; got != want {
		t.Fatalf("reordered reads advanced high-water to %d, want %d", got, want)
	}
	if fh.observeExternalPageRead("hash", 16*mib, mib) {
		t.Fatal("distant forward read was incorrectly treated as sequential")
	}
	if got, want := fh.externalReadHighWater, 17*mib; got != want {
		t.Fatalf("distant read advanced high-water to %d, want %d", got, want)
	}
	if fh.observeExternalPageRead("hash", 4*mib, mib) {
		t.Fatal("stale backward read was incorrectly treated as sequential")
	}
	if !fh.observeExternalPageRead("replacement-hash", 0, mib) {
		t.Fatal("new immutable content did not reset the read frontier")
	}
	if got, want := fh.externalReadHighWater, mib; got != want {
		t.Fatalf("replacement hash reset high-water to %d, want %d", got, want)
	}
}

func TestExternalPageReorderedReadsExtendPrefetchToEOF(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.MemoryLimit = 2 * externalPagePrefetchAheadBytes
	flags.ExternalCacheClient = &fakeContentCache{}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()
	fh := NewFileHandle(NewInode(fs, nil, "file"))
	cache := fs.externalPageCache()

	const hash = "hash"
	fileSize := uint64(externalPagePrefetchAheadBytes + 2*externalPageMmapWindowBytes)
	cache.mu.Lock()
	cache.regions = append(cache.regions, externalPageCachedRegion{
		cacheKey:  hash,
		fileStart: 0,
		fileEnd:   fileSize,
	})
	cache.mu.Unlock()

	pageCache := flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	const mib = uint64(1024 * 1024)
	for base := uint64(0); base <= externalPageMmapWindowBytes; base += 4 * mib {
		for _, delta := range []uint64{0, 2 * mib, mib, 3 * mib} {
			offset := base + delta
			if !fh.observeExternalPageRead(hash, offset, mib) {
				t.Fatalf("reordered read at offset %d broke the sequential run", offset)
			}
			fh.scheduleExternalPagePrefetch(hash, externalPageWindowEnd(offset+mib), fileSize, pageCache, nil)
		}
	}

	fh.externalPrefetchMu.Lock()
	next := fh.externalPrefetchNext
	highWater := fh.externalReadHighWater
	fh.externalPrefetchMu.Unlock()
	if next != fileSize {
		t.Fatalf("prefetch stopped at %d, want EOF %d", next, fileSize)
	}
	if want := externalPageMmapWindowBytes + 4*mib; highWater != want {
		t.Fatalf("read high-water stopped at %d, want %d", highWater, want)
	}
}

func TestExternalPageMmapCacheEvictsUnreferencedEntries(t *testing.T) {
	dir := t.TempDir()
	pageA := filepath.Join(dir, "page-a")
	pageB := filepath.Join(dir, "page-b")
	if err := os.WriteFile(pageA, []byte("aaaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pageB, []byte("bbbb"), 0644); err != nil {
		t.Fatal(err)
	}

	cache := newExternalPageMmapCache(4)
	defer cache.close()

	viewsA := []cfg.ClientLocalPageFileView{{Path: pageA, Offset: 0, Length: 4}}
	if err := cache.insertWindow("hash-a", 0, viewsA); err != nil {
		t.Fatal(err)
	}
	data, cleanup, ok := cache.lookup("hash-a", 0, 4)
	if !ok {
		t.Fatal("expected hash-a lookup")
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("aaaa")) {
		t.Fatalf("unexpected data for hash-a: %q", got)
	}
	cleanup()

	viewsB := []cfg.ClientLocalPageFileView{{Path: pageB, Offset: 0, Length: 4}}
	if err := cache.insertWindow("hash-b", 0, viewsB); err != nil {
		t.Fatal(err)
	}

	cache.mu.Lock()
	entries := len(cache.entries)
	mappedBytes := cache.mappedBytes
	cache.mu.Unlock()
	if entries != 1 || mappedBytes != 4 {
		t.Fatalf("expected one mapped page after eviction, got entries=%d mappedBytes=%d", entries, mappedBytes)
	}
}

func TestExternalCachePrefetchQueueSustainsConcurrency(t *testing.T) {
	started := make(chan int64, externalPagePrefetchMaxConcurrent+externalPagePrefetchMaxQueued)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	flags := cfg.DefaultFlags()
	flags.MemoryLimit = 2 * externalPagePrefetchAheadBytes
	var startedCount atomic.Int64
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			count := startedCount.Add(1)
			started <- count
			<-release
			return nil, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()

	inode := NewInode(fs, nil, "file")
	fh := NewFileHandle(inode)
	cache := fs.externalPageCache()
	fileSize := uint64(externalPageMmapWindowBytes * (externalPagePrefetchMaxConcurrent + externalPagePrefetchMaxQueued))

	fh.scheduleExternalPagePrefetch("hash", 0, fileSize, flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), nil)

	for i := 0; i < externalPagePrefetchMaxConcurrent; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for initial prefetch %d", i+1)
		}
	}
	cache.mu.Lock()
	active := cache.prefetchActive
	queued := len(cache.prefetchQueue)
	inflight := len(cache.prefetching)
	cache.mu.Unlock()
	if active != externalPagePrefetchMaxConcurrent || queued != externalPagePrefetchMaxQueued || inflight != externalPagePrefetchMaxConcurrent+externalPagePrefetchMaxQueued {
		t.Fatalf("unexpected prefetch scheduler state: active=%d queued=%d inflight=%d", active, queued, inflight)
	}
	if fh.externalPrefetchNext != fileSize {
		t.Fatalf("prefetch did not retain the full read-ahead range: got %d want %d", fh.externalPrefetchNext, fileSize)
	}
	if cache.prefetchWindow(fs, "overflow", 0, externalPageMmapWindowBytes, externalPageMmapWindowBytes, flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), nil) {
		t.Fatal("prefetch scheduler accepted work beyond its bounded queue")
	}

	select {
	case release <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("timed out releasing an active prefetch")
	}
	select {
	case count := <-started:
		if count != externalPagePrefetchMaxConcurrent+1 {
			t.Fatalf("unexpected next prefetch start count: %d", count)
		}
	case <-time.After(time.Second):
		t.Fatal("queued prefetch did not start when an active slot completed")
	}

	closeRelease()
	waitForExternalPageCondition(t, 2*time.Second, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.prefetchActive == 0 && len(cache.prefetchQueue) == 0 && len(cache.prefetching) == 0
	}, "prefetch queue to drain")
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.queued); got != int64(externalPagePrefetchMaxConcurrent+externalPagePrefetchMaxQueued) {
		t.Fatalf("unexpected queued prefetch metric: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.started); got != int64(externalPagePrefetchMaxConcurrent+externalPagePrefetchMaxQueued) {
		t.Fatalf("unexpected started prefetch metric: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.queueFull); got != 1 {
		t.Fatalf("unexpected queue-full metric: %d", got)
	}
}

func TestExternalCachePrefetchMemoryBudgetIncludesInflightBytes(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			started <- hash
			<-release
			for i := range dst {
				dst[i] = hash[0]
			}
			return int64(len(dst)), nil
		},
	}
	fs := newUnitFS(flags)
	cache := fs.externalPageCache()
	cache.mu.Lock()
	cache.maxBytes = 8
	cache.mu.Unlock()
	readInto := flags.ExternalCacheClient.(cfg.ContentCacheReadInto)

	for _, hash := range []string{"a", "b"} {
		if !cache.prefetchWindow(fs, hash, 0, 8, 8, nil, readInto) {
			t.Fatalf("prefetch %q was unexpectedly rejected", hash)
		}
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for budgeted prefetch")
	}
	waitForExternalPageCondition(t, time.Second, func() bool {
		return atomic.LoadInt64(&fs.stats.externalPrefetch.misses) == 1
	}, "over-budget prefetch to be rejected")
	select {
	case hash := <-started:
		t.Fatalf("second read-into bypassed the byte budget: hash=%q", hash)
	default:
	}
	cache.mu.Lock()
	inflightBytes := cache.inflightBytes
	mappedBytes := cache.mappedBytes
	cache.mu.Unlock()
	if inflightBytes != 8 || mappedBytes != 0 {
		t.Fatalf("unexpected active byte accounting: inflight=%d mapped=%d", inflightBytes, mappedBytes)
	}
	if got := atomic.LoadInt64(&fs.bufferPool.cur); got != 8 {
		t.Fatalf("in-flight cache bytes were not charged to the mount memory pool: %d", got)
	}

	closeRelease()
	waitForExternalPageCondition(t, time.Second, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.prefetchActive == 0
	}, "budgeted prefetches to finish")
	cache.mu.Lock()
	inflightBytes = cache.inflightBytes
	mappedBytes = cache.mappedBytes
	cache.mu.Unlock()
	if inflightBytes != 0 || mappedBytes != 8 {
		t.Fatalf("reservation did not transfer to resident accounting: inflight=%d mapped=%d", inflightBytes, mappedBytes)
	}
	if got := atomic.LoadInt64(&fs.bufferPool.cur); got != 8 {
		t.Fatalf("resident cache bytes were not retained in the mount memory pool: %d", got)
	}

	fs.closeExternalPageMmapCache()
	if got := atomic.LoadInt64(&fs.bufferPool.cur); got != 0 {
		t.Fatalf("cache close leaked mount memory accounting: %d", got)
	}
}

func TestExternalPageMmapCacheLimitPreservesSharedPoolHeadroom(t *testing.T) {
	tests := []struct {
		name     string
		poolMax  int64
		expected int64
	}{
		{name: "small mount budget", poolMax: 128 * 1024 * 1024, expected: 64 * 1024 * 1024},
		{name: "default mount budget", poolMax: 2 * 1024 * 1024 * 1024, expected: 1024 * 1024 * 1024},
		{name: "absolute cache cap", poolMax: 8 * 1024 * 1024 * 1024, expected: externalPageMmapMaxBytes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := &BufferPool{max: tt.poolMax}
			if got := externalPageMmapCacheLimit(pool); got != tt.expected {
				t.Fatalf("external page cache limit=%d, want=%d for pool=%d", got, tt.expected, tt.poolMax)
			}
		})
	}
}

func TestExternalPageCachePreservesForegroundMountMemory(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.MemoryLimit = 128 * 1024 * 1024
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			return nil, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	cache := fs.externalPageCache()
	defer fs.closeExternalPageMmapCache()

	if cache.memoryPool != fs.bufferPool {
		t.Fatal("external page cache is not attached to the mount memory pool")
	}
	expectedCacheLimit := fs.bufferPool.max / externalPageMmapPoolShareDivisor
	if cache.maxBytes != expectedCacheLimit {
		t.Fatalf("external page cache limit=%d, want=%d for effective mount limit=%d", cache.maxBytes, expectedCacheLimit, fs.bufferPool.max)
	}

	// Fully reserving the external page cache must still leave enough shared
	// pool capacity for foreground reads to use the rest of the mount budget.
	if !cache.reserveBytes(cache.maxBytes) {
		t.Fatal("failed to reserve the external page cache budget")
	}
	reservedBytes := cache.maxBytes
	defer func() {
		if reservedBytes > 0 {
			cache.releaseReservedBytes(reservedBytes, true)
		}
	}()
	foregroundHeadroom := fs.bufferPool.max - cache.maxBytes
	if err := fs.bufferPool.Use(foregroundHeadroom, false); err != nil {
		t.Fatalf("external page cache starved foreground pool capacity: %v", err)
	}
	fs.bufferPool.Use(-foregroundHeadroom, false)
	cache.releaseReservedBytes(reservedBytes, true)
	reservedBytes = 0

	inode := NewInode(fs, nil, "file")
	fh := NewFileHandle(inode)
	pageCache := flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	fh.scheduleExternalPagePrefetch("hash", 0, externalPagePrefetchAheadBytes, pageCache, nil)
	waitForExternalPageCondition(t, time.Second, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.prefetchActive == 0
	}, "memory-limited prefetch to finish")
	expected := (cache.maxBytes + externalPageMmapWindowBytes - 1) / externalPageMmapWindowBytes
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.started); got != expected {
		t.Fatalf("prefetch ahead ignored the effective mount limit: started=%d want=%d", got, expected)
	}
}

func TestExternalCacheCloseCancelsQueuedPrefetch(t *testing.T) {
	started := make(chan struct{}, externalPagePrefetchMaxConcurrent)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			started <- struct{}{}
			<-release
			return nil, errContentNotFound
		},
	}
	fs := newUnitFS(flags)
	cache := fs.externalPageCache()
	pageCache := flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews)
	fileSize := uint64(externalPageMmapWindowBytes * (externalPagePrefetchMaxConcurrent + 1))
	for i := 0; i < externalPagePrefetchMaxConcurrent+1; i++ {
		offset := uint64(i * externalPageMmapWindowBytes)
		if !cache.prefetchWindow(fs, "hash", offset, externalPageMmapWindowBytes, fileSize, pageCache, nil) {
			t.Fatalf("prefetch %d was unexpectedly rejected", i)
		}
	}
	for i := 0; i < externalPagePrefetchMaxConcurrent; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for active prefetch %d", i+1)
		}
	}
	queuedOffset := uint64(externalPagePrefetchMaxConcurrent * externalPageMmapWindowBytes)
	queuedDone := cache.prefetchDone("hash", queuedOffset, queuedOffset+externalPageMmapWindowBytes)
	if queuedDone == nil {
		t.Fatal("queued prefetch was not tracked")
	}

	closeDone := make(chan struct{})
	go func() {
		fs.closeExternalPageMmapCache()
		close(closeDone)
	}()
	select {
	case <-queuedDone:
	case <-time.After(time.Second):
		t.Fatal("close did not release queued prefetch waiter")
	}
	select {
	case <-closeDone:
		t.Fatal("close returned while active prefetches still held cache state")
	default:
	}
	closeRelease()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wait for active prefetches to finish")
	}
	waitForExternalPageCondition(t, 2*time.Second, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.prefetchActive == 0 && len(cache.prefetchQueue) == 0 && len(cache.prefetching) == 0
	}, "active prefetches to finish after close")
	select {
	case <-started:
		t.Fatal("queued prefetch started after cache close")
	default:
	}
}

func TestExternalCacheCloseWaitsForMappedSliceReferences(t *testing.T) {
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, []byte("mapped"), 0644); err != nil {
		t.Fatal(err)
	}

	cache := newExternalPageMmapCache(4096)
	if err := cache.insertWindow("hash", 0, []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: 6}}); err != nil {
		t.Fatal(err)
	}
	data, cleanup, ok := cache.lookup("hash", 0, 6)
	if !ok || cleanup == nil {
		t.Fatal("expected mapped lookup with cleanup callback")
	}

	closeDone := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cache.close()
			closeDone <- struct{}{}
		}()
	}
	waitForExternalPageCondition(t, time.Second, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.closed
	}, "cache close to begin")
	select {
	case <-closeDone:
		t.Fatal("close returned before mapped slice cleanup")
	default:
	}
	if got := string(bytes.Join(data, nil)); got != "mapped" {
		t.Fatalf("mapped slice changed while close was waiting: %q", got)
	}

	cleanup()
	for i := 0; i < 2; i++ {
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("concurrent close did not finish after mapped slice cleanup")
		}
	}
	cache.close()
}

func TestExternalCacheForegroundJoinsInflightPrefetch(t *testing.T) {
	const fileSize = 4 * 1024 * 1024
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(pagePath, fileSize); err != nil {
		t.Fatal(err)
	}

	prefetchStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	var pageCalls atomic.Int64
	var readIntoCalls atomic.Int64
	flags := cfg.DefaultFlags()
	flags.ExternalCacheReadTimeout = time.Second
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if pageCalls.Add(1) != 1 {
				return nil, errContentNotFound
			}
			close(prefetchStarted)
			<-release
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: int(length)}}, nil
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			readIntoCalls.Add(1)
			for i := range dst {
				dst[i] = 'r'
			}
			return int64(len(dst)), nil
		},
	}
	fs := newUnitFS(flags)
	defer func() {
		closeRelease()
		fs.closeExternalPageMmapCache()
	}()
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = fileSize
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)
	cache := fs.externalPageCache()
	cache.prefetchWindow(fs, "hash", 0, fileSize, fileSize, flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), nil)

	select {
	case <-prefetchStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prefetch to start")
	}
	type readResult struct {
		data     [][]byte
		bytes    int
		callback func()
		err      error
	}
	readDone := make(chan readResult, 1)
	go func() {
		data, bytesRead, callback, err := fh.ReadFileWithCallback(0, 1024*1024)
		readDone <- readResult{data: data, bytes: bytesRead, callback: callback, err: err}
	}()
	waitForExternalPageCondition(t, time.Second, func() bool {
		return atomic.LoadInt64(&fs.stats.externalPrefetch.waitCount) == 1
	}, "foreground read to join prefetch")
	closeRelease()

	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for joined foreground read")
	}
	if result.callback != nil {
		defer result.callback()
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.bytes != 1024*1024 || len(result.data) == 0 {
		t.Fatalf("unexpected joined read result: bytes=%d segments=%d", result.bytes, len(result.data))
	}
	if got := pageCalls.Load(); got != 1 {
		t.Fatalf("foreground issued a duplicate page lookup: calls=%d", got)
	}
	if got := readIntoCalls.Load(); got != 0 {
		t.Fatalf("foreground issued a duplicate read-into: calls=%d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.waitHits); got != 1 {
		t.Fatalf("unexpected joined-hit metric: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.waitTimeouts); got != 0 {
		t.Fatalf("unexpected join timeout metric: %d", got)
	}
}

func TestExternalCacheForegroundPrefetchJoinTimesOut(t *testing.T) {
	const fileSize = 4 * 1024 * 1024
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(pagePath, fileSize); err != nil {
		t.Fatal(err)
	}

	prefetchStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	var pageCalls atomic.Int64
	var readIntoCalls atomic.Int64
	flags := cfg.DefaultFlags()
	flags.ExternalCacheReadTimeout = time.Second
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if pageCalls.Add(1) == 1 {
				close(prefetchStarted)
				<-release
				return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: int(length)}}, nil
			}
			return nil, errContentNotFound
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			readIntoCalls.Add(1)
			for i := range dst {
				dst[i] = 'r'
			}
			return int64(len(dst)), nil
		},
	}
	fs := newUnitFS(flags)
	defer func() {
		closeRelease()
		fs.closeExternalPageMmapCache()
	}()
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = fileSize
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)
	cache := fs.externalPageCache()
	cache.prefetchWindow(fs, "hash", 0, fileSize, fileSize, flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), nil)

	select {
	case <-prefetchStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prefetch to start")
	}
	started := time.Now()
	data, bytesRead, callback, err := fh.ReadFileWithCallback(0, 1024*1024)
	if callback != nil {
		defer callback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < externalPagePrefetchMaxWait || elapsed > 750*time.Millisecond {
		t.Fatalf("prefetch join did not fail open promptly: %s", elapsed)
	}
	if bytesRead != 1024*1024 || len(data) != 1 || data[0][0] != 'r' {
		t.Fatalf("unexpected fail-open read result: bytes=%d segments=%d", bytesRead, len(data))
	}
	if got := pageCalls.Load(); got != 2 {
		t.Fatalf("expected prefetch and foreground page lookups, got %d", got)
	}
	if got := readIntoCalls.Load(); got != 1 {
		t.Fatalf("expected one fail-open read-into, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.waitTimeouts); got != 1 {
		t.Fatalf("unexpected join-timeout metric: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.waitHits); got != 0 {
		t.Fatalf("unexpected joined-hit metric: %d", got)
	}

	closeRelease()
	waitForExternalPageCondition(t, time.Second, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.prefetchActive == 0
	}, "timed-out prefetch to finish")
}

func waitForExternalPageCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

var externalPageBenchmarkSink byte

func BenchmarkExternalPageMmapLookupCopySequential1MiB(b *testing.B) {
	const (
		windowSize = 64 * 1024 * 1024
		readSize   = 1024 * 1024
	)
	pagePath := filepath.Join(b.TempDir(), "page")
	if err := os.WriteFile(pagePath, nil, 0644); err != nil {
		b.Fatal(err)
	}
	if err := os.Truncate(pagePath, windowSize); err != nil {
		b.Fatal(err)
	}
	cache := newExternalPageMmapCache(2 * windowSize)
	defer cache.close()
	if err := cache.insertWindow("hash", 0, []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: windowSize}}); err != nil {
		b.Fatal(err)
	}
	dst := make([]byte, readSize)
	b.ReportAllocs()
	b.SetBytes(readSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		offset := uint64((i * readSize) % windowSize)
		data, cleanup, ok := cache.lookup("hash", offset, readSize)
		if !ok {
			b.Fatal("mmap cache lookup missed")
		}
		written := 0
		for _, segment := range data {
			written += copy(dst[written:], segment)
		}
		if cleanup != nil {
			cleanup()
		}
		externalPageBenchmarkSink ^= dst[written-1]
	}
	b.ReportMetric(float64(b.N*readSize)/(1024*1024)/b.Elapsed().Seconds(), "MiB/s")
}
