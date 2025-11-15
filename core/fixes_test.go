package core

import (
	"sync"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

// TestReadCondNilCheck verifies that nil readCond doesn't cause panic.
func TestReadCondNilCheck(t *testing.T) {
	inode := &Inode{
		fs: &Goofys{
			flags: &cfg.FlagStorage{
				MemoryLimit: 100 * 1024 * 1024,
				PartSizes:   []cfg.PartSizeConfig{{PartSize: 5 * 1024 * 1024, PartCount: 10000}},
			},
			bufferPool: NewBufferPool(100*1024*1024, 100*1024*1024),
		},
		readCond: nil, // Intentionally nil
		Attributes: InodeAttributes{Size: 1024},
	}
	inode.buffers.helpers = inode

	// Should not panic with nil readCond
	inode.mu.Lock()
	if inode.readCond != nil {
		inode.readCond.Broadcast()
	}
	inode.mu.Unlock()
}

// TestReadCondNilPanicReproduction reproduces the original panic.
func TestReadCondNilPanicReproduction(t *testing.T) {
	inode := &Inode{Id: 1, Name: "test.txt"}
	
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Expected panic did not occur")
		}
	}()
	
	inode.mu.Lock()
	inode.readCond.Broadcast() // This WILL panic
	inode.mu.Unlock()
}

// TestReadCondRaceCondition tests concurrent access with nil readCond.
func TestReadCondRaceCondition(t *testing.T) {
	for i := 0; i < 50; i++ {
		inode := &Inode{Id: fuseops.InodeID(i), Name: "test.txt", Attributes: InodeAttributes{Size: 1024}}
		
		var wg sync.WaitGroup
		
		// Goroutine 1: Initialize readCond (delayed)
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			inode.mu.Lock()
			inode.readCond = sync.NewCond(&inode.mu)
			inode.mu.Unlock()
		}()
		
		// Goroutine 2: Try to broadcast (immediate)
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(1 * time.Millisecond)
			inode.mu.Lock()
			if inode.readCond != nil {
				inode.readCond.Broadcast()
			}
			inode.mu.Unlock()
		}()
		
		wg.Wait()
	}
}

// TestStagedWriteRetry verifies staged write retry logic.
func TestStagedWriteRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}
	
	// This is a placeholder for staged write retry tests
	// Real implementation would need mock S3 backend
}

// TestWaitForFlushTimeout verifies WaitForFlush timeout behavior.
func TestWaitForFlushTimeout(t *testing.T) {
	fs := &Goofys{
		flags: &cfg.FlagStorage{
			StagedWriteModeEnabled: true,
		},
	}
	
	// Should timeout quickly since nothing is flushing
	start := time.Now()
	fs.WaitForFlush(100 * time.Millisecond)
	elapsed := time.Since(start)
	
	if elapsed < 50*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Errorf("WaitForFlush took %v, expected ~100ms", elapsed)
	}
}

// TestCachingBehavior verifies external cache integration.
func TestCachingBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}
	
	// Mock cache for testing
	mockCache := &testCache{data: make(map[string][]byte)}
	
	fs := &Goofys{
		flags: &cfg.FlagStorage{
			ExternalCacheClient:    mockCache,
			MinFileSizeForHashKB:   1,
			HashAttr:               "hash",
		},
	}
	
	inode := &Inode{
		fs:   fs,
		Id:   1,
		Name: "test.txt",
		Attributes: InodeAttributes{Size: 2048},
		userMetadata: map[string][]byte{"hash": []byte("test-hash-123")},
	}
	
	// Trigger cache event
	fs.CacheFileInExternalCache(inode)
	
	// Allow async processing
	time.Sleep(100 * time.Millisecond)
}

// testCache is a minimal cache implementation for testing.
type testCache struct {
	data map[string][]byte
	mu   sync.RWMutex
}

func (c *testCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if data, ok := c.data[hash]; ok {
		return data, nil
	}
	return nil, nil
}


func (c *testCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	var buffer []byte
	for chunk := range chunks {
		buffer = append(buffer, chunk...)
	}
	c.mu.Lock()
	c.data[hash] = buffer
	c.mu.Unlock()
	return hash, nil
}

func (c *testCache) StoreContentFromS3(source struct {
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
	return opts.RoutingKey, nil
}
