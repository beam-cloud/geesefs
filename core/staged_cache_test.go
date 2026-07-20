package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/sirupsen/logrus"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

type fakeContentCache struct {
	getContent       func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error)
	getContentStream func(hash string, offset int64, length int64, opts struct {
		RoutingKey string
	}) (chan []byte, error)
	storeContent func(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error)
	storeFromS3  func(source struct {
		Path        string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error)
	storeLocalPath func(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error)
	lookupObjectContentHash  func(ctx context.Context, identity cfg.ContentCacheObjectIdentity) (string, bool, error)
	storeObjectContentHash   func(ctx context.Context, identity cfg.ContentCacheObjectIdentity, hash string) error
	clientLocalPageFileViews func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error)
	readContentInto          func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error)
}

func (c *fakeContentCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	if c.getContent != nil {
		return c.getContent(hash, offset, length, opts)
	}
	return nil, errContentNotFound
}

func (c *fakeContentCache) GetContentStream(hash string, offset int64, length int64, opts struct {
	RoutingKey string
}) (chan []byte, error) {
	if c.getContentStream != nil {
		return c.getContentStream(hash, offset, length, opts)
	}
	return nil, errContentNotFound
}

func (c *fakeContentCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	if c.storeContent != nil {
		return c.storeContent(chunks, hash, opts)
	}
	for range chunks {
	}
	return hash, nil
}

func (c *fakeContentCache) StoreContentFromS3(source struct {
	Path        string
	BucketName  string
	Region      string
	EndpointURL string
	AccessKey   string
	SecretKey   string
}, opts struct {
	RoutingKey string
	Lock       bool
}) (string, error) {
	if c.storeFromS3 != nil {
		return c.storeFromS3(source, opts)
	}
	return opts.RoutingKey, nil
}

func (c *fakeContentCache) StoreContentFromLocalPath(source struct {
	Path      string
	CachePath string
}, opts struct {
	RoutingKey string
	Lock       bool
}) (string, error) {
	if c.storeLocalPath != nil {
		return c.storeLocalPath(source, opts)
	}
	chunks := make(chan []byte)
	close(chunks)
	return c.StoreContent(chunks, opts.RoutingKey, struct{ RoutingKey string }{RoutingKey: opts.RoutingKey})
}

func (c *fakeContentCache) LookupObjectContentHash(ctx context.Context, identity cfg.ContentCacheObjectIdentity) (string, bool, error) {
	if c.lookupObjectContentHash != nil {
		return c.lookupObjectContentHash(ctx, identity)
	}
	return "", false, nil
}

func (c *fakeContentCache) StoreObjectContentHash(ctx context.Context, identity cfg.ContentCacheObjectIdentity, hash string) error {
	if c.storeObjectContentHash != nil {
		return c.storeObjectContentHash(ctx, identity, hash)
	}
	return nil
}

func (c *fakeContentCache) ClientLocalPageFileViews(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]cfg.ClientLocalPageFileView, error) {
	if c.clientLocalPageFileViews != nil {
		return c.clientLocalPageFileViews(hash, offset, length, opts)
	}
	return nil, errContentNotFound
}

func (c *fakeContentCache) ReadContentInto(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
	if c.readContentInto != nil {
		return c.readContentInto(ctx, hash, offset, dst, opts)
	}
	return 0, errContentNotFound
}

func newUnitFS(flags *cfg.FlagStorage) *Goofys {
	fs := &Goofys{
		bucket:           "bucket",
		flags:            flags,
		shutdownCh:       make(chan struct{}),
		bufferPool:       NewBufferPool(int64(flags.MemoryLimit), uint64(flags.GCInterval)<<20),
		cacheEventChan:   make(chan cacheEvent, 8),
		cacheEventDone:   make(chan struct{}, 1),
		cachingStatus:    make(map[string]bool),
		flushPriorities:  make([]int64, MAX_FLUSH_PRIORITY+1),
		inflightChanges:  make(map[string]int),
		inflightListings: make(map[int]map[string]bool),
		inodesByTime:     make(map[int64]map[fuseops.InodeID]bool),
	}
	fs.flusherCond = sync.NewCond(&fs.flusherMu)
	return fs
}

func newRootWithBackend(fs *Goofys, backend StorageBackend) *Inode {
	root := NewInode(fs, nil, "")
	root.Id = 1
	root.ToDir()
	root.dir.cloud = backend
	root.userMetadata = make(map[string][]byte)
	return root
}

func captureMainLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	output := &bytes.Buffer{}
	originalOutput := log.Out
	originalLevel := log.Level
	log.SetOutput(output)
	log.SetLevel(logrus.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
		log.SetLevel(originalLevel)
	})
	return output
}

func newNoSuchUploadError() error {
	return awserr.NewRequestFailure(
		awserr.New("NoSuchUpload", "The specified upload does not exist", nil),
		404,
		"request-id",
	)
}

func TestProcessCacheEventsDrainsQueuedEventsOnShutdown(t *testing.T) {
	flags := cfg.DefaultFlags()
	var mu sync.Mutex
	stored := make([]string, 0, 2)
	flags.ExternalCacheClient = &fakeContentCache{
		storeLocalPath: func(source struct {
			Path      string
			CachePath string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			mu.Lock()
			stored = append(stored, opts.RoutingKey)
			mu.Unlock()
			return opts.RoutingKey, nil
		},
	}
	fs := newUnitFS(flags)
	fs.cacheEventChan <- cacheEvent{path: "one", hash: "h1", size: 1, localSourcePath: "/tmp/one"}
	fs.cacheEventChan <- cacheEvent{path: "two", hash: "h2", size: 1, localSourcePath: "/tmp/two"}
	close(fs.shutdownCh)

	fs.processCacheEvents()

	if len(stored) != 2 {
		t.Fatalf("expected queued cache events to drain on shutdown, got %v", stored)
	}
}

func TestProcessCacheEventRetriesTransientExternalCacheStoreError(t *testing.T) {
	flags := cfg.DefaultFlags()
	var attempts int32
	flags.ExternalCacheClient = &fakeContentCache{
		storeLocalPath: func(source struct {
			Path      string
			CachePath string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			attempt := atomic.AddInt32(&attempts, 1)
			if attempt < 3 {
				return "", errors.New("transient cache dial error")
			}
			return opts.RoutingKey, nil
		},
	}
	fs := newUnitFS(flags)
	originalDelay := externalCacheStoreRetryDelay
	externalCacheStoreRetryDelay = func(int) time.Duration { return 0 }
	defer func() { externalCacheStoreRetryDelay = originalDelay }()

	fs.processCacheEvent(cacheEvent{path: "file", hash: "h1", size: 1, localSourcePath: "/tmp/file"})

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected store to be retried until success, got %d attempts", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 1 {
		t.Fatalf("expected one successful cache event, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsErrors); got != 0 {
		t.Fatalf("expected no final cache event errors, got %d", got)
	}
}

func TestProcessCacheEventDoesNotRetrySupersededObjectSource(t *testing.T) {
	const path = "volumes/volume/ComfyUI/user/comfyui.db-journal"
	expectedHash := strings.Repeat("a", 64)
	actualHash := strings.Repeat("b", 64)
	var attempts int32
	var published int32

	flags := cfg.DefaultFlags()
	flags.Backend = (&cfg.S3Config{}).Init()
	flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		if event == cfg.EventCacheTriggered {
			atomic.AddInt32(&published, 1)
		}
	}
	flags.ExternalCacheClient = &fakeContentCache{
		storeFromS3: func(source struct {
			Path        string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return actualHash, nil
		},
	}
	fs := newUnitFS(flags)
	fs.cachingStatus[expectedHash] = true
	logOutput := captureMainLog(t)

	fs.processCacheEvent(cacheEvent{path: path, hash: expectedHash, size: 1778176})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("superseded object source attempts = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsMismatch); got != 1 {
		t.Fatalf("cache mismatch count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 0 {
		t.Fatalf("cache success count = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsErrors); got != 0 {
		t.Fatalf("cache error count = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&published); got != 0 {
		t.Fatalf("published cache events = %d, want 0", got)
	}
	fs.cachingStatusMu.Lock()
	stillReserved := fs.cachingStatus[expectedHash]
	fs.cachingStatusMu.Unlock()
	if stillReserved {
		t.Fatal("superseded cache event retained its reservation")
	}
	if got := logOutput.String(); !strings.Contains(got, "status=superseded") || strings.Contains(got, "status=hash_mismatch") {
		t.Fatalf("unexpected superseded object-source log: %q", got)
	}
}

func TestProcessCacheEventDoesNotRetryEmptyObjectSourceHash(t *testing.T) {
	expectedHash := strings.Repeat("f", 64)
	var attempts int32
	flags := cfg.DefaultFlags()
	flags.Backend = (&cfg.S3Config{}).Init()
	flags.ExternalCacheClient = &fakeContentCache{
		storeFromS3: func(source struct {
			Path        string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return "", nil
		},
	}
	fs := newUnitFS(flags)
	logOutput := captureMainLog(t)
	originalDelay := externalCacheStoreRetryDelay
	externalCacheStoreRetryDelay = func(int) time.Duration { return 0 }
	defer func() { externalCacheStoreRetryDelay = originalDelay }()

	fs.processCacheEvent(cacheEvent{
		path: "volumes/volume/ComfyUI/user/comfyui.db-journal",
		hash: expectedHash,
		size: 1778176,
	})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("empty object-source hash attempts = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsMismatch); got != 1 {
		t.Fatalf("cache mismatch count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 0 {
		t.Fatalf("cache success count = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsErrors); got != 0 {
		t.Fatalf("cache error count = %d, want 0", got)
	}
	if got := logOutput.String(); !strings.Contains(got, "status=superseded") || !strings.Contains(got, `actual=""`) {
		t.Fatalf("unexpected empty object-source hash log: %q", got)
	}
}

func TestProcessCacheEventRetriesObjectSourceTransportError(t *testing.T) {
	expectedHash := strings.Repeat("c", 64)
	var attempts int32
	flags := cfg.DefaultFlags()
	flags.Backend = (&cfg.S3Config{}).Init()
	flags.ExternalCacheClient = &fakeContentCache{
		storeFromS3: func(source struct {
			Path        string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			attempt := atomic.AddInt32(&attempts, 1)
			if attempt < 3 {
				return "", errors.New("transient cache transport error")
			}
			return expectedHash, nil
		},
	}
	fs := newUnitFS(flags)
	originalDelay := externalCacheStoreRetryDelay
	externalCacheStoreRetryDelay = func(int) time.Duration { return 0 }
	defer func() { externalCacheStoreRetryDelay = originalDelay }()

	fs.processCacheEvent(cacheEvent{path: "volumes/volume/model.bin", hash: expectedHash, size: 1})

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("object-source transport attempts = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 1 {
		t.Fatalf("cache success count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsErrors); got != 0 {
		t.Fatalf("cache error count = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsMismatch); got != 0 {
		t.Fatalf("cache mismatch count = %d, want 0", got)
	}
}

func TestProcessCacheEventKeepsImmutableLocalMismatchLoud(t *testing.T) {
	expectedHash := strings.Repeat("d", 64)
	actualHash := strings.Repeat("e", 64)
	var attempts int32
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		storeLocalPath: func(source struct {
			Path      string
			CachePath string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return actualHash, nil
		},
	}
	fs := newUnitFS(flags)
	logOutput := captureMainLog(t)
	originalDelay := externalCacheStoreRetryDelay
	externalCacheStoreRetryDelay = func(int) time.Duration { return 0 }
	defer func() { externalCacheStoreRetryDelay = originalDelay }()

	fs.processCacheEvent(cacheEvent{
		path:            "volumes/volume/model.bin",
		hash:            expectedHash,
		size:            1,
		localSourcePath: "/immutable/staged/model.bin",
	})

	if got := atomic.LoadInt32(&attempts); got != externalCacheStoreAttempts {
		t.Fatalf("immutable local mismatch attempts = %d, want %d", got, externalCacheStoreAttempts)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsMismatch); got != 1 {
		t.Fatalf("cache mismatch count = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 0 {
		t.Fatalf("cache success count = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsErrors); got != 0 {
		t.Fatalf("cache error count = %d, want 0", got)
	}
	if got := logOutput.String(); !strings.Contains(got, "status=hash_mismatch") || strings.Contains(got, "status=superseded") {
		t.Fatalf("unexpected immutable local-source log: %q", got)
	}
}

func TestStagedFileCleanupIsIdempotent(t *testing.T) {
	flags := cfg.DefaultFlags()
	fs := newUnitFS(flags)
	inode := &Inode{Name: "file", fs: fs}
	fh := &FileHandle{inode: inode}
	stagedPath := filepath.Join(t.TempDir(), "file")
	fd, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}

	stagedFile := &StagedFile{FH: fh, FD: fd}
	stagedFile.Cleanup()
	stagedFile.Cleanup()

	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("expected staged path to be removed, got err=%v", err)
	}
}

func TestStagedFileCleanupDoesNotRemoveReplacement(t *testing.T) {
	flags := cfg.DefaultFlags()
	fs := newUnitFS(flags)
	inode := &Inode{Name: "file", fs: fs}
	stagedPath := filepath.Join(t.TempDir(), "file")
	fd, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Write([]byte("old generation")); err != nil {
		t.Fatal(err)
	}
	stagedFile := &StagedFile{FH: &FileHandle{inode: inode}, FD: fd}

	replacementPath := stagedPath + ".replacement"
	if err := os.WriteFile(replacementPath, []byte("new generation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, stagedPath); err != nil {
		t.Fatal(err)
	}

	stagedFile.Cleanup()

	got, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("replacement was removed by stale cleanup: %v", err)
	}
	if string(got) != "new generation" {
		t.Fatalf("replacement data = %q, want new generation", got)
	}
}

func TestStagedFilesUseUniquePathsAcrossMounts(t *testing.T) {
	stagedWritePath := t.TempDir()
	newStagedFile := func() *StagedFile {
		flags := cfg.DefaultFlags()
		flags.StagedWritePath = stagedWritePath
		fs := newUnitFS(flags)
		root := newRootWithBackend(fs, &TestBackend{})
		inode := NewInode(fs, root, "main")
		inode.Id = 2
		fh := NewFileHandle(inode)
		if err := fh.getOrCreateStagedFile(); err != nil {
			t.Fatal(err)
		}
		return inode.StagedFile
	}

	first := newStagedFile()
	second := newStagedFile()
	firstPath, firstOK := first.Path()
	secondPath, secondOK := second.Path()
	if !firstOK || !secondOK {
		t.Fatal("staged generation path is unavailable")
	}
	if firstPath == secondPath {
		t.Fatalf("independent mounts shared staged path %q", firstPath)
	}
	first.Cleanup()
	second.Cleanup()
}

func TestNoSuchUploadPreservesStagedData(t *testing.T) {
	flags := cfg.DefaultFlags()
	fs := newUnitFS(flags)
	backend := &TestBackend{}
	backend.MultipartBlobAddFunc = func(param *MultipartBlobAddInput) (*MultipartBlobAddOutput, error) {
		return nil, newNoSuchUploadError()
	}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CREATED)
	inode.Attributes.Size = 5 * 1024 * 1024
	inode.mpu = &MultipartBlobCommitInput{
		Key:      PString("file"),
		UploadId: PString("upload-id"),
		Parts:    make([]*string, 10000),
	}

	stagedPath := filepath.Join(t.TempDir(), "file")
	fd, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fh := &FileHandle{inode: inode}
	inode.StagedFile = &StagedFile{FH: fh, FD: fd}
	data := bytes.Repeat([]byte{1}, int(inode.Attributes.Size))
	inode.buffers.Add(0, data, BUF_DIRTY, false)

	inode.mu.Lock()
	inode.flushPart(0)
	inode.mu.Unlock()

	if inode.mpu != nil {
		t.Fatal("expected MPU state to be cleared after NoSuchUpload")
	}
	if inode.StagedFile == nil || inode.StagedFile.FD == nil {
		t.Fatal("expected staged file to be preserved")
	}
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("expected staged file to remain on disk: %v", err)
	}
	if inode.flushError == nil {
		t.Fatal("expected flush error to be recorded for retry backoff")
	}
}

func TestResetMultipartStateForRetryKeepsFlushedBuffersDirty(t *testing.T) {
	flags := cfg.DefaultFlags()
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.CacheState = ST_MODIFIED
	inode.mpu = &MultipartBlobCommitInput{
		Key:      PString("file"),
		UploadId: PString("upload"),
		Parts:    make([]*string, 10000),
	}

	data := []byte("multipart-data")
	inode.buffers.Add(0, data, BUF_DIRTY, true)
	_, ids, err := inode.buffers.GetData(0, uint64(len(data)), true)
	if err != nil {
		t.Fatal(err)
	}
	inode.buffers.SetState(0, uint64(len(data)), ids, BUF_FLUSHED_FULL)

	inode.resetMultipartStateForRetry()

	if inode.mpu != nil {
		t.Fatal("expected MPU state to be cleared")
	}
	if !inode.isStillDirty() {
		t.Fatal("expected flushed buffers to become dirty again")
	}
	got, ids, err := inode.buffers.GetData(0, uint64(len(data)), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("expected reverted buffers to have dirty ids")
	}
	if !bytes.Equal(bytes.Join(got, nil), data) {
		t.Fatalf("expected data %q, got %q", data, bytes.Join(got, nil))
	}
}

func TestNonStagedFlushPartUsesRetryableBufferReader(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	fs := newUnitFS(flags)
	payload := bytes.Repeat([]byte("retryable-buffer-reader-"), 8192)
	partID := "part-1"
	var sawMultiReader bool

	backend := &TestBackend{}
	backend.MultipartBlobAddFunc = func(param *MultipartBlobAddInput) (*MultipartBlobAddOutput, error) {
		if _, ok := param.Body.(*MultiReader); !ok {
			t.Fatalf("expected non-hashed part body to use MultiReader directly, got %T", param.Body)
		}
		sawMultiReader = true
		first, err := io.ReadAll(param.Body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := param.Body.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		second, err := io.ReadAll(param.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, payload) || !bytes.Equal(second, payload) {
			t.Fatalf("upload body changed across retry seek: first=%d second=%d want=%d", len(first), len(second), len(payload))
		}
		return &MultipartBlobAddOutput{PartId: &partID}, nil
	}

	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CREATED)
	inode.Attributes.Size = uint64(len(payload))
	inode.mpu = &MultipartBlobCommitInput{
		Key:      PString("file"),
		UploadId: PString("upload-id"),
		Parts:    make([]*string, 10000),
	}
	inode.buffers.Add(0, payload, BUF_DIRTY, false)

	inode.mu.Lock()
	inode.flushPart(0)
	inode.mu.Unlock()

	if !sawMultiReader {
		t.Fatal("expected MultipartBlobAdd to be called")
	}
	if inode.mpu == nil || inode.mpu.Parts[0] == nil || *inode.mpu.Parts[0] != partID {
		t.Fatalf("expected uploaded part id to be recorded, got %#v", inode.mpu)
	}
	if inode.flushError != nil {
		t.Fatalf("expected successful part flush, got flush error %v", inode.flushError)
	}
}

func TestNonStagedFlushPartSpoolsHashSourceToTempFile(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.CachePath = t.TempDir()
	flags.HashAttr = "sha256"
	flags.MinFileSizeForHashKB = 0
	fs := newUnitFS(flags)
	payload := bytes.Repeat([]byte("hash-spooled-part-"), 8192)
	expectedHash := sha256.Sum256(payload)
	partID := "part-1"
	var spooledPath string

	backend := &TestBackend{}
	backend.MultipartBlobAddFunc = func(param *MultipartBlobAddInput) (*MultipartBlobAddOutput, error) {
		f, ok := param.Body.(*os.File)
		if !ok {
			t.Fatalf("expected hashed part body to use a temp file, got %T", param.Body)
		}
		spooledPath = f.Name()
		got, err := io.ReadAll(param.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("uploaded payload mismatch: got %d bytes want %d", len(got), len(payload))
		}
		if _, err := param.Body.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		return &MultipartBlobAddOutput{PartId: &partID}, nil
	}

	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CREATED)
	inode.Attributes.Size = uint64(len(payload))
	inode.mpu = &MultipartBlobCommitInput{
		Key:      PString("file"),
		UploadId: PString("upload-id"),
		Parts:    make([]*string, 10000),
	}
	inode.buffers.Add(0, payload, BUF_DIRTY, false)

	inode.mu.Lock()
	inode.flushPart(0)
	gotHash := ""
	if inode.hashInProgress != nil {
		gotHash = hex.EncodeToString(inode.hashInProgress.Sum(nil))
	}
	inode.mu.Unlock()

	if spooledPath == "" {
		t.Fatal("expected a temp file upload source")
	}
	if _, err := os.Stat(spooledPath); !os.IsNotExist(err) {
		t.Fatalf("expected temp upload source to be removed after hashing, stat err=%v", err)
	}
	if gotHash != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("hash mismatch: got %s want %s", gotHash, hex.EncodeToString(expectedHash[:]))
	}
	if inode.mpu == nil || inode.mpu.Parts[0] == nil || *inode.mpu.Parts[0] != partID {
		t.Fatalf("expected uploaded part id to be recorded, got %#v", inode.mpu)
	}
}

func TestFailedStagedFlushDoesNotEmitUploadEvent(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.RetryInterval = time.Millisecond
	var events int32
	flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		if event == cfg.EventStagedFileUploaded {
			atomic.AddInt32(&events, 1)
		}
	}
	fs := newUnitFS(flags)
	backend := &TestBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		return nil, syscall.EIO
	}
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedInodeForFlush(t, fs, root, []byte("hello"))

	err := fs.flushStagedFile(inode)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("expected EIO, got %v", err)
	}
	if got := atomic.LoadInt32(&events); got != 0 {
		t.Fatalf("expected no staged upload events, got %d", got)
	}
	if inode.StagedFile == nil || inode.StagedFile.FD == nil {
		t.Fatal("expected failed flush to keep staged file attached")
	}
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("expected staged file to remain on disk: %v", err)
	}
}

func TestSuccessfulStagedFlushEmitsOneUploadEvent(t *testing.T) {
	flags := cfg.DefaultFlags()
	var events int32
	flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		if event == cfg.EventStagedFileUploaded {
			atomic.AddInt32(&events, 1)
		}
	}
	fs := newUnitFS(flags)
	backend := &TestBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedInodeForFlush(t, fs, root, []byte("hello"))

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatalf("flushStagedFile failed: %v", err)
	}
	if got := atomic.LoadInt32(&events); got != 1 {
		t.Fatalf("expected one staged upload event, got %d", got)
	}
	if inode.StagedFile != nil {
		t.Fatal("expected successful flush to detach staged file")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("expected staged file to be removed, got err=%v", err)
	}
}

func TestSuccessfulStagedFlushPublishesFromItsGenerationPath(t *testing.T) {
	payload := []byte("cached staged content")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	flags.MinFileSizeForHashKB = 0
	flags.CacheThroughModeEnabled = true
	flags.ExternalCacheClient = &fakeContentCache{
		storeLocalPath: func(source struct {
			Path      string
			CachePath string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			got, err := os.ReadFile(source.Path)
			if err != nil {
				return "", err
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("cache source = %q, want %q", got, payload)
			}
			return opts.RoutingKey, nil
		},
	}
	fs := newUnitFS(flags)
	backend := &TestBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		if _, err := io.Copy(io.Discard, param.Body); err != nil {
			return nil, err
		}
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedInodeForFlush(t, fs, root, payload)

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("staged generation was removed before cache publication: %v", err)
	}
	select {
	case event := <-fs.cacheEventChan:
		if event.hash != expectedHash || event.localSourcePath != stagedPath || !event.removeLocalAfter {
			t.Fatalf("unexpected staged cache event: %+v", event)
		}
		fs.processCacheEvent(event)
	default:
		t.Fatal("expected staged cache event")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged generation remained after cache publication: %v", err)
	}
}

func TestExternalCacheStoreEventIncludesContentIdentity(t *testing.T) {
	flags := cfg.DefaultFlags()
	var gotEvent cfg.EventType
	var gotData map[string]interface{}
	flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		gotEvent = event
		gotData = data
	}
	fs := newUnitFS(flags)

	fs.emitExternalCacheStoredEvent(cacheEvent{
		path: "/volumes/ws/files/data.bin",
		hash: "hash",
		size: 32 << 20,
	}, "s3")

	if gotEvent != cfg.EventCacheTriggered {
		t.Fatalf("event = %q, want %q", gotEvent, cfg.EventCacheTriggered)
	}
	if gotData["path"] != "/volumes/ws/files/data.bin" {
		t.Fatalf("path = %#v", gotData["path"])
	}
	if gotData["inode"] != "/volumes/ws/files/data.bin" {
		t.Fatalf("inode = %#v", gotData["inode"])
	}
	if gotData["hash"] != "hash" || gotData["content_hash"] != "hash" {
		t.Fatalf("hash payload = %#v", gotData)
	}
	if gotData["size_bytes"] != uint64(32<<20) || gotData["size"] != uint64(32<<20) {
		t.Fatalf("size payload = %#v", gotData)
	}
	if gotData["source"] != "s3" {
		t.Fatalf("source = %#v", gotData["source"])
	}
}

func newStagedInodeForFlush(t *testing.T, fs *Goofys, root *Inode, data []byte) (*Inode, string) {
	t.Helper()

	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CREATED)
	inode.Attributes.Size = uint64(len(data))
	fh := &FileHandle{inode: inode}
	stagedPath := filepath.Join(t.TempDir(), "file")
	fd, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	inode.StagedFile = &StagedFile{
		FH:          fh,
		FD:          fd,
		lastWriteAt: time.Now().Add(-time.Second),
		lastReadAt:  time.Now().Add(-time.Second),
		debounce:    0,
	}
	fs.stagedFiles.Store(inode.Id, inode)
	return inode, stagedPath
}

type stagedRenameBackend struct {
	TestBackend
	deleteBlob func(*DeleteBlobInput) (*DeleteBlobOutput, error)
}

func (b *stagedRenameBackend) DeleteBlob(param *DeleteBlobInput) (*DeleteBlobOutput, error) {
	if b.deleteBlob != nil {
		return b.deleteBlob(param)
	}
	return b.TestBackend.DeleteBlob(param)
}

type stagedMultipartBackend struct {
	TestBackend

	mu                sync.Mutex
	nextUpload        int
	parts             map[string]map[uint32][]byte
	committed         [][]byte
	mutateFirstPart   func() error
	mutateFirstPartDo sync.Once
}

func (b *stagedMultipartBackend) MultipartBlobBegin(param *MultipartBlobBeginInput) (*MultipartBlobCommitInput, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextUpload++
	uploadID := fmt.Sprintf("upload-%d", b.nextUpload)
	key := param.Key
	if b.parts == nil {
		b.parts = make(map[string]map[uint32][]byte)
	}
	b.parts[uploadID] = make(map[uint32][]byte)
	return &MultipartBlobCommitInput{
		Key:      &key,
		Metadata: param.Metadata,
		UploadId: &uploadID,
		Parts:    make([]*string, 10000),
	}, nil
}

func (b *stagedMultipartBackend) MultipartBlobAdd(param *MultipartBlobAddInput) (*MultipartBlobAddOutput, error) {
	var mutationErr error
	b.mutateFirstPartDo.Do(func() {
		if b.mutateFirstPart != nil {
			mutationErr = b.mutateFirstPart()
		}
	})
	if mutationErr != nil {
		return nil, mutationErr
	}

	data, err := io.ReadAll(param.Body)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != param.Size {
		return nil, fmt.Errorf("multipart part %d length = %d, want %d", param.PartNumber, len(data), param.Size)
	}
	if param.Commit == nil || param.Commit.UploadId == nil {
		return nil, errors.New("missing multipart upload id")
	}

	uploadID := *param.Commit.UploadId
	partID := fmt.Sprintf("%s-part-%d", uploadID, param.PartNumber)
	b.mu.Lock()
	parts := b.parts[uploadID]
	if parts == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("unknown multipart upload %q", uploadID)
	}
	parts[param.PartNumber] = append([]byte(nil), data...)
	b.mu.Unlock()
	return &MultipartBlobAddOutput{PartId: &partID}, nil
}

func (b *stagedMultipartBackend) MultipartBlobCommit(param *MultipartBlobCommitInput) (*MultipartBlobCommitOutput, error) {
	if param == nil || param.UploadId == nil {
		return nil, errors.New("missing multipart upload id")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	parts := b.parts[*param.UploadId]
	var generation []byte
	for partNum := uint32(1); partNum <= param.NumParts; partNum++ {
		part, ok := parts[partNum]
		if !ok {
			return nil, fmt.Errorf("multipart upload %q missing part %d", *param.UploadId, partNum)
		}
		generation = append(generation, part...)
	}
	if param.Size == nil || uint64(len(generation)) != *param.Size {
		return nil, fmt.Errorf("committed generation length = %d, want %v", len(generation), param.Size)
	}
	b.committed = append(b.committed, generation)
	delete(b.parts, *param.UploadId)
	etag := fmt.Sprintf("etag-%d", len(b.committed))
	now := time.Now()
	return &MultipartBlobCommitOutput{ETag: &etag, LastModified: &now}, nil
}

func (b *stagedMultipartBackend) MultipartBlobAbort(param *MultipartBlobCommitInput) (*MultipartBlobAbortOutput, error) {
	if param != nil && param.UploadId != nil {
		b.mu.Lock()
		delete(b.parts, *param.UploadId)
		b.mu.Unlock()
	}
	return &MultipartBlobAbortOutput{}, nil
}

func (b *stagedMultipartBackend) committedGenerations() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([][]byte, len(b.committed))
	for i, generation := range b.committed {
		result[i] = append([]byte(nil), generation...)
	}
	return result
}

func newStagedRenameInode(t *testing.T, fs *Goofys, root *Inode, name string, data []byte) (*Inode, string) {
	t.Helper()

	inode := NewInode(fs, root, name)
	inode.Id = 2
	inode.SetCacheState(ST_CREATED)
	inode.Attributes.Size = uint64(len(data))
	root.insertChild(inode)

	stagedDir := filepath.Join(fs.flags.StagedWritePath, filepath.Dir(name))
	if err := os.MkdirAll(stagedDir, 0755); err != nil {
		t.Fatal(err)
	}
	fd, err := os.CreateTemp(stagedDir, ".geesefs-staged-*")
	if err != nil {
		t.Fatal(err)
	}
	stagedPath := fd.Name()
	if _, err := fd.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	inode.StagedFile = &StagedFile{
		FH:          NewFileHandle(inode),
		FD:          fd,
		lastWriteAt: time.Now().Add(-time.Second),
		lastReadAt:  time.Now().Add(-time.Second),
	}
	fs.stagedFiles.Store(inode.Id, inode)
	return inode, stagedPath
}

func TestStagedRenameBeforeFlushUploadsFinalKey(t *testing.T) {
	const (
		oldName = "model.safetensors.incomplete"
		newName = "model.safetensors"
	)
	payload := []byte("model weights")
	var uploadedKey string
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		uploadedKey = param.Key
		got, err := io.ReadAll(param.Body)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("uploaded data = %q, want %q", got, payload)
		}
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, oldPath := newStagedRenameInode(t, fs, root, oldName, payload)

	if err := root.Rename(oldName, root, newName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("staged generation moved during logical rename: %v", err)
	}

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	if uploadedKey != newName {
		t.Fatalf("uploaded key = %q, want %q", uploadedKey, newName)
	}
	if inode.oldParent != nil {
		t.Fatal("rename before flush should not require a remote rename")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("staged path was not cleaned up: %v", err)
	}
}

func TestStagedRenameDuringFlushCompletesRemoteRename(t *testing.T) {
	const (
		oldName = "model.safetensors.incomplete"
		newName = "model.safetensors"
	)
	payload := []byte("model weights")
	uploadStarted := make(chan string, 1)
	releaseUpload := make(chan struct{})
	uploaded := make(chan []byte, 1)
	copied := make(chan *CopyBlobInput, 1)
	releaseCopy := make(chan struct{})
	deleted := make(chan string, 1)
	var originReads atomic.Int32
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		uploadStarted <- param.Key
		<-releaseUpload
		data, err := io.ReadAll(param.Body)
		if err != nil {
			return nil, err
		}
		uploaded <- data
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}
	backend.CopyBlobFunc = func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copied <- param
		<-releaseCopy
		return &CopyBlobOutput{}, nil
	}
	backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
		originReads.Add(1)
		return nil, syscall.ENOENT
	}
	backend.deleteBlob = func(param *DeleteBlobInput) (*DeleteBlobOutput, error) {
		deleted <- param.Key
		return &DeleteBlobOutput{}, nil
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, oldPath := newStagedRenameInode(t, fs, root, oldName, payload)

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- fs.flushStagedFile(inode)
	}()

	select {
	case key := <-uploadStarted:
		if key != oldName {
			t.Fatalf("upload started at key %q, want %q", key, oldName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for staged upload")
	}

	if err := root.Rename(oldName, root, newName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("staged generation moved during logical rename: %v", err)
	}
	close(releaseUpload)

	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for staged flush")
	}
	if got := <-uploaded; !bytes.Equal(got, payload) {
		t.Fatalf("uploaded data = %q, want %q", got, payload)
	}

	inode.mu.Lock()
	oldParent := inode.oldParent
	flushes := inode.IsFlushing
	stagedFile := inode.StagedFile
	inode.mu.Unlock()
	if oldParent != root {
		t.Fatal("rename during flush did not retain the old object location")
	}
	if flushes != 0 {
		t.Fatalf("staged flush marker = %d, want 0", flushes)
	}
	if stagedFile == nil {
		t.Fatal("staged generation was discarded before the remote rename completed")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("staged generation was removed before the remote rename completed: %v", err)
	}
	if !inode.TryFlush(1) {
		t.Fatal("remote rename was not scheduled")
	}

	select {
	case copyInput := <-copied:
		if copyInput.Source != oldName || copyInput.Destination != newName {
			t.Fatalf("remote copy = %q -> %q, want %q -> %q", copyInput.Source, copyInput.Destination, oldName, newName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for remote copy")
	}

	data, bytesRead, err := NewFileHandle(inode).ReadFile(0, int64(len(payload)))
	if err != nil {
		t.Fatalf("read during remote rename failed: %v", err)
	}
	if bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("read during remote rename = %q (%d bytes), want %q", bytes.Join(data, nil), bytesRead, payload)
	}
	if got := originReads.Load(); got != 0 {
		t.Fatalf("read during remote rename reached origin %d times", got)
	}
	close(releaseCopy)

	select {
	case key := <-deleted:
		if key != oldName {
			t.Fatalf("deleted key = %q, want %q", key, oldName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for old object deletion")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("staged path was not cleaned up: %v", err)
	}
	inode.mu.Lock()
	stagedFile = inode.StagedFile
	inode.mu.Unlock()
	if stagedFile != nil {
		t.Fatal("staged generation remained attached after the remote rename completed")
	}
}

func TestStagedRenameCopyFailureRetainsReadableLocalGeneration(t *testing.T) {
	payload := []byte("model revision")
	uploadStarted := make(chan struct{}, 1)
	releaseUpload := make(chan struct{})
	copyAttempted := make(chan struct{}, 1)
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		uploadStarted <- struct{}{}
		<-releaseUpload
		if _, err := io.Copy(io.Discard, param.Body); err != nil {
			return nil, err
		}
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}
	backend.CopyBlobFunc = func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyAttempted <- struct{}{}
		return nil, errors.New("transient copy failure")
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedRenameInode(t, fs, root, "main.tmp", payload)

	flushDone := make(chan error, 1)
	go func() { flushDone <- fs.flushStagedFile(inode) }()
	select {
	case <-uploadStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for staged upload")
	}
	if err := root.Rename("main.tmp", root, "main"); err != nil {
		t.Fatal(err)
	}
	close(releaseUpload)
	if err := <-flushDone; err != nil {
		t.Fatal(err)
	}
	if !inode.TryFlush(1) {
		t.Fatal("remote rename was not scheduled")
	}
	select {
	case <-copyAttempted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for remote copy attempt")
	}

	deadline := time.Now().Add(time.Second)
	for {
		inode.mu.Lock()
		flushing := inode.IsFlushing
		stagedFile := inode.StagedFile
		oldParent := inode.oldParent
		inode.mu.Unlock()
		if flushing == 0 {
			if stagedFile == nil || oldParent == nil {
				t.Fatal("copy failure discarded the local staged generation")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for failed remote rename")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("copy failure removed the local staged generation: %v", err)
	}
	data, bytesRead, err := NewFileHandle(inode).ReadFile(0, int64(len(payload)))
	if err != nil {
		t.Fatalf("read after copy failure failed: %v", err)
	}
	if bytesRead != len(payload) || !bytes.Equal(bytes.Join(data, nil), payload) {
		t.Fatalf("read after copy failure = %q (%d bytes), want %q", bytes.Join(data, nil), bytesRead, payload)
	}
}

func TestStagedRenameMissingSourceUploadsRetainedGenerationDirectly(t *testing.T) {
	payload := []byte("model revision")
	firstUploadStarted := make(chan struct{}, 1)
	releaseFirstUpload := make(chan struct{})
	copyAttempted := make(chan struct{}, 1)
	var mu sync.Mutex
	var uploadedKeys []string
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		mu.Lock()
		call := len(uploadedKeys)
		uploadedKeys = append(uploadedKeys, param.Key)
		mu.Unlock()
		if call == 0 {
			firstUploadStarted <- struct{}{}
			<-releaseFirstUpload
		}
		got, err := io.ReadAll(param.Body)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("uploaded data = %q, want %q", got, payload)
		}
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}
	backend.CopyBlobFunc = func(param *CopyBlobInput) (*CopyBlobOutput, error) {
		copyAttempted <- struct{}{}
		return nil, syscall.ENOENT
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedRenameInode(t, fs, root, "main.tmp", payload)

	flushDone := make(chan error, 1)
	go func() { flushDone <- fs.flushStagedFile(inode) }()
	select {
	case <-firstUploadStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for staged upload")
	}
	if err := root.Rename("main.tmp", root, "main"); err != nil {
		t.Fatal(err)
	}
	close(releaseFirstUpload)
	if err := <-flushDone; err != nil {
		t.Fatal(err)
	}
	if !inode.TryFlush(1) {
		t.Fatal("remote rename was not scheduled")
	}
	select {
	case <-copyAttempted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for remote copy attempt")
	}
	waitForInodeFlushes(t, inode, 0)

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	keys := append([]string(nil), uploadedKeys...)
	mu.Unlock()
	if len(keys) != 2 || keys[0] != "main.tmp" || keys[1] != "main" {
		t.Fatalf("uploaded keys = %v, want [main.tmp main]", keys)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged path was not cleaned after direct fallback upload: %v", err)
	}
}

func TestStagedWriteDuringFlushRetainsGenerationForRetry(t *testing.T) {
	initial := []byte("old generation")
	updated := []byte("new generation")
	firstUploadStarted := make(chan struct{}, 1)
	releaseFirstUpload := make(chan struct{})
	var mu sync.Mutex
	var uploads [][]byte
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		mu.Lock()
		call := len(uploads)
		mu.Unlock()
		if call == 0 {
			firstUploadStarted <- struct{}{}
			<-releaseFirstUpload
		}
		data, err := io.ReadAll(param.Body)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		uploads = append(uploads, data)
		mu.Unlock()
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedRenameInode(t, fs, root, "file", initial)

	flushDone := make(chan error, 1)
	go func() { flushDone <- fs.flushStagedFile(inode) }()
	select {
	case <-firstUploadStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for staged upload")
	}
	if err := inode.StagedFile.FH.WriteFileStaged(0, updated); err != nil {
		t.Fatal(err)
	}
	close(releaseFirstUpload)
	if err := <-flushDone; err != nil {
		t.Fatal(err)
	}

	inode.mu.Lock()
	stagedFile := inode.StagedFile
	inode.mu.Unlock()
	if stagedFile == nil {
		t.Fatal("write during flush was discarded with the completed upload")
	}
	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotUploads := append([][]byte(nil), uploads...)
	mu.Unlock()
	if len(gotUploads) != 2 {
		t.Fatalf("uploads = %d, want 2", len(gotUploads))
	}
	if !bytes.Equal(gotUploads[1], updated) {
		t.Fatalf("retried upload = %q, want %q", gotUploads[1], updated)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged path was not cleaned after stable upload: %v", err)
	}
}

func TestDirectStagedPutUsesImmutableGeneration(t *testing.T) {
	initial := bytes.Repeat([]byte("a"), 512)
	replacement := []byte("new journal\n")
	var inode *Inode
	var mu sync.Mutex
	var uploads [][]byte
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		mu.Lock()
		call := len(uploads)
		mu.Unlock()
		if call == 0 {
			if err := inode.SetAttributes(PUInt64(0), nil, nil, nil, nil); err != nil {
				return nil, err
			}
			if err := inode.StagedFile.FH.WriteFileStaged(0, replacement); err != nil {
				return nil, err
			}
		}

		got, err := io.ReadAll(param.Body)
		if err != nil {
			return nil, err
		}
		if param.Size == nil {
			return nil, errors.New("missing ContentLength")
		}
		if uint64(len(got)) != *param.Size {
			return nil, fmt.Errorf("ContentLength=%d with Body length %d", *param.Size, len(got))
		}
		mu.Lock()
		uploads = append(uploads, got)
		mu.Unlock()
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, _ = newStagedRenameInode(t, fs, root, "comfyui.db-journal", initial)

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	inode.mu.Lock()
	stagedFile := inode.StagedFile
	inode.mu.Unlock()
	if stagedFile == nil {
		t.Fatal("replacement generation was discarded after the first upload")
	}
	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	gotUploads := append([][]byte(nil), uploads...)
	mu.Unlock()
	if len(gotUploads) != 2 {
		t.Fatalf("uploads = %d, want 2", len(gotUploads))
	}
	if !bytes.Equal(gotUploads[0], initial) {
		t.Fatalf("first upload length = %d, want immutable %d-byte generation", len(gotUploads[0]), len(initial))
	}
	if !bytes.Equal(gotUploads[1], replacement) {
		t.Fatalf("second upload = %q, want %q", gotUploads[1], replacement)
	}
}

func TestDirectStagedMultipartUsesImmutableGenerationAcrossTruncate(t *testing.T) {
	const partSize = uint64(1024 * 1024)
	initial := make([]byte, stagedUploadMemorySnapshotLimit+int(partSize)+137)
	for offset := 0; offset < len(initial); offset += int(partSize) {
		end := offset + int(partSize)
		if end > len(initial) {
			end = len(initial)
		}
		for i := offset; i < end; i++ {
			initial[i] = byte(offset/int(partSize) + 1)
		}
	}
	replacement := bytes.Repeat([]byte("next-generation-"), 70000)

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	flags.SinglePartMB = 1
	flags.MaxParallelParts = 1
	flags.PartSizes = []cfg.PartSizeConfig{{PartSize: partSize, PartCount: 10000}}
	fs := newUnitFS(flags)
	backend := &stagedMultipartBackend{}
	root := newRootWithBackend(fs, backend)
	inode, stagedPath := newStagedRenameInode(t, fs, root, "model.bin", initial)

	var snapshotPath string
	backend.mutateFirstPart = func() error {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(stagedPath), ".geesefs-upload-*"))
		if err != nil {
			return err
		}
		if len(matches) != 1 {
			return fmt.Errorf("file-backed upload snapshots = %v, want exactly one", matches)
		}
		snapshotPath = matches[0]

		if err := inode.SetAttributes(PUInt64(0), nil, nil, nil, nil); err != nil {
			return err
		}
		return inode.StagedFile.FH.WriteFileStaged(0, replacement)
	}

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	if snapshotPath == "" {
		t.Fatal("multipart upload did not use a file-backed immutable snapshot")
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("multipart snapshot was not cleaned after upload: %v", err)
	}
	committed := backend.committedGenerations()
	if len(committed) != 1 {
		t.Fatalf("first multipart commits = %d, want 1", len(committed))
	}
	if !bytes.Equal(committed[0], initial) {
		t.Fatalf("first multipart commit mixed generations: size=%d want=%d", len(committed[0]), len(initial))
	}

	inode.mu.Lock()
	stagedFile := inode.StagedFile
	cacheState := inode.CacheState
	inode.mu.Unlock()
	if stagedFile == nil {
		t.Fatal("next staged generation was discarded after multipart commit")
	}
	stagedFile.mu.Lock()
	nextDirty := stagedFile.shouldFlush && !stagedFile.flushing && !stagedFile.awaitingRemoteRename
	stagedFile.mu.Unlock()
	if !nextDirty || cacheState != ST_MODIFIED {
		t.Fatalf("next staged generation is not dirty: should_flush=%v cache_state=%d", nextDirty, cacheState)
	}

	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
	committed = backend.committedGenerations()
	if len(committed) != 2 {
		t.Fatalf("multipart commits = %d, want 2", len(committed))
	}
	if !bytes.Equal(committed[1], replacement) {
		t.Fatalf("second multipart commit size = %d, want replacement size %d", len(committed[1]), len(replacement))
	}
	inode.mu.Lock()
	stagedFile = inode.StagedFile
	inode.mu.Unlock()
	if stagedFile != nil {
		t.Fatal("stable replacement generation remained attached after upload")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("live staged path was not cleaned after replacement upload: %v", err)
	}
}

func TestStagedPartialOverwritePreservesLogicalSize(t *testing.T) {
	origin := bytes.Repeat([]byte("o"), 512)
	replacement := []byte("new journal\n")
	backend := &TestBackend{}
	backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
		end := param.Start + param.Count
		if end > uint64(len(origin)) {
			return nil, fmt.Errorf("origin read %d:%d exceeds %d", param.Start, end, len(origin))
		}
		return &GetBlobOutput{
			Body: io.NopCloser(bytes.NewReader(origin[param.Start:end])),
		}, nil
	}
	flags := cfg.DefaultFlags()
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "comfyui.db-journal")
	inode.Id = 2
	inode.SetCacheState(ST_CACHED)
	inode.Attributes.Size = 512
	inode.knownSize = 512
	fh := NewFileHandle(inode)

	staged, err := fh.prepareStagedWrite(0, len(replacement))
	if err != nil {
		t.Fatal(err)
	}
	if staged {
		t.Fatal("partial overwrite of an existing object selected direct staging")
	}
	if err := fh.WriteFile(0, replacement, true); err != nil {
		t.Fatal(err)
	}
	if got := inode.Attributes.Size; got != 512 {
		t.Fatalf("staged inode size = %d after partial overwrite, want 512", got)
	}
	if inode.StagedFile != nil {
		t.Fatal("partial overwrite created a staged file")
	}
	if !inode.buffers.AnyUnclean() {
		t.Fatal("partial overwrite did not use dirty buffers")
	}
	data, bytesRead, err := fh.ReadFile(0, int64(len(origin)))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), origin...)
	copy(want, replacement)
	if got := bytes.Join(data, nil); bytesRead != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("partial overwrite read = %q (%d bytes), want preserved %d-byte object", got, bytesRead, len(want))
	}
	staged, err = fh.prepareStagedWrite(0, 512)
	if err != nil {
		t.Fatal(err)
	}
	if staged {
		t.Fatal("subsequent write switched a dirty buffered generation to staging")
	}
}

func TestFullExistingObjectReplacementUsesStaging(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, &TestBackend{})
	inode := NewInode(fs, root, "model.bin")
	inode.Id = 2
	inode.SetCacheState(ST_CACHED)
	inode.Attributes.Size = 512
	inode.knownSize = 512
	fh := NewFileHandle(inode)

	staged, err := fh.prepareStagedWrite(0, 512)
	if err != nil {
		t.Fatal(err)
	}
	if !staged || inode.StagedFile == nil {
		t.Fatal("full existing-object replacement did not select direct staging")
	}
	inode.mu.Lock()
	stagedFile := inode.StagedFile
	inode.StagedFile = nil
	inode.mu.Unlock()
	stagedFile.Cleanup()
}

func TestDirectStagedPutEmptyGeneration(t *testing.T) {
	backend := &stagedRenameBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		if param.Size == nil || *param.Size != 0 {
			return nil, fmt.Errorf("ContentLength=%v, want 0", param.Size)
		}
		got, err := io.ReadAll(param.Body)
		if err != nil {
			return nil, err
		}
		if len(got) != 0 {
			return nil, fmt.Errorf("empty staged body length = %d", len(got))
		}
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}

	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, backend)
	inode, _ := newStagedInodeForFlush(t, fs, root, nil)
	if err := fs.flushStagedFile(inode); err != nil {
		t.Fatal(err)
	}
}

func TestLargeStagedPutSnapshotIsFileBackedAndImmutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staged")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	size := uint64(stagedUploadMemorySnapshotLimit + 1)
	if err := file.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("start"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("end"), int64(size)-3); err != nil {
		t.Fatal(err)
	}

	snapshot, cleanup, err := snapshotStagedUpload(file, size)
	if err != nil {
		t.Fatal(err)
	}
	snapshotFile, ok := snapshot.(*os.File)
	if !ok {
		cleanup()
		t.Fatalf("large snapshot type = %T, want *os.File", snapshot)
	}
	snapshotPath := snapshotFile.Name()
	if err := file.Truncate(12); err != nil {
		cleanup()
		t.Fatal(err)
	}
	start := make([]byte, 5)
	if _, err := snapshot.ReadAt(start, 0); err != nil {
		cleanup()
		t.Fatal(err)
	}
	end := make([]byte, 3)
	if _, err := snapshot.ReadAt(end, int64(size)-3); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if string(start) != "start" || string(end) != "end" {
		cleanup()
		t.Fatalf("snapshot changed after source truncate: start=%q end=%q", start, end)
	}
	cleanup()
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("file-backed snapshot was not removed: %v", err)
	}
}

func TestOpenFileHonorsExplicitTruncate(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = t.TempDir()
	fs := newUnitFS(flags)
	fs.inodes = make(map[fuseops.InodeID]*Inode)
	fs.fileHandles = make(map[fuseops.HandleID]*FileHandle)
	fs.nextHandleID = 1
	root := newRootWithBackend(fs, &TestBackend{})
	fs.inodes[root.Id] = root

	newExistingFile := func(id fuseops.InodeID, name string) *Inode {
		inode := NewInode(fs, root, name)
		inode.Id = id
		inode.SetCacheState(ST_CACHED)
		inode.Attributes.Size = 512
		inode.knownSize = 512
		root.insertChild(inode)
		fs.inodes[id] = inode
		return inode
	}
	truncated := newExistingFile(2, "truncate.db-journal")
	untouched := newExistingFile(3, "ordinary.db-journal")
	fuseFS := NewGoofysFuse(fs)

	truncateOp := &fuseops.OpenFileOp{
		Inode:     truncated.Id,
		OpenFlags: syscall.O_WRONLY | syscall.O_TRUNC,
	}
	if err := fuseFS.OpenFile(context.Background(), truncateOp); err != nil {
		t.Fatal(err)
	}
	if got := truncated.Attributes.Size; got != 0 {
		t.Fatalf("O_TRUNC inode size = %d, want 0", got)
	}
	if truncated.CacheState != ST_MODIFIED {
		t.Fatalf("O_TRUNC cache state = %d, want modified", truncated.CacheState)
	}
	truncateWrite := &fuseops.WriteFileOp{
		Handle: truncateOp.Handle,
		Offset: 0,
		Data:   []byte("new journal\n"),
	}
	if err := fuseFS.WriteFile(context.Background(), truncateWrite); err != nil {
		t.Fatal(err)
	}
	if truncated.StagedFile == nil {
		t.Fatal("write after O_TRUNC did not select direct staging")
	}
	if got := truncated.Attributes.Size; got != uint64(len(truncateWrite.Data)) {
		t.Fatalf("write after O_TRUNC size = %d, want %d", got, len(truncateWrite.Data))
	}

	ordinaryOp := &fuseops.OpenFileOp{
		Inode:     untouched.Id,
		OpenFlags: syscall.O_WRONLY,
	}
	if err := fuseFS.OpenFile(context.Background(), ordinaryOp); err != nil {
		t.Fatal(err)
	}
	if got := untouched.Attributes.Size; got != 512 {
		t.Fatalf("ordinary open inode size = %d, want 512", got)
	}
}

func waitForInodeFlushes(t *testing.T, inode *Inode, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		inode.mu.Lock()
		flushing := inode.IsFlushing
		inode.mu.Unlock()
		if flushing == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("inode flush count = %d, want %d", flushing, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExternalCacheShortStreamFallsBackWithoutZeroPadding(t *testing.T) {
	want := []byte("hello world")
	flags := cfg.DefaultFlags()
	flags.ExternalCacheStreamingEnabled = true
	flags.ExternalCacheClient = &fakeContentCache{
		getContentStream: func(hash string, offset int64, length int64, opts struct {
			RoutingKey string
		}) (chan []byte, error) {
			ch := make(chan []byte, 1)
			ch <- []byte("short")
			close(ch)
			return ch, nil
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.knownETag = "etag-v1"
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.readCond = sync.NewCond(&inode.mu)
	backend := &TestBackend{}
	var getBlobCalls int32
	backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
		atomic.AddInt32(&getBlobCalls, 1)
		if param.IfMatch == nil || *param.IfMatch != "etag-v1" {
			t.Fatalf("fallback origin read If-Match = %v, want etag-v1", param.IfMatch)
		}
		return &GetBlobOutput{
			Body: io.NopCloser(bytes.NewReader(want[param.Start : param.Start+param.Count])),
			HeadBlobOutput: HeadBlobOutput{
				BlobItemOutput: BlobItemOutput{Metadata: map[string]*string{}},
			},
		}, nil
	}

	inode.retryRead(backend, "file", 0, uint64(len(want)), false)

	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one S3 fallback read, got %d", got)
	}
	data, _, err := inode.buffers.GetData(0, uint64(len(want)), false)
	if err != nil {
		t.Fatal(err)
	}
	got := bytes.Join(data, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("fallback data mismatch: got %q want %q", got, want)
	}
	if bytes.Contains(got, []byte{0}) {
		t.Fatalf("fallback data contains zero padding: %q", got)
	}
}

func TestLoadFromServerPinsOneETagAcrossParallelRanges(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 2*1024)
	flags := cfg.DefaultFlags()
	flags.ReadAheadParallelKB = 1
	flags.ReadMergeKB = 0
	fs := newUnitFS(flags)
	ifMatches := make(chan string, 2)
	backend := &TestBackend{GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
		if param.IfMatch == nil {
			ifMatches <- ""
		} else {
			ifMatches <- *param.IfMatch
		}
		end := param.Start + param.Count
		etag := "etag-v1"
		return &GetBlobOutput{
			Body: io.NopCloser(bytes.NewReader(payload[param.Start:end])),
			HeadBlobOutput: HeadBlobOutput{BlobItemOutput: BlobItemOutput{
				Key: &param.Key, ETag: &etag, Size: param.Count, Metadata: map[string]*string{},
			}},
		}, nil
	}}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "model.bin")
	inode.Attributes.Size = uint64(len(payload))
	inode.knownSize = uint64(len(payload))
	inode.knownETag = "etag-v1"
	inode.SetCacheState(ST_CACHED)

	inode.mu.Lock()
	inode.loadFromServer([]Range{{Start: 0, End: uint64(len(payload))}}, 0, false)
	// The range goroutines cannot acquire inode.mu until this unlock. Changing
	// the inode identity here proves they use the launch-time snapshot.
	inode.knownETag = "etag-v2"
	inode.mu.Unlock()

	for i := 0; i < 2; i++ {
		select {
		case got := <-ifMatches:
			if got != "etag-v1" {
				t.Fatalf("parallel range %d If-Match = %q, want etag-v1", i, got)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for parallel origin range")
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		inode.mu.Lock()
		loading := len(inode.readRanges)
		inode.mu.Unlock()
		if loading == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parallel origin ranges did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestParallelReadPreservesConditionalConflictFromEarlierRange(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 2*1024)
	flags := cfg.DefaultFlags()
	flags.ReadAheadParallelKB = 1
	flags.ReadMergeKB = 0
	fs := newUnitFS(flags)
	var inode *Inode
	var successObservedConflict atomic.Bool
	backend := &TestBackend{GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
		if param.Start != 0 {
			return nil, syscall.EBUSY
		}
		deadline := time.Now().Add(time.Second)
		for {
			inode.mu.Lock()
			conflictRecorded := errors.Is(mapAwsError(inode.readError), syscall.EBUSY)
			inode.mu.Unlock()
			if conflictRecorded {
				successObservedConflict.Store(true)
				break
			}
			if time.Now().After(deadline) {
				return nil, errors.New("timed out waiting for parallel conflict")
			}
			time.Sleep(time.Millisecond)
		}
		etag := "etag-v1"
		return &GetBlobOutput{
			Body: io.NopCloser(bytes.NewReader(payload[:param.Count])),
			HeadBlobOutput: HeadBlobOutput{BlobItemOutput: BlobItemOutput{
				Key: &param.Key, ETag: &etag, Size: param.Count, Metadata: map[string]*string{},
			}},
		}, nil
	}}
	root := newRootWithBackend(fs, backend)
	inode = NewInode(fs, root, "model.bin")
	inode.Attributes.Size = uint64(len(payload))
	inode.knownSize = uint64(len(payload))
	inode.knownETag = "etag-v1"
	inode.SetCacheState(ST_CACHED)

	inode.mu.Lock()
	inode.loadFromServer([]Range{{Start: 0, End: uint64(len(payload))}}, 0, false)
	inode.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for {
		inode.mu.Lock()
		loading := len(inode.readRanges)
		readErr := inode.readError
		inode.mu.Unlock()
		if loading == 0 && successObservedConflict.Load() {
			if !errors.Is(mapAwsError(readErr), syscall.EBUSY) {
				t.Fatalf("last successful range overwrote conditional conflict: %v", readErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parallel origin ranges did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExternalCacheReadIntoTimeoutFallsBackToCloud(t *testing.T) {
	want := []byte("hello world")
	flags := cfg.DefaultFlags()
	flags.ExternalCacheReadTimeout = 10 * time.Millisecond
	releaseCache := make(chan struct{})
	cacheStarted := make(chan struct{})
	var closeStarted sync.Once
	flags.ExternalCacheClient = &fakeContentCache{
		readContentInto: func(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error) {
			closeStarted.Do(func() { close(cacheStarted) })
			<-releaseCache
			return 0, errContentNotFound
		},
	}
	defer close(releaseCache)

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.readCond = sync.NewCond(&inode.mu)
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

	started := time.Now()
	inode.retryRead(backend, "file", 0, uint64(len(want)), false)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cache timeout fallback took too long: %s", elapsed)
	}
	select {
	case <-cacheStarted:
	default:
		t.Fatal("expected cache read-into call")
	}
	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one S3 fallback read, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalCacheTimeouts); got != 1 {
		t.Fatalf("expected one external cache timeout, got %d", got)
	}
	data, _, err := inode.buffers.GetData(0, uint64(len(want)), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("fallback data mismatch: got %q want %q", got, want)
	}
}

func TestExternalCacheUnaryTimeoutFallsBackToCloud(t *testing.T) {
	want := []byte("hello world")
	flags := cfg.DefaultFlags()
	flags.ExternalCacheReadTimeout = 10 * time.Millisecond
	releaseCache := make(chan struct{})
	cacheStarted := make(chan struct{})
	var closeStarted sync.Once
	flags.ExternalCacheClient = &fakeContentCache{
		getContent: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
			closeStarted.Do(func() { close(cacheStarted) })
			<-releaseCache
			return nil, errContentNotFound
		},
	}
	defer close(releaseCache)

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.readCond = sync.NewCond(&inode.mu)
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

	started := time.Now()
	inode.retryRead(backend, "file", 0, uint64(len(want)), false)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cache timeout fallback took too long: %s", elapsed)
	}
	select {
	case <-cacheStarted:
	default:
		t.Fatal("expected cache unary call")
	}
	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one S3 fallback read, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalCacheTimeouts); got != 1 {
		t.Fatalf("expected one external cache timeout, got %d", got)
	}
	data, _, err := inode.buffers.GetData(0, uint64(len(want)), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("fallback data mismatch: got %q want %q", got, want)
	}
}

func TestExternalCacheStreamStallFallsBackToCloud(t *testing.T) {
	want := []byte("hello world")
	flags := cfg.DefaultFlags()
	flags.ExternalCacheStreamingEnabled = true
	flags.ExternalCacheReadTimeout = 10 * time.Millisecond
	streamCh := make(chan []byte)
	flags.ExternalCacheClient = &fakeContentCache{
		getContentStream: func(hash string, offset int64, length int64, opts struct {
			RoutingKey string
		}) (chan []byte, error) {
			return streamCh, nil
		},
	}
	defer close(streamCh)

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.readCond = sync.NewCond(&inode.mu)
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

	started := time.Now()
	inode.retryRead(backend, "file", 0, uint64(len(want)), false)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cache stream fallback took too long: %s", elapsed)
	}
	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one S3 fallback read, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.externalCacheTimeouts); got != 1 {
		t.Fatalf("expected one external cache timeout, got %d", got)
	}
	data, _, err := inode.buffers.GetData(0, uint64(len(want)), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("fallback data mismatch: got %q want %q", got, want)
	}
}

func TestExternalCacheStreamProgressDoesNotTimeout(t *testing.T) {
	want := []byte("hello world!")
	flags := cfg.DefaultFlags()
	flags.ExternalCacheStreamingEnabled = true
	flags.ExternalCacheReadTimeout = 100 * time.Millisecond
	streamCh := make(chan []byte)
	flags.ExternalCacheClient = &fakeContentCache{
		getContentStream: func(hash string, offset int64, length int64, opts struct {
			RoutingKey string
		}) (chan []byte, error) {
			return streamCh, nil
		},
	}

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.readCond = sync.NewCond(&inode.mu)
	backend := &TestBackend{}
	backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
		t.Fatalf("stream with steady progress should not fall back to S3: %+v", param)
		return nil, nil
	}

	go func() {
		streamCh <- []byte("hello ")
		time.Sleep(60 * time.Millisecond)
		streamCh <- []byte("world")
		time.Sleep(60 * time.Millisecond)
		streamCh <- []byte("!")
		close(streamCh)
	}()

	started := time.Now()
	inode.retryRead(backend, "file", 0, uint64(len(want)), false)
	if elapsed := time.Since(started); elapsed <= flags.ExternalCacheReadTimeout {
		t.Fatalf("test did not exercise a stream longer than timeout: elapsed=%s timeout=%s", elapsed, flags.ExternalCacheReadTimeout)
	}
	if got := atomic.LoadInt64(&fs.stats.externalCacheTimeouts); got != 0 {
		t.Fatalf("expected no external cache timeouts, got %d", got)
	}
	data, _, err := inode.buffers.GetData(0, uint64(len(want)), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("stream data mismatch: got %q want %q", got, want)
	}
}

func TestStagedReadBroadcastsLoadedBuffersToWaiters(t *testing.T) {
	want := bytes.Repeat([]byte{7}, 2*1024*1024)
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.ReadAheadKB = uint64(len(want) / 1024)
	flags.ReadAheadLargeKB = flags.ReadAheadKB

	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, &TestBackend{})
	inode := NewInode(fs, root, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.knownSize = uint64(len(want))

	stagedPath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(stagedPath, want, 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := os.OpenFile(stagedPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()

	inode.StagedFile = &StagedFile{
		FH:          NewFileHandle(inode),
		FD:          fd,
		lastWriteAt: time.Now(),
		lastReadAt:  time.Now(),
		debounce:    time.Hour,
	}

	markedLoading := make(chan struct{})
	releaseLoad := make(chan struct{})
	var hookOnce sync.Once
	oldHook := stagedReadAfterAddLoadingHook
	stagedReadAfterAddLoadingHook = func() {
		hookOnce.Do(func() {
			close(markedLoading)
			<-releaseLoad
		})
	}
	defer func() {
		stagedReadAfterAddLoadingHook = oldHook
	}()

	firstDone := make(chan error, 1)
	go func() {
		data, bytesRead, err := NewFileHandle(inode).ReadFile(0, int64(len(want)/2))
		if err == nil && bytesRead != len(want)/2 {
			err = errors.New("short first read")
		}
		if err == nil && !bytes.Equal(bytes.Join(data, nil), want[:len(want)/2]) {
			err = errors.New("first read data mismatch")
		}
		firstDone <- err
	}()

	select {
	case <-markedLoading:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for staged read to mark loading buffers")
	}

	secondDone := make(chan error, 1)
	go func() {
		data, bytesRead, err := NewFileHandle(inode).ReadFile(int64(len(want)/2), int64(len(want)/2))
		if err == nil && bytesRead != len(want)/2 {
			err = errors.New("short second read")
		}
		if err == nil && !bytes.Equal(bytes.Join(data, nil), want[len(want)/2:]) {
			err = errors.New("second read data mismatch")
		}
		secondDone <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		inode.mu.Lock()
		waiting := inode.readCond != nil
		inode.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for second staged read to block on loading buffers")
		}
		time.Sleep(time.Millisecond)
	}

	close(releaseLoad)
	for name, ch := range map[string]chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s read failed: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s read did not complete after staged load broadcast", name)
		}
	}
}

func TestStagedReadSplitsLargeReadaheadLoads(t *testing.T) {
	const maxChunk = 1024 * 1024
	want := bytes.Repeat([]byte{3}, 8*maxChunk)
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.ReadAheadKB = uint64(len(want) / 1024)
	flags.ReadAheadParallelKB = maxChunk / 1024

	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, &TestBackend{})
	inode := NewInode(fs, root, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.knownSize = uint64(len(want))

	stagedPath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(stagedPath, want, 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := os.OpenFile(stagedPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()

	inode.StagedFile = &StagedFile{
		FH:       NewFileHandle(inode),
		FD:       fd,
		debounce: time.Hour,
	}

	data, bytesRead, err := NewFileHandle(inode).ReadFile(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != 1 || !bytes.Equal(bytes.Join(data, nil), want[:1]) {
		t.Fatalf("unexpected read result: bytes=%d data=%q", bytesRead, bytes.Join(data, nil))
	}

	var loadedBuffers int
	inode.buffers.at.Ascend(0, func(_ uint64, b *FileBuffer) bool {
		if b.data != nil {
			loadedBuffers++
			if b.length > maxChunk {
				t.Fatalf("staged read buffer length = %d, want <= %d", b.length, maxChunk)
			}
		}
		return true
	})
	if loadedBuffers != len(want)/maxChunk {
		t.Fatalf("loaded buffers = %d, want %d", loadedBuffers, len(want)/maxChunk)
	}
}

func TestStagedReadErrorClearsLoadingBuffers(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true

	fs := newUnitFS(flags)
	root := newRootWithBackend(fs, &TestBackend{})
	inode := NewInode(fs, root, "file")
	inode.Attributes.Size = 4
	inode.knownSize = 4
	inode.StagedFile = &StagedFile{
		FH:       NewFileHandle(inode),
		debounce: time.Hour,
	}

	_, _, err := NewFileHandle(inode).ReadFile(0, 4)
	if !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("expected staged read EAGAIN, got %v", err)
	}
	_, loading, _ := inode.buffers.GetHoles(0, 4)
	if loading {
		t.Fatal("staged read error left loading buffers behind")
	}
}

func TestStagedReadFallsBackToOriginWhenStagedFileDetachedDuringLoad(t *testing.T) {
	want := []byte("origin data")
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true

	fs := newUnitFS(flags)
	backend := &TestBackend{}
	var getBlobCalls int32
	backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
		atomic.AddInt32(&getBlobCalls, 1)
		start := int(param.Start)
		end := int(param.Start + param.Count)
		return &GetBlobOutput{
			Body: io.NopCloser(bytes.NewReader(want[start:end])),
			HeadBlobOutput: HeadBlobOutput{
				BlobItemOutput: BlobItemOutput{Metadata: map[string]*string{}},
			},
		}, nil
	}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Attributes.Size = uint64(len(want))
	inode.knownSize = uint64(len(want))

	stagedPath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(stagedPath, bytes.Repeat([]byte("x"), len(want)), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := os.OpenFile(stagedPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	stagedFile := &StagedFile{
		FH:       NewFileHandle(inode),
		FD:       fd,
		debounce: time.Hour,
	}
	inode.StagedFile = stagedFile

	var hookOnce sync.Once
	oldHook := stagedReadAfterAddLoadingHook
	stagedReadAfterAddLoadingHook = func() {
		hookOnce.Do(func() {
			stagedFile.mu.Lock()
			_ = stagedFile.FD.Close()
			stagedFile.FD = nil
			stagedFile.mu.Unlock()

			inode.mu.Lock()
			inode.StagedFile = nil
			inode.SetCacheState(ST_CACHED)
			inode.mu.Unlock()
		})
	}
	defer func() {
		stagedReadAfterAddLoadingHook = oldHook
	}()

	data, bytesRead, err := NewFileHandle(inode).ReadFile(0, int64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != len(want) {
		t.Fatalf("expected %d bytes read, got %d", len(want), bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("origin fallback data mismatch: got %q want %q", got, want)
	}
	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one origin read, got %d", got)
	}
	_, loading, _ := inode.buffers.GetHoles(0, uint64(len(want)))
	if loading {
		t.Fatal("origin fallback left loading buffers behind")
	}
}

func TestReadFileRetriesStalledLoadingBuffer(t *testing.T) {
	want := []byte("hello world")
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.HTTPTimeout = 10 * time.Millisecond

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
	inode.readCond = sync.NewCond(&inode.mu)
	inode.buffers.AddLoading(0, uint64(len(want)))
	fh := NewFileHandle(inode)

	started := time.Now()
	data, bytesRead, err := fh.ReadFile(0, int64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stalled loading retry took too long: %s", elapsed)
	}
	if bytesRead != len(want) {
		t.Fatalf("expected %d bytes read, got %d", len(want), bytesRead)
	}
	if got := atomic.LoadInt32(&getBlobCalls); got != 1 {
		t.Fatalf("expected one origin read after stale loading timeout, got %d", got)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, want) {
		t.Fatalf("origin data mismatch: got %q want %q", got, want)
	}
}

func TestCacheStatusClearedByHash(t *testing.T) {
	for _, tt := range []struct {
		name     string
		storeErr error
	}{
		{name: "success"},
		{name: "failure", storeErr: errors.New("cache failure")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			flags := cfg.DefaultFlags()
			flags.Backend = (&cfg.S3Config{}).Init()
			flags.ExternalCacheClient = &fakeContentCache{
				storeFromS3: func(source struct {
					Path        string
					BucketName  string
					Region      string
					EndpointURL string
					AccessKey   string
					SecretKey   string
				}, opts struct {
					RoutingKey string
					Lock       bool
				}) (string, error) {
					return opts.RoutingKey, tt.storeErr
				},
			}
			fs := newUnitFS(flags)
			defer close(fs.shutdownCh)
			go fs.processCacheEvents()

			inode := NewInode(fs, nil, "file")
			inode.Attributes.Size = 1
			inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}

			if !fs.CacheFileInExternalCacheFromSource(inode, "", false) {
				t.Fatal("expected cache event to be queued")
			}

			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				fs.cachingStatusMu.Lock()
				_, ok := fs.cachingStatus["hash"]
				fs.cachingStatusMu.Unlock()
				if !ok {
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("cache status was not cleared")
		})
	}
}

func TestCacheThroughUsesLocalStagedSource(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{}
	fs := newUnitFS(flags)
	stagedPath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(stagedPath, []byte("cached"), 0600); err != nil {
		t.Fatal(err)
	}

	var storeContentCalls int32
	var storeFromS3Calls int32
	flags.ExternalCacheClient = &fakeContentCache{
		storeContent: func(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
			atomic.AddInt32(&storeContentCalls, 1)
			for range chunks {
			}
			return hash, nil
		},
		storeFromS3: func(source struct {
			Path        string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&storeFromS3Calls, 1)
			return opts.RoutingKey, nil
		},
	}
	defer close(fs.shutdownCh)
	go fs.processCacheEvents()

	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 6
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	if !fs.CacheFileInExternalCacheFromSource(inode, stagedPath, true) {
		t.Fatal("expected cache event to be queued")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&storeContentCalls) == 1 && atomic.LoadInt64(&fs.stats.cacheEventsSuccess) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&storeContentCalls); got != 1 {
		t.Fatalf("expected local StoreContent call, got %d", got)
	}
	if got := atomic.LoadInt32(&storeFromS3Calls); got != 0 {
		t.Fatalf("expected no StoreContentFromS3 calls, got %d", got)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stagedPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expected staged source to be removed after cache processing")
}

func TestCacheFileInExternalCacheFromSourceUsesLocalPathStore(t *testing.T) {
	dir := t.TempDir()
	stagedPath := filepath.Join(dir, "staged.bin")
	if err := os.WriteFile(stagedPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}

	flags := cfg.DefaultFlags()
	flags.CacheThroughModeEnabled = true
	flags.HashAttr = "sha256"
	flags.Backend = &cfg.S3Config{}
	var storeContentCalls int32
	var storeLocalPathCalls int32
	flags.ExternalCacheClient = &fakeContentCache{
		storeContent: func(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
			atomic.AddInt32(&storeContentCalls, 1)
			for range chunks {
			}
			return hash, nil
		},
		storeLocalPath: func(source struct {
			Path      string
			CachePath string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&storeLocalPathCalls, 1)
			if source.Path != stagedPath {
				t.Fatalf("unexpected local path: %q", source.Path)
			}
			if source.CachePath != "file" {
				t.Fatalf("unexpected cache path: %q", source.CachePath)
			}
			if !opts.Lock {
				t.Fatal("expected local path store to request lock")
			}
			return opts.RoutingKey, nil
		},
	}
	fs := newUnitFS(flags)
	defer close(fs.shutdownCh)
	go fs.processCacheEvents()

	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 6
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	if !fs.CacheFileInExternalCacheFromSource(inode, stagedPath, true) {
		t.Fatal("expected cache event to be queued")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&storeLocalPathCalls) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&storeLocalPathCalls); got != 1 {
		t.Fatalf("expected local path StoreContent call, got %d", got)
	}
	if got := atomic.LoadInt32(&storeContentCalls); got != 0 {
		t.Fatalf("expected no chunk StoreContent fallback, got %d", got)
	}
}

func TestCacheThroughFromFlushedBuffersUsesLocalBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("cache-through-data-"), 512*1024)
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])

	flags := cfg.DefaultFlags()
	flags.CacheThroughModeEnabled = true
	flags.HashAttr = "sha256"
	flags.ExternalCacheClient = &fakeContentCache{}

	var storeContentCalls int32
	var storeFromS3Calls int32
	flags.ExternalCacheClient = &fakeContentCache{
		storeContent: func(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
			atomic.AddInt32(&storeContentCalls, 1)
			hasher := sha256.New()
			for chunk := range chunks {
				if _, err := hasher.Write(chunk); err != nil {
					return "", err
				}
			}
			return hex.EncodeToString(hasher.Sum(nil)), nil
		},
		storeFromS3: func(source struct {
			Path        string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&storeFromS3Calls, 1)
			return opts.RoutingKey, nil
		},
	}

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(payload))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte(expectedHash)}
	inode.buffers.at.Set(uint64(len(payload)), &FileBuffer{
		offset: 0,
		length: uint64(len(payload)),
		data:   payload,
		ptr:    &BufferPointer{mem: payload, refs: 1},
		state:  BUF_FLUSHED_FULL,
	})

	inode.mu.Lock()
	ok := fs.CacheFileInExternalCacheFromBuffersLocked(inode)
	inode.mu.Unlock()

	if !ok {
		t.Fatal("expected flushed buffer cache-through to succeed")
	}
	if got := atomic.LoadInt32(&storeContentCalls); got != 1 {
		t.Fatalf("expected one StoreContent call, got %d", got)
	}
	if got := atomic.LoadInt32(&storeFromS3Calls); got != 0 {
		t.Fatalf("expected no StoreContentFromS3 calls, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 1 {
		t.Fatalf("expected one successful cache event, got %d", got)
	}
	if fs.cachingStatus[expectedHash] {
		t.Fatal("expected cache-through reservation to be cleared after success")
	}
	if _, _, err := inode.buffers.GetData(0, uint64(len(payload)), true); !errors.Is(err, ErrBufferIsMissing) {
		t.Fatalf("expected flushed cache-through buffers to be released, got err=%v", err)
	}
}

func TestReadThroughFromBuffersUsesLocalBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("read-through-data-"), 256*1024)
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])

	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"

	var storeContentCalls int32
	var storeFromS3Calls int32
	flags.ExternalCacheClient = &fakeContentCache{
		storeContent: func(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
			atomic.AddInt32(&storeContentCalls, 1)
			if hash != expectedHash || opts.RoutingKey != expectedHash {
				t.Fatalf("unexpected store request: hash=%q routing=%q", hash, opts.RoutingKey)
			}
			hasher := sha256.New()
			for chunk := range chunks {
				if _, err := hasher.Write(chunk); err != nil {
					return "", err
				}
			}
			return hex.EncodeToString(hasher.Sum(nil)), nil
		},
		storeFromS3: func(source struct {
			Path        string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			atomic.AddInt32(&storeFromS3Calls, 1)
			return opts.RoutingKey, nil
		},
	}

	fs := newUnitFS(flags)
	defer close(fs.shutdownCh)
	go fs.processCacheEvents()

	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(payload))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte(expectedHash)}
	inode.buffers.Add(0, payload, BUF_CLEAN, false)

	if !fs.CacheFileInExternalCacheFromReadBuffers(inode) {
		t.Fatal("expected read-through cache event to be queued")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&storeContentCalls) == 1 && atomic.LoadInt64(&fs.stats.cacheEventsSuccess) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&storeContentCalls); got != 1 {
		t.Fatalf("expected one StoreContent call, got %d", got)
	}
	if got := atomic.LoadInt32(&storeFromS3Calls); got != 0 {
		t.Fatalf("expected no StoreContentFromS3 calls, got %d", got)
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsSuccess); got != 1 {
		t.Fatalf("expected one successful cache event, got %d errors=%d mismatch=%d",
			got,
			atomic.LoadInt64(&fs.stats.cacheEventsErrors),
			atomic.LoadInt64(&fs.stats.cacheEventsMismatch),
		)
	}
}

func TestReadThroughFallbackQueuesReadBufferCacheEvent(t *testing.T) {
	payload := []byte("0123456789")
	expectedHash := "hash"

	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	flags.ExternalCacheClient = &fakeContentCache{
		getContent: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
			return nil, errContentNotFound
		},
	}

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(payload))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte(expectedHash)}
	inode.readCond = sync.NewCond(&inode.mu)

	backend := &TestBackend{
		GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
			return &GetBlobOutput{
				Body: io.NopCloser(bytes.NewReader(payload[param.Start : param.Start+param.Count])),
				HeadBlobOutput: HeadBlobOutput{
					BlobItemOutput: BlobItemOutput{Metadata: map[string]*string{}},
				},
			}, nil
		},
	}

	inode.retryRead(backend, "file", 0, uint64(len(payload)), false)

	select {
	case event := <-fs.cacheEventChan:
		if event.hash != expectedHash || event.path != "file" || event.size != uint64(len(payload)) {
			t.Fatalf("unexpected read-through cache event: %+v", event)
		}
		if !event.fromBuffers || event.localSourcePath != "" {
			t.Fatalf("expected read-buffer cache event, got %+v", event)
		}
	default:
		t.Fatal("expected read-through read-buffer cache event")
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsQueued); got != 1 {
		t.Fatalf("expected one queued cache event, got %d", got)
	}
}

func TestReadThroughNonZeroFallbackQueuesObjectSourceCacheEvent(t *testing.T) {
	payload := []byte("0123456789")
	expectedHash := "hash"

	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	flags.ExternalCacheClient = &fakeContentCache{
		getContent: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
			return nil, errContentNotFound
		},
	}

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = uint64(len(payload))
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte(expectedHash)}
	inode.readCond = sync.NewCond(&inode.mu)

	backend := &TestBackend{
		GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
			return &GetBlobOutput{
				Body: io.NopCloser(bytes.NewReader(payload[param.Start : param.Start+param.Count])),
				HeadBlobOutput: HeadBlobOutput{
					BlobItemOutput: BlobItemOutput{Metadata: map[string]*string{}},
				},
			}, nil
		},
	}

	inode.retryRead(backend, "file", 5, 5, false)

	select {
	case event := <-fs.cacheEventChan:
		if event.hash != expectedHash || event.path != "file" || event.size != uint64(len(payload)) {
			t.Fatalf("unexpected read-through cache event: %+v", event)
		}
		if event.fromBuffers || event.localSourcePath != "" {
			t.Fatalf("expected object-source cache event, got %+v", event)
		}
	default:
		t.Fatal("expected read-through object-source cache event")
	}
}

func TestCacheThroughAfterFlushQueuesObjectSource(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.CacheThroughModeEnabled = true
	flags.HashAttr = "sha256"
	flags.ExternalCacheClient = &fakeContentCache{}

	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 123
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}

	inode.mu.Lock()
	inode.queueCacheThroughAfterFlushLocked()
	inode.mu.Unlock()

	select {
	case event := <-fs.cacheEventChan:
		if event.hash != "hash" || event.path != "file" || event.size != 123 {
			t.Fatalf("unexpected cache-through event: %+v", event)
		}
		if event.fromBuffers || event.localSourcePath != "" {
			t.Fatalf("expected object-source cache-through event, got %+v", event)
		}
	default:
		t.Fatal("expected cache-through event")
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsQueued); got != 1 {
		t.Fatalf("expected one queued cache event, got %d", got)
	}
}

func TestDeferredHashMetadataPublishFailureDoesNotPoisonFlush(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	fs := newUnitFS(flags)
	done := make(chan struct{})
	backend := &TestBackend{
		CopyBlobFunc: func(param *CopyBlobInput) (*CopyBlobOutput, error) {
			close(done)
			return nil, syscall.EIO
		},
	}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CACHED)
	inode.Attributes.Size = 5
	inode.knownSize = 5
	inode.knownETag = "etag"
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.hashMetadataDirty = true

	inode.mu.Lock()
	inode.sendHashUpdateMeta()
	inode.mu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hash metadata publish")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inode.mu.Lock()
		syncing := inode.hashMetadataSync
		inode.mu.Unlock()
		if !syncing {
			break
		}
		time.Sleep(time.Millisecond)
	}

	inode.mu.Lock()
	defer inode.mu.Unlock()
	if inode.flushError != nil {
		t.Fatalf("deferred hash metadata error must not set flushError, got %v", inode.flushError)
	}
	if !inode.hashMetadataDirty {
		t.Fatal("expected transient hash metadata failure to remain dirty for background retry")
	}
	if inode.CacheState != ST_CACHED {
		t.Fatalf("expected file to remain cached after deferred metadata failure, got %v", inode.CacheState)
	}
}

func TestDeferredHashMetadataPublishCASFailureInvalidatesLocalView(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	fs := newUnitFS(flags)
	done := make(chan struct{})
	backend := &TestBackend{
		CopyBlobFunc: func(param *CopyBlobInput) (*CopyBlobOutput, error) {
			if param.ETag == nil || *param.ETag != "etag" {
				t.Fatalf("expected hash metadata copy to be guarded by current ETag, got %v", param.ETag)
			}
			close(done)
			return nil, syscall.EBUSY
		},
	}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CACHED)
	inode.Attributes.Size = 5
	inode.knownSize = 5
	inode.knownETag = "etag"
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.hashMetadataDirty = true
	inode.buffers.Add(0, []byte("hello"), BUF_CLEAN, true)

	inode.mu.Lock()
	inode.sendHashUpdateMeta()
	inode.mu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hash metadata publish")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inode.mu.Lock()
		syncing := inode.hashMetadataSync
		inode.mu.Unlock()
		if !syncing {
			break
		}
		time.Sleep(time.Millisecond)
	}

	inode.mu.Lock()
	defer inode.mu.Unlock()
	if inode.flushError != nil {
		t.Fatalf("CAS hash metadata failure must not set flushError, got %v", inode.flushError)
	}
	if inode.hashMetadataDirty {
		t.Fatal("expected CAS failure to stop retrying stale hash metadata")
	}
	if got := string(inode.userMetadata[flags.HashAttr]); got != "" {
		t.Fatalf("expected CAS failure to remove stale hash metadata, got %q", got)
	}
	if inode.hashMetadataChecked {
		t.Fatal("expected CAS failure to force metadata revalidation")
	}
	if inode.buffers.AnyUnclean() {
		t.Fatal("expected resetCache to drop local buffer state")
	}
}

func TestDeferredHashMetadataPublishParticipatesInFlushAccounting(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	flags.StagedWriteFlushTimeout = time.Second

	fs := newUnitFS(flags)
	started := make(chan struct{})
	release := make(chan struct{})
	backend := &TestBackend{
		CopyBlobFunc: func(param *CopyBlobInput) (*CopyBlobOutput, error) {
			close(started)
			<-release
			return &CopyBlobOutput{}, nil
		},
	}
	root := newRootWithBackend(fs, backend)
	inode := NewInode(fs, root, "file")
	inode.Id = 2
	inode.SetCacheState(ST_CACHED)
	inode.Attributes.Size = 5
	inode.knownSize = 5
	inode.knownETag = "etag"
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	inode.hashMetadataDirty = true

	inode.mu.Lock()
	inode.sendHashUpdateMeta()
	inode.mu.Unlock()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hash metadata publish")
	}

	if got := atomic.LoadInt64(&fs.activeFlushers); got != 1 {
		t.Fatalf("expected deferred hash metadata publish to count as active flusher, got %d", got)
	}

	waitDone := make(chan struct{})
	go func() {
		fs.WaitForFlush()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("WaitForFlush returned before deferred hash metadata publish completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for WaitForFlush")
	}

	if got := atomic.LoadInt64(&fs.activeFlushers); got != 0 {
		t.Fatalf("expected active flusher count to drain, got %d", got)
	}
}

func TestWaitForFlushWaitsForExternalCachePublish(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.StagedWriteFlushTimeout = time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	flags.ExternalCacheClient = &fakeContentCache{
		storeLocalPath: func(source struct {
			Path      string
			CachePath string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			close(started)
			<-release
			return opts.RoutingKey, nil
		},
	}

	fs := newUnitFS(flags)
	go fs.processCacheEvents()
	defer close(fs.shutdownCh)

	fs.cacheEventChan <- cacheEvent{path: "file", hash: "hash", size: 1, localSourcePath: "/tmp/file"}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cache publish to start")
	}

	waitDone := make(chan struct{})
	go func() {
		fs.WaitForFlush()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("WaitForFlush returned before external cache publish completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for WaitForFlush")
	}
}

func TestDetachFlushAndShutdownOrdersSuccessfulUnmount(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = ""
	flags.StagedWriteModeEnabled = true
	flags.StagedWriteFlushTimeout = time.Second
	fs := newUnitFS(flags)

	var detached atomic.Bool
	var uploaded atomic.Bool
	backend := &TestBackend{}
	backend.PutBlobFunc = func(param *PutBlobInput) (*PutBlobOutput, error) {
		if !detached.Load() {
			return nil, errors.New("staged flush ran before filesystem detach")
		}
		if atomic.LoadInt32(&fs.shutdown) != 0 {
			return nil, errors.New("filesystem shut down before staged flush")
		}
		if _, err := io.Copy(io.Discard, param.Body); err != nil {
			return nil, err
		}
		uploaded.Store(true)
		now := time.Now()
		return &PutBlobOutput{ETag: PString("etag"), LastModified: &now}, nil
	}
	root := newRootWithBackend(fs, backend)
	inode, _ := newStagedInodeForFlush(t, fs, root, []byte("accepted before detach"))

	err := fs.detachFlushAndShutdown(func() error {
		if uploaded.Load() {
			return errors.New("staged flush ran before detach callback")
		}
		detached.Store(true)
		return nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !uploaded.Load() {
		t.Fatal("successful detach did not flush accepted staged data")
	}
	if atomic.LoadInt32(&fs.shutdown) != 1 {
		t.Fatal("filesystem did not shut down after staged flush")
	}
	inode.mu.Lock()
	stagedFile := inode.StagedFile
	inode.mu.Unlock()
	if stagedFile != nil {
		t.Fatal("staged file remained attached after successful unmount lifecycle")
	}
}

func TestDetachFlushAndShutdownPreservesUnmountErrorPolicies(t *testing.T) {
	detachErr := errors.New("detach failed")

	unixLike := newUnitFS(cfg.DefaultFlags())
	err := unixLike.detachFlushAndShutdown(func() error { return detachErr }, true)
	if !errors.Is(err, detachErr) {
		t.Fatalf("Unix-like detach error = %v, want %v", err, detachErr)
	}
	if atomic.LoadInt32(&unixLike.shutdown) != 1 {
		t.Fatal("Unix-like detach failure did not preserve shutdown cleanup")
	}

	windowsLike := newUnitFS(cfg.DefaultFlags())
	err = windowsLike.detachFlushAndShutdown(func() error { return detachErr }, false)
	if !errors.Is(err, detachErr) {
		t.Fatalf("Windows-like detach error = %v, want %v", err, detachErr)
	}
	if atomic.LoadInt32(&windowsLike.shutdown) != 0 {
		t.Fatal("Windows-like detach failure stopped a still-mounted filesystem")
	}
	windowsLike.Shutdown()
}

func TestOrdinaryCacheProducerCallbackCanShutdown(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	flags.ExternalCacheClient = &fakeContentCache{}
	var fs *Goofys
	callbackDone := make(chan struct{})
	flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		fs.Shutdown()
		close(callbackDone)
	}
	fs = newUnitFS(flags)
	inode := NewInode(fs, nil, "model.bin")
	inode.Attributes.Size = 4
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}

	if fs.CacheFileInExternalCacheFromSource(inode, "", false) {
		t.Fatal("cache event was accepted after callback initiated shutdown")
	}
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("cache-trigger callback deadlocked during shutdown")
	}
	if queued := len(fs.cacheEventChan); queued != 0 {
		t.Fatalf("cache events queued after shutdown = %d, want 0", queued)
	}
	fs.cachingStatusMu.Lock()
	statusCount := len(fs.cachingStatus)
	fs.cachingStatusMu.Unlock()
	if statusCount != 0 {
		t.Fatalf("cache reservations retained after rejected submission = %d", statusCount)
	}
}

func TestOrdinaryCacheProducerRejectedAfterConsumerShutdown(t *testing.T) {
	flags := cfg.DefaultFlags()
	flags.HashAttr = "sha256"
	flags.ExternalCacheClient = &fakeContentCache{}
	fs := newUnitFS(flags)
	processorDone := make(chan struct{})
	go func() {
		fs.processCacheEvents()
		close(processorDone)
	}()
	fs.Shutdown()
	select {
	case <-processorDone:
	case <-time.After(time.Second):
		t.Fatal("cache event consumer did not exit")
	}

	inode := NewInode(fs, nil, "model.bin")
	inode.Attributes.Size = 4
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	if fs.CacheFileInExternalCacheFromSource(inode, "", false) {
		t.Fatal("ordinary producer queued after cache event consumer exited")
	}
	if queued := len(fs.cacheEventChan); queued != 0 {
		t.Fatalf("cache events stranded after consumer exit = %d, want 0", queued)
	}
}

func BenchmarkExternalCacheLargeOutput(b *testing.B) {
	for _, size := range []int64{10 << 20, 100 << 20, 1 << 30} {
		size := size
		b.Run(byteSizeName(size), func(b *testing.B) {
			if size >= 1<<30 && os.Getenv("GEESEFS_RUN_LARGE_BENCH") == "" {
				b.Skip("set GEESEFS_RUN_LARGE_BENCH=1 to run 1GB benchmark")
			}
			payload := bytes.Repeat([]byte{7}, int(size))
			flags := cfg.DefaultFlags()
			flags.ExternalCacheClient = &fakeContentCache{
				getContent: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
					return payload[offset : offset+length], nil
				},
			}
			fs := newUnitFS(flags)
			inode := NewInode(fs, nil, "file")
			inode.Attributes.Size = uint64(size)
			inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
			inode.readCond = sync.NewCond(&inode.mu)
			backend := &TestBackend{}
			backend.GetBlobFunc = func(param *GetBlobInput) (*GetBlobOutput, error) {
				b.Fatal("cache-hit benchmark should not read from S3")
				return nil, nil
			}

			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				inode.buffers.RemoveRange(0, uint64(size), nil)
				inode.retryRead(backend, "file", 0, uint64(size), false)
			}
		})
	}
}

func byteSizeName(size int64) string {
	switch size {
	case 10 << 20:
		return "10MB"
	case 100 << 20:
		return "100MB"
	case 1 << 30:
		return "1GB"
	default:
		return "custom"
	}
}
