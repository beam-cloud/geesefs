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
	"os"
	"path/filepath"
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

func TestExternalCacheReadIntoUnavailableReturnsError(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	if !isExternalCacheUnavailable(err) {
		t.Fatalf("expected external cache unavailable, got %v", err)
	}
	if ok {
		t.Fatal("unexpected read-into hit")
	}
}

func TestExternalCachePrefetchFallsBackToReadInto(t *testing.T) {
	flags := cfg.DefaultFlags()
	payload := bytes.Repeat([]byte("a"), int(externalPageMmapWindowBytes))
	flags.ExternalCacheClient = &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			return nil, errContentNotFound
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || len(dst) != len(payload) {
				t.Fatalf("unexpected read-into prefetch: hash=%q routing=%q offset=%d len=%d", hash, opts.RoutingKey, offset, len(dst))
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
			return
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for read-into prefetch")
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

func TestExternalCachePrefetchDoesNotSkipWhenQueueFull(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()

	inode := NewInode(fs, nil, "file")
	fh := NewFileHandle(inode)
	cache := fs.externalPageCache()

	for i := 0; i < cap(cache.prefetchSem); i++ {
		cache.prefetchSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(cache.prefetchSem); i++ {
			<-cache.prefetchSem
		}
	}()

	fh.scheduleExternalPagePrefetch("hash", 0, 2*externalPageMmapWindowBytes, flags.ExternalCacheClient.(cfg.ContentCacheClientLocalPageFileViews), nil)
	if fh.externalPrefetchNext != 0 {
		t.Fatalf("prefetch advanced after queue-full drop: got %d", fh.externalPrefetchNext)
	}
}
