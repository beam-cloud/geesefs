package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/jacobsa/fuse/fuseops"
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

func TestReadThroughFromBuffersDoesNotQueueFromPartialEOFRead(t *testing.T) {
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
		t.Fatalf("partial EOF read should not queue whole-file read-through cache event: %+v", event)
	default:
	}
	if got := atomic.LoadInt64(&fs.stats.cacheEventsQueued); got != 0 {
		t.Fatalf("expected no queued cache events, got %d", got)
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
