//go:build !windows

package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

type lazyReadStoreCall struct {
	path       string
	cachePath  string
	routingKey string
	lock       bool
	content    []byte
}

func newLazyReadTestFile(t *testing.T, payload []byte, stagedWritePath string, cache cfg.ContentCache, backend *TestBackend) (*Goofys, *Inode, *FileHandle) {
	t.Helper()
	flags := cfg.DefaultFlags()
	flags.CacheThroughModeEnabled = true
	flags.ExternalCacheClient = cache
	flags.HashAttr = "sha256"
	flags.MinFileSizeForHashKB = 0
	flags.StagedWritePath = stagedWritePath
	flags.ReadAheadKB = 0
	flags.ReadAheadLargeKB = 0
	flags.ReadAheadSmallKB = 0

	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "model.bin")
	inode.Id = 2
	inode.Attributes.Size = uint64(len(payload))
	inode.knownSize = uint64(len(payload))
	inode.knownETag = "etag-v1"
	inode.SetCacheState(ST_CACHED)
	inode.userMetadata = map[string][]byte{"content-type": []byte("application/octet-stream")}
	fh, err := inode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}
	return fs, inode, fh
}

func stableLazyReadBackend(payload []byte, copyFn func(*CopyBlobInput) (*CopyBlobOutput, error)) *TestBackend {
	etag := "etag-v1"
	return &TestBackend{
		HeadBlobFunc: func(param *HeadBlobInput) (*HeadBlobOutput, error) {
			return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
				Key:      &param.Key,
				ETag:     &etag,
				Size:     uint64(len(payload)),
				Metadata: map[string]*string{},
			}}, nil
		},
		GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
			end := param.Start + param.Count
			if end > uint64(len(payload)) {
				return nil, fmt.Errorf("read beyond payload: start=%d count=%d", param.Start, param.Count)
			}
			return &GetBlobOutput{
				Body: io.NopCloser(bytes.NewReader(payload[param.Start:end])),
				HeadBlobOutput: HeadBlobOutput{BlobItemOutput: BlobItemOutput{
					Key:      &param.Key,
					ETag:     &etag,
					Size:     param.Count,
					Metadata: map[string]*string{},
				}},
			}, nil
		},
		CopyBlobFunc: copyFn,
	}
}

func waitForLazyReadCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal(message)
	}
}

func assertLazyReadStageDirEmpty(t *testing.T, dir string) {
	t.Helper()
	waitForLazyReadCondition(t, time.Second, func() bool {
		entries, err := os.ReadDir(dir)
		return err == nil && len(entries) == 0
	}, "lazy read staging directory was not cleaned")
}

func TestLazyReadSequentiallyStoresLocalContentBeforePublishingHash(t *testing.T) {
	payload := bytes.Repeat([]byte("model-weights-"), 1024)
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	storeCalls := make(chan lazyReadStoreCall, 1)
	copyCalls := make(chan *CopyBlobInput, 1)

	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		content, err := os.ReadFile(source.Path)
		if err != nil {
			return "", err
		}
		storeCalls <- lazyReadStoreCall{
			path:       source.Path,
			cachePath:  source.CachePath,
			routingKey: opts.RoutingKey,
			lock:       opts.Lock,
			content:    content,
		}
		return expectedHash, nil
	}}
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyCalls <- param
		return &CopyBlobOutput{}, nil
	})
	fs, inode, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()
	go fs.processCacheEvents()

	split := len(payload) / 3
	for offset, size := range []int{split, split, len(payload) - 2*split} {
		readOffset := 0
		if offset == 1 {
			readOffset = split
		} else if offset == 2 {
			readOffset = 2 * split
		}
		data, bytesRead, cleanup, err := fh.ReadFileWithCallback(int64(readOffset), int64(size))
		if cleanup != nil {
			cleanup()
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytesRead != size || !bytes.Equal(bytes.Join(data, nil), payload[readOffset:readOffset+size]) {
			t.Fatalf("unexpected foreground read at offset %d", readOffset)
		}
	}

	var stored lazyReadStoreCall
	select {
	case stored = <-storeCalls:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for local cache store")
	}
	if stored.cachePath != "model.bin" || stored.routingKey != expectedHash || !stored.lock {
		t.Fatalf("unexpected local cache store: %+v", stored)
	}
	if !bytes.Equal(stored.content, payload) {
		t.Fatal("local cache store did not receive the foreground bytes")
	}

	var copied *CopyBlobInput
	select {
	case copied = <-copyCalls:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for guarded hash metadata publish")
	}
	if copied.ETag == nil || *copied.ETag != "etag-v1" || copied.Size == nil || *copied.Size != uint64(len(payload)) {
		t.Fatalf("metadata publish was not guarded by the read identity: %+v", copied)
	}
	if copied.Metadata == nil || copied.Metadata[fs.flags.HashAttr] == nil || *copied.Metadata[fs.flags.HashAttr] != expectedHash {
		t.Fatalf("metadata publish did not contain the computed hash: %+v", copied.Metadata)
	}
	waitForLazyReadCondition(t, time.Second, func() bool {
		_, err := os.Stat(stored.path)
		return os.IsNotExist(err)
	}, "expected staged source to be removed")
	inode.mu.Lock()
	gotHash := string(inode.userMetadata[fs.flags.HashAttr])
	inode.mu.Unlock()
	if gotHash != expectedHash {
		t.Fatalf("inode hash = %q, want %q", gotHash, expectedHash)
	}
}

func TestLazyReadIncompleteReadsAreAbandoned(t *testing.T) {
	payload := []byte("0123456789abcdef")
	for _, test := range []struct {
		name string
		read func(t *testing.T, fh *FileHandle)
	}{
		{
			name: "partial close",
			read: func(t *testing.T, fh *FileHandle) {
				_, _, _, err := fh.ReadFileWithCallback(0, 4)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "gap",
			read: func(t *testing.T, fh *FileHandle) {
				if _, _, _, err := fh.ReadFileWithCallback(0, 4); err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := fh.ReadFileWithCallback(6, 4); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stageDir := t.TempDir()
			var stores atomic.Int32
			var copies atomic.Int32
			cache := &fakeContentCache{storeLocalPath: func(source struct {
				Path      string
				CachePath string
			}, opts struct {
				RoutingKey string
				Lock       bool
			}) (string, error) {
				stores.Add(1)
				return opts.RoutingKey, nil
			}}
			backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
				copies.Add(1)
				return &CopyBlobOutput{}, nil
			})
			fs, _, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
			test.read(t, fh)
			fh.Release()
			close(fs.shutdownCh)
			assertLazyReadStageDirEmpty(t, stageDir)
			time.Sleep(20 * time.Millisecond)
			if stores.Load() != 0 || copies.Load() != 0 {
				t.Fatalf("abandoned read stored=%d copied=%d", stores.Load(), copies.Load())
			}
		})
	}
}

func TestLazyReadOutOfOrderOverlappingCompletionsPromote(t *testing.T) {
	payload := []byte("0123456789abcdef")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	storedContent := make(chan []byte, 1)
	copyDone := make(chan struct{}, 1)
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		content, err := os.ReadFile(source.Path)
		if err != nil {
			return "", err
		}
		storedContent <- content
		return expectedHash, nil
	}}
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyDone <- struct{}{}
		return &CopyBlobOutput{}, nil
	})
	fs, _, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()
	go fs.processCacheEvents()

	fh.retrieveHashMetadata()
	// Async FUSE reads can complete in a different order than they were issued.
	// The overlap is intentional and must not duplicate bytes in the snapshot.
	fh.recordLazyRead(4, 8, [][]byte{payload[4:12]}, 8, nil)
	fh.recordLazyRead(0, 8, [][]byte{payload[:8]}, 8, nil)
	fh.recordLazyRead(12, 4, [][]byte{payload[12:]}, 4, nil)

	select {
	case got := <-storedContent:
		if !bytes.Equal(got, payload) {
			t.Fatalf("stored content = %q, want %q", got, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for out-of-order lazy cache store")
	}
	select {
	case <-copyDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for out-of-order hash publish")
	}
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestRetrieveHashMetadataDiscardsHeadAfterRename(t *testing.T) {
	payload := []byte("original-object")
	stageDir := t.TempDir()
	headStarted := make(chan struct{})
	releaseHead := make(chan struct{})
	wrongHash := "hash-for-replacement-object"
	backend := stableLazyReadBackend(payload, nil)
	backend.HeadBlobFunc = func(param *HeadBlobInput) (*HeadBlobOutput, error) {
		close(headStarted)
		<-releaseHead
		etag := "etag-replacement"
		return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
			Key:      &param.Key,
			ETag:     &etag,
			Size:     uint64(len(payload)),
			Metadata: map[string]*string{"sha256": &wrongHash},
		}}, nil
	}
	cache := &fakeContentCache{}
	fs, inode, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()

	done := make(chan struct{})
	go func() {
		fh.retrieveHashMetadata()
		close(done)
	}()
	<-headStarted
	inode.mu.Lock()
	inode.Name = "renamed-model.bin"
	inode.mu.Unlock()
	close(releaseHead)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale HEAD rejection")
	}

	inode.mu.Lock()
	gotETag := inode.knownETag
	gotHash := string(inode.userMetadata[fs.flags.HashAttr])
	checked := inode.hashMetadataChecked
	inode.mu.Unlock()
	if gotETag != "etag-v1" || gotHash != "" || checked {
		t.Fatalf("stale HEAD mutated renamed inode: etag=%q hash=%q checked=%t", gotETag, gotHash, checked)
	}
}

func TestLazyReadStagedByteAdmissionReleasesOnAbandon(t *testing.T) {
	payload := []byte("0123456789abcdef")
	stageDir := t.TempDir()
	cache := &fakeContentCache{}
	backend := stableLazyReadBackend(payload, nil)
	fs, firstInode, first := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	fs.lazyReadStageLimitBytes = uint64(len(payload))
	firstInode.hashMetadataChecked = true

	secondInode := NewInode(fs, firstInode.Parent, "other-model.bin")
	secondInode.Id = 3
	secondInode.Attributes.Size = uint64(len(payload))
	secondInode.knownSize = uint64(len(payload))
	secondInode.knownETag = "etag-v1"
	secondInode.SetCacheState(ST_CACHED)
	secondInode.userMetadata = map[string][]byte{}
	secondInode.hashMetadataChecked = true
	second, err := secondInode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}

	first.recordLazyRead(0, 4, [][]byte{payload[:4]}, 4, nil)
	second.recordLazyRead(0, 4, [][]byte{payload[:4]}, 4, nil)
	if first.lazyReadStage == nil || second.lazyReadStage != nil || !second.lazyReadDisabled {
		t.Fatalf("unexpected admission state: first_stage=%t second_stage=%t second_disabled=%t", first.lazyReadStage != nil, second.lazyReadStage != nil, second.lazyReadDisabled)
	}
	fs.lazyReadClaimsMu.Lock()
	reserved := fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if reserved != uint64(len(payload)) {
		t.Fatalf("reserved staged bytes = %d, want %d", reserved, len(payload))
	}

	first.Release()
	fs.lazyReadClaimsMu.Lock()
	reserved = fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if reserved != 0 {
		t.Fatalf("reserved staged bytes after abandon = %d, want 0", reserved)
	}
	third, err := secondInode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}
	third.recordLazyRead(0, 4, [][]byte{payload[:4]}, 4, nil)
	if third.lazyReadStage == nil {
		t.Fatal("released byte admission was not reusable")
	}
	second.Release()
	third.Release()
	close(fs.shutdownCh)
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadStageCountAdmissionReleasesClaim(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	fs.lazyReadStageLimitBytes = 1024
	fs.lazyReadStageLimitCount = 1
	first := lazyReadIdentity{path: "one", objectPath: "one", etag: "etag-one", size: 10}
	second := lazyReadIdentity{path: "two", objectPath: "two", etag: "etag-two", size: 10}
	if !fs.claimLazyRead(first) {
		t.Fatal("first lazy claim was unexpectedly rejected")
	}
	if fs.claimLazyRead(second) {
		t.Fatal("second lazy claim exceeded the count limit")
	}
	fs.releaseLazyReadClaim(first)
	if !fs.claimLazyRead(second) {
		t.Fatal("released lazy claim count was not reusable")
	}
	fs.releaseLazyReadClaim(second)
	fs.lazyReadClaimsMu.Lock()
	claimCount := len(fs.lazyReadClaims)
	reserved := fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if claimCount != 0 || reserved != 0 {
		t.Fatalf("lazy admission leaked after release: claims=%d bytes=%d", claimCount, reserved)
	}
	close(fs.shutdownCh)
}

func TestLazyReadCompletionAfterShutdownIsAbandoned(t *testing.T) {
	payload := []byte("complete-after-shutdown")
	stageDir := t.TempDir()
	cache := &fakeContentCache{}
	backend := stableLazyReadBackend(payload, nil)
	fs, inode, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	inode.hashMetadataChecked = true
	split := len(payload) / 2
	fh.recordLazyRead(0, uint64(split), [][]byte{payload[:split]}, split, nil)
	if fh.lazyReadStage == nil {
		t.Fatal("partial lazy stage was not created")
	}
	fs.Shutdown()
	fh.recordLazyRead(uint64(split), uint64(len(payload)-split), [][]byte{payload[split:]}, len(payload)-split, nil)
	if fh.lazyReadStage != nil || !fh.lazyReadDisabled {
		t.Fatalf("late completion was not abandoned: stage=%t disabled=%t", fh.lazyReadStage != nil, fh.lazyReadDisabled)
	}
	if active := atomic.LoadInt64(&fs.activeCacheEvents); active != 0 {
		t.Fatalf("active cache events after late completion = %d, want 0", active)
	}
	fs.lazyReadClaimsMu.Lock()
	claimCount := len(fs.lazyReadClaims)
	reserved := fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if claimCount != 0 || reserved != 0 {
		t.Fatalf("late completion leaked admission: claims=%d bytes=%d", claimCount, reserved)
	}
	fh.Release()
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadConcurrentHandlesShareObjectIdentityClaim(t *testing.T) {
	payload := bytes.Repeat([]byte("one-object-one-stage-"), 256)
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	var stores atomic.Int32
	copyDone := make(chan struct{}, 1)
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		stores.Add(1)
		return expectedHash, nil
	}}
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyDone <- struct{}{}
		return &CopyBlobOutput{}, nil
	})
	fs, inode, first := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer first.Release()
	go fs.processCacheEvents()
	second, err := inode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	split := len(payload) / 2
	if _, _, _, err := first.ReadFileWithCallback(0, int64(split)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(stageDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one claimed stage after the first partial read, entries=%d err=%v", len(entries), err)
	}

	data, bytesRead, cleanup, err := second.ReadFileWithCallback(0, int64(len(payload)))
	if cleanup != nil {
		cleanup()
	}
	if err != nil || bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("second foreground reader changed: bytes=%d err=%v", bytesRead, err)
	}
	entries, err = os.ReadDir(stageDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("second handle created a duplicate stage, entries=%d err=%v", len(entries), err)
	}

	if _, _, _, err := first.ReadFileWithCallback(int64(split), int64(len(payload)-split)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-copyDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lazy metadata publish")
	}
	if stores.Load() != 1 {
		t.Fatalf("expected one local cache store, got %d", stores.Load())
	}
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadFinalBytesWithEOFStillPromote(t *testing.T) {
	payload := []byte("final-bytes-with-eof")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	storeDone := make(chan struct{}, 1)
	copyDone := make(chan struct{}, 1)
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		storeDone <- struct{}{}
		return expectedHash, nil
	}}
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyDone <- struct{}{}
		return &CopyBlobOutput{}, nil
	})
	fs, _, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()
	go fs.processCacheEvents()

	fh.retrieveHashMetadata()
	fh.recordLazyRead(0, uint64(len(payload)), [][]byte{payload}, len(payload), io.EOF)
	select {
	case <-storeDone:
	case <-time.After(time.Second):
		t.Fatal("EOF final bytes did not populate the cache")
	}
	select {
	case <-copyDone:
	case <-time.After(time.Second):
		t.Fatal("EOF final bytes did not publish hash metadata")
	}
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadFinishIsCountedUntilCacheEventCompletes(t *testing.T) {
	payload := []byte("count-finish-before-queue")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	revalidationStarted := make(chan struct{})
	releaseRevalidation := make(chan struct{})
	copyDone := make(chan struct{}, 1)
	var headCalls atomic.Int32
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyDone <- struct{}{}
		return &CopyBlobOutput{}, nil
	})
	backend.HeadBlobFunc = func(param *HeadBlobInput) (*HeadBlobOutput, error) {
		if headCalls.Add(1) == 2 {
			close(revalidationStarted)
			<-releaseRevalidation
		}
		etag := "etag-v1"
		return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
			Key: &param.Key, ETag: &etag, Size: uint64(len(payload)), Metadata: map[string]*string{},
		}}, nil
	}
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		return expectedHash, nil
	}}
	fs, _, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	fs.flags.StagedWriteFlushTimeout = 3 * time.Second
	defer close(fs.shutdownCh)
	defer fh.Release()
	go fs.processCacheEvents()

	if _, _, cleanup, err := fh.ReadFileWithCallback(0, int64(len(payload))); err != nil {
		t.Fatal(err)
	} else if cleanup != nil {
		cleanup()
	}
	select {
	case <-revalidationStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lazy revalidation")
	}

	flushDone := make(chan struct{})
	go func() {
		fs.WaitForFlush()
		close(flushDone)
	}()
	select {
	case <-flushDone:
		t.Fatal("WaitForFlush returned while lazy finish was not yet queued")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseRevalidation)
	select {
	case <-flushDone:
	case <-time.After(4 * time.Second):
		t.Fatal("WaitForFlush did not observe lazy cache completion")
	}
	select {
	case <-copyDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for counted metadata publish")
	}
	if active := atomic.LoadInt64(&fs.activeCacheEvents); active != 0 {
		t.Fatalf("active cache events = %d, want 0", active)
	}
	fs.lazyReadClaimsMu.Lock()
	reserved := fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if reserved != 0 {
		t.Fatalf("reserved staged bytes = %d, want 0", reserved)
	}
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadShutdownDrainsPrecountedCacheEvent(t *testing.T) {
	payload := []byte("queued-after-shutdown")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	stagePath := filepath.Join(stageDir, "completed-lazy-read")
	if err := os.WriteFile(stagePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		return expectedHash, nil
	}}
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = cache
	flags.StagedWritePath = stageDir
	fs := newUnitFS(flags)
	identity := lazyReadIdentity{path: "model.bin", objectPath: "model.bin", etag: "etag-v1", size: uint64(len(payload))}
	if !fs.claimLazyRead(identity) {
		t.Fatal("failed to reserve lazy read claim")
	}
	atomic.AddInt64(&fs.activeCacheEvents, 1)

	processorDone := make(chan struct{})
	go func() {
		fs.processCacheEvents()
		close(processorDone)
	}()
	close(fs.shutdownCh)
	select {
	case <-processorDone:
		t.Fatal("cache processor exited before the precounted lazy event handoff")
	case <-time.After(25 * time.Millisecond):
	}
	fs.cacheEventChan <- cacheEvent{
		path:             identity.path,
		size:             identity.size,
		hash:             expectedHash,
		localSourcePath:  stagePath,
		removeLocalAfter: true,
		lazyReadIdentity: &identity,
		skipCacheStatus:  true,
		activeCounted:    true,
	}
	select {
	case <-processorDone:
	case <-time.After(time.Second):
		t.Fatal("cache processor did not drain the precounted lazy event")
	}
	if active := atomic.LoadInt64(&fs.activeCacheEvents); active != 0 {
		t.Fatalf("active cache events = %d, want 0", active)
	}
	fs.lazyReadClaimsMu.Lock()
	reserved := fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if reserved != 0 {
		t.Fatalf("reserved staged bytes = %d, want 0", reserved)
	}
	if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("completed lazy stage was not removed: %v", err)
	}
}

func TestLazyReadCacheTriggerCallbackCanShutdown(t *testing.T) {
	payload := []byte("callback-shutdown")
	stageDir := t.TempDir()
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		return &CopyBlobOutput{}, nil
	})
	cache := &fakeContentCache{}
	fs, _, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer fh.Release()
	callbackDone := make(chan struct{})
	fs.flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		fs.Shutdown()
		close(callbackDone)
	}

	if _, _, cleanup, err := fh.ReadFileWithCallback(0, int64(len(payload))); err != nil {
		t.Fatal(err)
	} else if cleanup != nil {
		cleanup()
	}
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("cache-trigger callback deadlocked during shutdown")
	}
	waitForLazyReadCondition(t, time.Second, func() bool {
		return atomic.LoadInt64(&fs.activeCacheEvents) == 0
	}, "lazy cache finish remained active after callback shutdown")
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadRevalidationMetadataIsPreserved(t *testing.T) {
	payload := []byte("metadata-must-follow-revalidation")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	stageDir := t.TempDir()
	copyCalls := make(chan *CopyBlobInput, 1)
	var headCalls atomic.Int32
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyCalls <- param
		return &CopyBlobOutput{}, nil
	})
	backend.HeadBlobFunc = func(param *HeadBlobInput) (*HeadBlobOutput, error) {
		metadataValue := "before-read"
		if headCalls.Add(1) > 1 {
			metadataValue = "at-revalidation"
		}
		etag := "etag-v1"
		return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
			Key:      &param.Key,
			ETag:     &etag,
			Size:     uint64(len(payload)),
			Metadata: map[string]*string{"user-note": &metadataValue},
		}}, nil
	}
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		return expectedHash, nil
	}}
	fs, _, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()
	go fs.processCacheEvents()

	if _, _, cleanup, err := fh.ReadFileWithCallback(0, int64(len(payload))); err != nil {
		t.Fatal(err)
	} else if cleanup != nil {
		cleanup()
	}
	var copied *CopyBlobInput
	select {
	case copied = <-copyCalls:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for revalidated metadata publish")
	}
	if copied.Metadata == nil || copied.Metadata["user-note"] == nil || *copied.Metadata["user-note"] != "at-revalidation" {
		t.Fatalf("metadata publish restored a stale snapshot: %+v", copied.Metadata)
	}
	if copied.Metadata[fs.flags.HashAttr] == nil || *copied.Metadata[fs.flags.HashAttr] != expectedHash {
		t.Fatalf("metadata publish omitted hash: %+v", copied.Metadata)
	}
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadMutationFailsConditionalOriginReadWithoutPublishing(t *testing.T) {
	firstVersion := []byte("version-one-model")
	secondVersion := []byte("version-two-model")
	stageDir := t.TempDir()
	var version atomic.Int32
	version.Store(1)
	ifMatches := make(chan string, 2)
	var stores atomic.Int32
	var copies atomic.Int32
	backend := &TestBackend{
		HeadBlobFunc: func(param *HeadBlobInput) (*HeadBlobOutput, error) {
			etag := "etag-v1"
			if version.Load() == 2 {
				etag = "etag-v2"
			}
			return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
				Key: &param.Key, ETag: &etag, Size: uint64(len(firstVersion)), Metadata: map[string]*string{},
			}}, nil
		},
		GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
			if param.IfMatch == nil {
				ifMatches <- ""
				return nil, errors.New("origin read omitted If-Match")
			}
			ifMatches <- *param.IfMatch
			currentETag := "etag-v1"
			payload := firstVersion
			if version.Load() == 2 {
				currentETag = "etag-v2"
				payload = secondVersion
			}
			if *param.IfMatch != currentETag {
				return nil, syscall.EBUSY
			}
			end := param.Start + param.Count
			return &GetBlobOutput{
				Body: io.NopCloser(bytes.NewReader(payload[param.Start:end])),
				HeadBlobOutput: HeadBlobOutput{BlobItemOutput: BlobItemOutput{
					Key: &param.Key, ETag: &currentETag, Size: param.Count, Metadata: map[string]*string{},
				}},
			}, nil
		},
		CopyBlobFunc: func(param *CopyBlobInput) (*CopyBlobOutput, error) {
			copies.Add(1)
			return &CopyBlobOutput{}, nil
		},
	}
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		stores.Add(1)
		return opts.RoutingKey, nil
	}}
	fs, inode, fh := newLazyReadTestFile(t, firstVersion, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()

	split := len(firstVersion) / 2
	if _, _, cleanup, err := fh.ReadFileWithCallback(0, int64(split)); err != nil {
		t.Fatal(err)
	} else if cleanup != nil {
		cleanup()
	}
	version.Store(2)
	if _, _, cleanup, err := fh.ReadFileWithCallback(int64(split), int64(len(firstVersion)-split)); !errors.Is(err, syscall.EBUSY) {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("mutated origin read error = %v, want EBUSY", err)
	} else if cleanup != nil {
		cleanup()
	}

	if first, second := <-ifMatches, <-ifMatches; first != "etag-v1" || second != "etag-v1" {
		t.Fatalf("conditional reads used %q and %q, want etag-v1", first, second)
	}
	if stores.Load() != 0 || copies.Load() != 0 {
		t.Fatalf("mixed-version bytes reached publication: stores=%d copies=%d", stores.Load(), copies.Load())
	}
	inode.mu.Lock()
	metadataChecked := inode.hashMetadataChecked
	_, _, staleBufferErr := inode.buffers.GetData(0, uint64(split), false)
	inode.mu.Unlock()
	if metadataChecked {
		t.Fatal("precondition conflict retained the stale hash-metadata identity")
	}
	if !errors.Is(staleBufferErr, ErrBufferIsMissing) {
		t.Fatalf("precondition conflict retained stale clean bytes: %v", staleBufferErr)
	}
	fs.lazyReadClaimsMu.Lock()
	reserved := fs.lazyReadStagedBytes
	fs.lazyReadClaimsMu.Unlock()
	if reserved != 0 {
		t.Fatalf("reserved staged bytes = %d, want 0", reserved)
	}
	assertLazyReadStageDirEmpty(t, stageDir)
}

func TestLazyReadIdentityChangePreventsCacheAndMetadataPublish(t *testing.T) {
	payload := []byte("stable-user-visible-bytes")
	stageDir := t.TempDir()
	var headCalls atomic.Int32
	revalidated := make(chan struct{})
	var stores atomic.Int32
	var copies atomic.Int32
	etag1 := "etag-v1"
	etag2 := "etag-v2"
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copies.Add(1)
		return &CopyBlobOutput{}, nil
	})
	backend.HeadBlobFunc = func(param *HeadBlobInput) (*HeadBlobOutput, error) {
		etag := etag1
		if headCalls.Add(1) > 1 {
			etag = etag2
			select {
			case <-revalidated:
			default:
				close(revalidated)
			}
		}
		return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
			Key: &param.Key, ETag: &etag, Size: uint64(len(payload)), Metadata: map[string]*string{},
		}}, nil
	}
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		stores.Add(1)
		return opts.RoutingKey, nil
	}}
	fs, inode, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, int64(len(payload)))
	if cleanup != nil {
		cleanup()
	}
	if err != nil || bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("foreground read changed: bytes=%d err=%v", bytesRead, err)
	}
	select {
	case <-revalidated:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for identity revalidation")
	}
	assertLazyReadStageDirEmpty(t, stageDir)
	if stores.Load() != 0 || copies.Load() != 0 {
		t.Fatalf("changed object stored=%d copied=%d", stores.Load(), copies.Load())
	}
	inode.mu.Lock()
	gotHash := string(inode.userMetadata[fs.flags.HashAttr])
	inode.mu.Unlock()
	if gotHash != "" {
		t.Fatalf("changed object received hash metadata %q", gotHash)
	}
}

func TestLazyReadCacheFailureNeverPublishesMetadata(t *testing.T) {
	payload := []byte("cache-store-failure-must-not-break-read")
	stageDir := t.TempDir()
	var stores atomic.Int32
	var copies atomic.Int32
	originalDelay := externalCacheStoreRetryDelay
	externalCacheStoreRetryDelay = func(int) time.Duration { return 0 }
	defer func() { externalCacheStoreRetryDelay = originalDelay }()

	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		stores.Add(1)
		return "", errors.New("cache unavailable")
	}}
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copies.Add(1)
		return nil, syscall.EBUSY
	})
	fs, inode, fh := newLazyReadTestFile(t, payload, stageDir, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()
	go fs.processCacheEvents()

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, int64(len(payload)))
	if cleanup != nil {
		cleanup()
	}
	if err != nil || bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("foreground read changed: bytes=%d err=%v", bytesRead, err)
	}
	waitForLazyReadCondition(t, time.Second, func() bool {
		return stores.Load() == externalCacheStoreAttempts && atomic.LoadInt64(&fs.stats.cacheEventsErrors) == 1
	}, "cache failure did not finish")
	assertLazyReadStageDirEmpty(t, stageDir)
	if copies.Load() != 0 {
		t.Fatalf("metadata was published despite cache failure: copies=%d", copies.Load())
	}
	inode.mu.Lock()
	gotHash := string(inode.userMetadata[fs.flags.HashAttr])
	inode.mu.Unlock()
	if gotHash != "" {
		t.Fatalf("cache failure exposed hash metadata %q", gotHash)
	}
}

func TestLazyReadStagingFailureDoesNotChangeForegroundRead(t *testing.T) {
	payload := []byte("foreground-read-wins")
	parent := t.TempDir()
	invalidStagePath := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(invalidStagePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stores atomic.Int32
	cache := &fakeContentCache{storeLocalPath: func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		stores.Add(1)
		return opts.RoutingKey, nil
	}}
	backend := stableLazyReadBackend(payload, func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		return &CopyBlobOutput{}, nil
	})
	fs, _, fh := newLazyReadTestFile(t, payload, invalidStagePath, cache, backend)
	defer close(fs.shutdownCh)
	defer fh.Release()

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, int64(len(payload)))
	if cleanup != nil {
		cleanup()
	}
	if err != nil || bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("staging failure changed foreground read: bytes=%d err=%v", bytesRead, err)
	}
	if stores.Load() != 0 {
		t.Fatalf("unexpected cache store after staging failure: %d", stores.Load())
	}
}
