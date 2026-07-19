//go:build !windows

package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

type readThroughCache struct {
	*fakeContentCache
	materializeLocal   func(context.Context, string, int64, struct{ RoutingKey string }) (bool, error)
	materializeS3Local func(context.Context, struct {
		Path        string
		CachePath   string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}, struct {
		RoutingKey string
		Lock       bool
	}) (string, error)
}

func (c *readThroughCache) MaterializeLocal(ctx context.Context, hash string, size int64, opts struct{ RoutingKey string }) (bool, error) {
	if c.materializeLocal == nil {
		return true, nil
	}
	return c.materializeLocal(ctx, hash, size, opts)
}

func (c *readThroughCache) MaterializeS3Local(ctx context.Context, source struct {
	Path        string
	CachePath   string
	BucketName  string
	Region      string
	EndpointURL string
	AccessKey   string
	SecretKey   string
}, opts struct {
	RoutingKey string
	Lock       bool
}) (string, error) {
	if c.materializeS3Local != nil {
		return c.materializeS3Local(ctx, source, opts)
	}
	return c.StoreContentFromS3(source, opts)
}

func newReadThroughTestFile(t *testing.T, payload []byte, cache cfg.ContentCache, backend *TestBackend) (*Goofys, *Inode, *FileHandle) {
	t.Helper()
	flags := cfg.DefaultFlags()
	flags.Backend = (&cfg.S3Config{}).Init()
	flags.CacheThroughModeEnabled = true
	flags.ExternalCacheClient = cache
	flags.HashAttr = "sha256"
	flags.MinFileSizeForHashKB = 0
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
	inode.hashMetadataChecked = true
	inode.SetCacheState(ST_CACHED)
	inode.userMetadata = map[string][]byte{"content-type": []byte("application/octet-stream")}
	fh, err := inode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}
	return fs, inode, fh
}

func stableReadThroughBackend(payload []byte, etag *atomic.Value) *TestBackend {
	return &TestBackend{
		HeadBlobFunc: func(param *HeadBlobInput) (*HeadBlobOutput, error) {
			currentETag := etag.Load().(string)
			return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
				Key:      &param.Key,
				ETag:     &currentETag,
				Size:     uint64(len(payload)),
				Metadata: map[string]*string{},
			}}, nil
		},
		GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
			end := param.Start + param.Count
			if end > uint64(len(payload)) {
				return nil, fmt.Errorf("read beyond payload: start=%d count=%d", param.Start, param.Count)
			}
			currentETag := etag.Load().(string)
			return &GetBlobOutput{
				Body: io.NopCloser(bytes.NewReader(payload[param.Start:end])),
				HeadBlobOutput: HeadBlobOutput{BlobItemOutput: BlobItemOutput{
					Key:      &param.Key,
					ETag:     &currentETag,
					Size:     param.Count,
					Metadata: map[string]*string{},
				}},
			}, nil
		},
		CopyBlobFunc: func(*CopyBlobInput) (*CopyBlobOutput, error) {
			return &CopyBlobOutput{}, nil
		},
	}
}

func TestReadThroughMaterializesBeforeFirstRead(t *testing.T) {
	payload := bytes.Repeat([]byte("model-weights-"), 1024)
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	var materializeCalls atomic.Int32
	cache := &readThroughCache{
		fakeContentCache: &fakeContentCache{},
		materializeS3Local: func(_ context.Context, source struct {
			Path        string
			CachePath   string
			BucketName  string
			Region      string
			EndpointURL string
			AccessKey   string
			SecretKey   string
		}, opts struct {
			RoutingKey string
			Lock       bool
		}) (string, error) {
			materializeCalls.Add(1)
			if source.Path != "model.bin" || source.CachePath == "" || !opts.Lock || opts.RoutingKey == "" {
				t.Fatalf("unexpected materialization request: source=%+v opts=%+v", source, opts)
			}
			return expectedHash, nil
		},
	}
	var etag atomic.Value
	etag.Store("etag-v1")
	fs, inode, fh := newReadThroughTestFile(t, payload, cache, stableReadThroughBackend(payload, &etag))
	defer close(fs.shutdownCh)
	defer fh.Release()

	fh.materializeReadThrough(0)

	if materializeCalls.Load() != 1 {
		t.Fatalf("expected one direct local materialization, got %d", materializeCalls.Load())
	}
	inode.mu.Lock()
	actualHash := string(inode.userMetadata[fs.flags.HashAttr])
	inode.mu.Unlock()
	if actualHash != expectedHash {
		t.Fatalf("expected published hash %q, got %q", expectedHash, actualHash)
	}
}

func TestReadThroughCoalescesConcurrentReaders(t *testing.T) {
	payload := []byte("model-weights")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	started := make(chan struct{})
	release := make(chan struct{})
	var storeCalls atomic.Int32
	cache := &readThroughCache{fakeContentCache: &fakeContentCache{storeFromS3: func(source struct {
		Path        string
		CachePath   string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		if storeCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return expectedHash, nil
	}}}
	var etag atomic.Value
	etag.Store("etag-v1")
	fs, _, first := newReadThroughTestFile(t, payload, cache, stableReadThroughBackend(payload, &etag))
	defer close(fs.shutdownCh)
	defer first.Release()
	second, err := first.inode.OpenFile()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	done := make(chan struct{}, 2)
	go func() { first.materializeReadThrough(0); done <- struct{}{} }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("materialization did not start")
	}
	go func() { second.materializeReadThrough(0); done <- struct{}{} }()
	time.Sleep(10 * time.Millisecond)
	close(release)
	<-done
	<-done

	if storeCalls.Load() != 1 {
		t.Fatalf("expected one coalesced store, got %d", storeCalls.Load())
	}
}

func TestReadThroughDoesNotPublishChangedObject(t *testing.T) {
	payload := []byte("model-weights")
	sum := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(sum[:])
	var etag atomic.Value
	etag.Store("etag-v1")
	cache := &readThroughCache{fakeContentCache: &fakeContentCache{storeFromS3: func(source struct {
		Path        string
		CachePath   string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error) {
		etag.Store("etag-v2")
		return expectedHash, nil
	}}}
	fs, inode, fh := newReadThroughTestFile(t, payload, cache, stableReadThroughBackend(payload, &etag))
	defer close(fs.shutdownCh)
	defer fh.Release()

	fh.materializeReadThrough(0)

	inode.mu.Lock()
	actualHash := string(inode.userMetadata[fs.flags.HashAttr])
	inode.mu.Unlock()
	if actualHash != "" {
		t.Fatalf("changed object published hash %q", actualHash)
	}
}
