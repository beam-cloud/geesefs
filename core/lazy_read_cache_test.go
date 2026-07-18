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

func TestLazyReadPartialAndOutOfOrderReadsAreAbandoned(t *testing.T) {
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
		{
			name: "nonzero first read",
			read: func(t *testing.T, fh *FileHandle) {
				if _, _, _, err := fh.ReadFileWithCallback(4, 4); err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := fh.ReadFileWithCallback(0, int64(len(payload))); err != nil {
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
