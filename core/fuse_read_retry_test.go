//go:build !windows

package core

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestFUSEReadRetriesCleanConditionalIdentityConflict(t *testing.T) {
	payload := []byte(`{"model_type":"qwen3"}`)
	const (
		staleETag = "etag-v1"
		freshETag = "etag-v2"
	)
	var headCalls atomic.Int32
	var getCalls atomic.Int32
	backend := &TestBackend{
		HeadBlobFunc: func(param *HeadBlobInput) (*HeadBlobOutput, error) {
			headCalls.Add(1)
			return &HeadBlobOutput{BlobItemOutput: BlobItemOutput{
				Key:      &param.Key,
				ETag:     PString(freshETag),
				Size:     uint64(len(payload)),
				Metadata: map[string]*string{},
			}}, nil
		},
		GetBlobFunc: func(param *GetBlobInput) (*GetBlobOutput, error) {
			getCalls.Add(1)
			if param.IfMatch == nil {
				t.Fatal("conditional read omitted If-Match")
			}
			if *param.IfMatch == staleETag {
				return nil, syscall.EBUSY
			}
			if *param.IfMatch != freshETag {
				t.Fatalf("conditional read used ETag %q", *param.IfMatch)
			}
			start := min(param.Start, uint64(len(payload)))
			end := min(param.Start+param.Count, uint64(len(payload)))
			return &GetBlobOutput{
				Body: io.NopCloser(bytes.NewReader(payload[start:end])),
				HeadBlobOutput: HeadBlobOutput{BlobItemOutput: BlobItemOutput{
					Key:      &param.Key,
					ETag:     PString(freshETag),
					Size:     uint64(len(payload)),
					Metadata: map[string]*string{},
				}},
			}, nil
		},
	}

	flags := cfg.DefaultFlags()
	fs := newUnitFS(flags)
	fs.inodes = make(map[fuseops.InodeID]*Inode)
	fs.fileHandles = make(map[fuseops.HandleID]*FileHandle)
	root := newRootWithBackend(fs, backend)
	fs.inodes[root.Id] = root

	inode := NewInode(fs, root, "config.json")
	inode.Id = 2
	inode.Attributes.Size = uint64(len(payload))
	inode.knownSize = uint64(len(payload))
	inode.knownETag = staleETag
	inode.hashMetadataChecked = true
	inode.SetCacheState(ST_CACHED)
	root.insertChild(inode)
	fs.inodes[inode.Id] = inode

	const handleID fuseops.HandleID = 7
	fs.fileHandles[handleID] = NewFileHandle(inode)
	op := &fuseops.ReadFileOp{
		Inode:  inode.Id,
		Handle: handleID,
		Offset: 0,
		Size:   int64(len(payload)),
	}
	if err := NewGoofysFuse(fs).ReadFile(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(op.Data, nil); !bytes.Equal(got, payload) {
		t.Fatalf("read data = %q, want %q", got, payload)
	}
	if op.BytesRead != len(payload) {
		t.Fatalf("bytes read = %d, want %d", op.BytesRead, len(payload))
	}
	if got := getCalls.Load(); got != 2 {
		t.Fatalf("origin reads = %d, want stale attempt plus retry", got)
	}
	if got := headCalls.Load(); got != 1 {
		t.Fatalf("metadata refreshes = %d, want 1", got)
	}
}
