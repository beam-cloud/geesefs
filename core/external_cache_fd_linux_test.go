//go:build linux

package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/yandex-cloud/geesefs/core/cfg"
	"golang.org/x/sys/unix"
)

type contentCacheWithoutLocalViews struct {
	cfg.ContentCache
}

func newExternalFDTestFile(t *testing.T, payload []byte, cache cfg.ContentCache) (*Goofys, *Inode, *FileHandle) {
	t.Helper()
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = cache
	fs := newUnitFS(flags)
	fs.externalCacheFDReads = true
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(payload))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)
	return fs, inode, fh
}

func TestExternalCacheFDReadRequiresOptInAndLocalPageViews(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &contentCacheWithoutLocalViews{ContentCache: &fakeContentCache{}}
	fs := newUnitFS(flags)
	fs.externalCacheFDReads = true
	if fs.externalCacheFDReadsEnabled() {
		t.Fatal("FD reads enabled without local page-file views")
	}

	flags.ExternalCacheClient = &fakeContentCache{}
	if !fs.externalCacheFDReadsEnabled() {
		t.Fatal("FD reads disabled with mount opt-in and local page-file views")
	}
	fs.externalCacheFDReads = false
	if fs.externalCacheFDReadsEnabled() {
		t.Fatal("FD reads enabled without mount opt-in")
	}
}

func TestExternalCacheFDReadReturnsDedicatedDescriptorUntilCallback(t *testing.T) {
	payload := []byte("payload")
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, append([]byte("xx"), append(payload, []byte("yy")...)...), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || length != int64(len(payload)) {
				t.Fatalf("unexpected page view request: hash=%q routing=%q offset=%d length=%d", hash, opts.RoutingKey, offset, length)
			}
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 2, Length: len(payload)}}, nil
		},
	}
	fs, inode, fh := newExternalFDTestFile(t, payload, cache)
	fs.fileHandles = map[fuseops.HandleID]*FileHandle{7: fh}
	fuseFS := NewGoofysFuse(fs)
	op := &fuseops.ReadFileOp{Inode: fuseops.InodeID(inode.Id), Handle: 7, Offset: 0, Size: int64(len(payload))}
	if err := fuseFS.ReadFile(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if op.FD == nil {
		t.Fatal("expected local page-file descriptor")
	}
	if len(op.Data) != 0 || op.BytesRead != len(payload) || op.Callback == nil {
		t.Fatalf("unexpected FD response: data=%d bytes=%d callback=%v", len(op.Data), op.BytesRead, op.Callback != nil)
	}

	got := make([]byte, len(payload))
	n, err := unix.Pread(int(op.FD.FD), got, op.FD.Offset)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("descriptor data mismatch: read=%d data=%q", n, got)
	}

	op.FD.Transfer = fuseops.ReadFileFDTransferPread
	op.FD.BytesTransferred = len(payload)
	op.FD.SpliceFallback = errors.New("test splice fallback")
	op.Callback()
	op.Callback()
	if _, err := unix.Pread(int(op.FD.FD), got, op.FD.Offset); err == nil {
		t.Fatal("page-file descriptor remained open after reply callback")
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.hits); got != 1 {
		t.Fatalf("FD hits=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.bytes); got != int64(len(payload)) {
		t.Fatalf("FD bytes=%d, want %d", got, len(payload))
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.preadTransfers); got != 1 {
		t.Fatalf("pread transfers=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.transferBytes); got != int64(len(payload)) {
		t.Fatalf("transport bytes=%d, want %d", got, len(payload))
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.spliceFallbacks); got != 1 {
		t.Fatalf("splice fallbacks=%d, want 1", got)
	}
}

func TestExternalCacheFDReadPrefersExistingMappedData(t *testing.T) {
	payload := []byte("mapped")
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	var viewCalls atomic.Int64
	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			viewCalls.Add(1)
			return nil, errContentNotFound
		},
	}
	fs, _, fh := newExternalFDTestFile(t, payload, cache)
	defer fs.closeExternalPageMmapCache()
	if err := fs.externalPageCache().insertWindow("hash", 0, []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: len(payload)}}); err != nil {
		t.Fatal(err)
	}

	data, bytesRead, fdRead, callback, err := fh.readFileWithCallback(0, int64(len(payload)), true)
	if err != nil {
		t.Fatal(err)
	}
	if callback != nil {
		defer callback()
	}
	if fdRead != nil {
		t.Fatal("FD path bypassed an existing mapped cache hit")
	}
	if bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("mapped response mismatch: bytes=%d data=%q", bytesRead, bytes.Join(data, nil))
	}
	if got := viewCalls.Load(); got != 0 {
		t.Fatalf("page view lookup ran despite mapped hit: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.attempts); got != 0 {
		t.Fatalf("FD attempts=%d, want 0", got)
	}
}

func TestExternalCacheFDReadMultiViewFallsThroughToMmap(t *testing.T) {
	left := []byte("abc")
	right := []byte("def")
	dir := t.TempDir()
	leftPath := filepath.Join(dir, "left")
	rightPath := filepath.Join(dir, "right")
	if err := os.WriteFile(leftPath, left, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rightPath, right, 0o644); err != nil {
		t.Fatal(err)
	}
	var viewCalls atomic.Int64
	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			viewCalls.Add(1)
			return []cfg.ClientLocalPageFileView{
				{Path: leftPath, Offset: 0, Length: len(left)},
				{Path: rightPath, Offset: 0, Length: len(right)},
			}, nil
		},
	}
	payload := append(append([]byte(nil), left...), right...)
	fs, _, fh := newExternalFDTestFile(t, payload, cache)
	defer fs.closeExternalPageMmapCache()

	data, bytesRead, fdRead, callback, err := fh.readFileWithCallback(0, int64(len(payload)), true)
	if err != nil {
		t.Fatal(err)
	}
	if callback != nil {
		defer callback()
	}
	if fdRead != nil {
		t.Fatal("multi-view response incorrectly used the FD path")
	}
	if bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("fallback response mismatch: bytes=%d data=%q", bytesRead, bytes.Join(data, nil))
	}
	if got := viewCalls.Load(); got != 2 {
		t.Fatalf("page view calls=%d, want exact FD lookup plus unchanged mmap lookup", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.fallbacks); got != 1 {
		t.Fatalf("FD fallbacks=%d, want 1", got)
	}
}

func TestExternalCacheFDReadRevalidatesInodeAfterOpen(t *testing.T) {
	payload := []byte("abc")
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Inode)
	}{
		{name: "hash", mutate: func(inode *Inode) { inode.userMetadata[inode.fs.flags.HashAttr] = []byte("replacement") }},
		{name: "size", mutate: func(inode *Inode) { inode.Attributes.Size++ }},
		{name: "staged", mutate: func(inode *Inode) { inode.StagedFile = &StagedFile{} }},
		{name: "dirty buffers", mutate: func(inode *Inode) { inode.buffers.Add(0, []byte("x"), BUF_DIRTY, true) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache := &fakeContentCache{
				clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
					return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: len(payload)}}, nil
				},
			}
			fs, inode, fh := newExternalFDTestFile(t, payload, cache)
			externalCacheFDAfterOpenHook = func(_ *FileHandle) {
				inode.mu.Lock()
				test.mutate(inode)
				inode.mu.Unlock()
			}
			fdRead, callback, ok := fh.tryReadExternalCacheFD(cache, "file", "hash", 0, uint64(len(payload)), uint64(len(payload)), time.Now())
			externalCacheFDAfterOpenHook = nil
			if callback != nil {
				callback()
			}
			if ok || fdRead != nil {
				t.Fatal("FD read survived an inode mutation after open")
			}
			if got := atomic.LoadInt64(&fs.stats.externalFDRead.revalidationRaces); got != 1 {
				t.Fatalf("revalidation races=%d, want 1", got)
			}
		})
	}
}

func TestExternalCacheFDPrefetchUsesPageFileHintsOnly(t *testing.T) {
	payload := []byte("prefetch")
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	var viewCalls atomic.Int64
	var readIntoCalls atomic.Int64
	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			viewCalls.Add(1)
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: len(payload)}}, nil
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			readIntoCalls.Add(1)
			copy(dst, payload)
			return int64(len(payload)), nil
		},
	}
	fs, _, fh := newExternalFDTestFile(t, payload, cache)
	defer fs.closeExternalPageMmapCache()
	pageCache := cfg.ContentCacheClientLocalPageFileViews(cache)
	readInto := cfg.ContentCacheReadInto(cache)
	fh.scheduleExternalPagePrefetch("hash", 0, uint64(len(payload)), pageCache, readInto)
	cacheState := fs.externalPageCache()
	waitForExternalPageCondition(t, time.Second, func() bool {
		cacheState.mu.Lock()
		defer cacheState.mu.Unlock()
		return cacheState.prefetchActive == 0 && len(cacheState.prefetchQueue) == 0
	}, "FD page-file hint prefetch")

	if got := viewCalls.Load(); got != 1 {
		t.Fatalf("page view calls=%d, want 1", got)
	}
	if got := readIntoCalls.Load(); got != 0 {
		t.Fatalf("read-into calls=%d, want 0", got)
	}
	cacheState.mu.Lock()
	entries := len(cacheState.entries)
	mappedBytes := cacheState.mappedBytes
	inflightBytes := cacheState.inflightBytes
	cacheState.mu.Unlock()
	if entries != 0 || mappedBytes != 0 || inflightBytes != 0 {
		t.Fatalf("FD prefetch built resident windows: entries=%d mapped=%d inflight=%d", entries, mappedBytes, inflightBytes)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.prefetchHints); got != 1 {
		t.Fatalf("prefetch hints=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.prefetchHintBytes); got != int64(len(payload)) {
		t.Fatalf("prefetch hint bytes=%d, want %d", got, len(payload))
	}
}

func TestExternalCacheFDReadDoesNotWaitForHintOnlyPrefetch(t *testing.T) {
	const (
		fileSize = uint64(externalPageMmapWindowBytes)
		readSize = uint64(1024 * 1024)
	)
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(pagePath, int64(fileSize)); err != nil {
		t.Fatal(err)
	}

	prefetchStarted := make(chan struct{})
	releasePrefetch := make(chan struct{})
	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			if offset == 0 && length == int64(fileSize) {
				close(prefetchStarted)
				<-releasePrefetch
				return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: int(fileSize)}}, nil
			}
			if offset == 0 && length == int64(readSize) {
				return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: 0, Length: int(readSize)}}, nil
			}
			return nil, errContentNotFound
		},
	}
	fs, inode, fh := newExternalFDTestFile(t, nil, cache)
	inode.Attributes.Size = fileSize
	defer fs.closeExternalPageMmapCache()
	fh.scheduleExternalPagePrefetch("hash", 0, fileSize, cache, nil)
	select {
	case <-prefetchStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hint-only prefetch")
	}

	started := time.Now()
	data, bytesRead, fdRead, callback, err := fh.readFileWithCallback(0, int64(readSize), true)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if callback != nil {
		defer callback()
	}
	if len(data) != 0 || bytesRead != int(readSize) || fdRead == nil {
		t.Fatalf("unexpected FD response while hint was active: data=%d bytes=%d fd=%v", len(data), bytesRead, fdRead != nil)
	}
	if elapsed >= externalPagePrefetchMaxWait/2 {
		t.Fatalf("FD foreground read waited for hint-only prefetch: %s", elapsed)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.waitCount); got != 0 {
		t.Fatalf("FD foreground read joined hint-only prefetch %d times", got)
	}

	close(releasePrefetch)
	waitForExternalPageCondition(t, 2*time.Second, func() bool {
		cacheState := fs.externalPageCache()
		cacheState.mu.Lock()
		defer cacheState.mu.Unlock()
		return cacheState.prefetchActive == 0
	}, "hint-only prefetch to finish")
}

func TestExternalCacheFDPrefetchFailedHintIsNotCountedAsSuccess(t *testing.T) {
	payload := []byte("prefetch")
	missingPath := filepath.Join(t.TempDir(), "missing-page")
	var readIntoCalls atomic.Int64
	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			return []cfg.ClientLocalPageFileView{{Path: missingPath, Offset: 0, Length: len(payload)}}, nil
		},
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			readIntoCalls.Add(1)
			return int64(len(dst)), nil
		},
	}
	fs, _, fh := newExternalFDTestFile(t, payload, cache)
	defer fs.closeExternalPageMmapCache()
	fh.scheduleExternalPagePrefetch("hash", 0, uint64(len(payload)), cache, cache)
	cacheState := fs.externalPageCache()
	waitForExternalPageCondition(t, time.Second, func() bool {
		cacheState.mu.Lock()
		defer cacheState.mu.Unlock()
		return cacheState.prefetchActive == 0
	}, "failed FD page-file hint")

	if got := readIntoCalls.Load(); got != 0 {
		t.Fatalf("failed FD hint fell back to read-into %d times", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.prefetchHints); got != 0 {
		t.Fatalf("failed hints counted as successes: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.prefetchHintBytes); got != 0 {
		t.Fatalf("failed hint bytes counted as successes: %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalFDRead.prefetchHintFailures); got != 1 {
		t.Fatalf("hint failures=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalPrefetch.misses); got != 1 {
		t.Fatalf("prefetch misses=%d, want 1", got)
	}
}

func TestExternalCacheFDPrefetchKeepsTwoAlignedWindowsAhead(t *testing.T) {
	const window = int64(externalPageMmapWindowBytes)
	fileSize := uint64(4 * window)
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(pagePath, int64(fileSize)); err != nil {
		t.Fatal(err)
	}

	type request struct {
		offset int64
		length int64
	}
	started := make(chan request, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	cache := &fakeContentCache{
		clientLocalPageFileViews: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
			started <- request{offset: offset, length: length}
			<-release
			return []cfg.ClientLocalPageFileView{{Path: pagePath, Offset: offset, Length: int(length)}}, nil
		},
	}
	fs, inode, fh := newExternalFDTestFile(t, nil, cache)
	inode.Attributes.Size = fileSize
	defer fs.closeExternalPageMmapCache()
	pageCache := cfg.ContentCacheClientLocalPageFileViews(cache)

	fh.scheduleExternalPagePrefetch("hash", 0, fileSize, pageCache, nil)
	wantInitial := map[int64]int64{0: window, window: window}
	for len(wantInitial) > 0 {
		select {
		case got := <-started:
			wantLength, ok := wantInitial[got.offset]
			if !ok || got.length != wantLength {
				t.Fatalf("unexpected initial FD lookahead: %+v", got)
			}
			delete(wantInitial, got.offset)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %d initial FD lookahead windows", len(wantInitial))
		}
	}
	for i := 0; i < 32; i++ {
		fh.scheduleExternalPagePrefetch("hash", 0, fileSize, pageCache, nil)
	}
	cacheState := fs.externalPageCache()
	cacheState.mu.Lock()
	active := cacheState.prefetchActive
	queued := len(cacheState.prefetchQueue)
	inflight := len(cacheState.prefetching)
	cacheState.mu.Unlock()
	if active != 2 || queued != 0 || inflight != 2 {
		t.Fatalf("FD lookahead flooded scheduler: active=%d queued=%d inflight=%d", active, queued, inflight)
	}
	select {
	case got := <-started:
		t.Fatalf("repeated FD lookahead scheduled an extra window: %+v", got)
	default:
	}
	fh.externalPrefetchMu.Lock()
	next := fh.externalPrefetchNext
	fh.externalPrefetchMu.Unlock()
	if next != uint64(2*window) {
		t.Fatalf("initial FD lookahead frontier=%d, want %d", next, 2*window)
	}
	closeRelease()
	waitForExternalPageCondition(t, 2*time.Second, func() bool {
		cacheState.mu.Lock()
		defer cacheState.mu.Unlock()
		return cacheState.prefetchActive == 0
	}, "initial FD lookahead to finish")

	fh.scheduleExternalPagePrefetch("hash", uint64(window), fileSize, pageCache, nil)
	select {
	case got := <-started:
		if got.offset != 2*window || got.length != window {
			t.Fatalf("advanced FD lookahead=%+v, want offset=%d length=%d", got, 2*window, window)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for advanced FD lookahead")
	}
	fh.externalPrefetchMu.Lock()
	next = fh.externalPrefetchNext
	fh.externalPrefetchMu.Unlock()
	if next != uint64(3*window) {
		t.Fatalf("advanced FD lookahead frontier=%d, want %d", next, 3*window)
	}
	waitForExternalPageCondition(t, 2*time.Second, func() bool {
		cacheState.mu.Lock()
		defer cacheState.mu.Unlock()
		return cacheState.prefetchActive == 0
	}, "advanced FD lookahead to finish")
	select {
	case got := <-started:
		t.Fatalf("advanced FD lookahead scheduled beyond its two-window horizon: %+v", got)
	default:
	}
}
