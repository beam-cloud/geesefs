package core

import (
	"bytes"
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

			deadline := time.Now().Add(time.Second)
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
		if atomic.LoadInt32(&storeContentCalls) == 1 {
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
