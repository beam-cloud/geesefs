// Copyright 2025
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

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

// TestStagedWriteWithCaching tests the full flow:
// 1. Write file to staged location
// 2. Flush to S3
// 3. Cache in external cache
// 4. Read back from cache
func TestStagedWriteWithCaching(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "true" {
		t.Skip("Skipping integration test - set RUN_INTEGRATION_TESTS=true to run")
	}

	// Create temp directories
	tmpDir, err := os.MkdirTemp("", "geesefs-integration-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	stagedDir := filepath.Join(tmpDir, "staged")
	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(stagedDir, 0755)
	os.MkdirAll(cacheDir, 0755)

	// Create mock cache that tracks operations
	mockCache := &TrackingMockCache{
		storage:    make(map[string][]byte),
		operations: make([]string, 0),
	}

	// Track cache events
	cacheEventsTriggered := make([]map[string]interface{}, 0)
	eventCallback := func(event cfg.EventType, data map[string]interface{}) {
		t.Logf("Event: %s, Data: %+v", event, data)
		if event == cfg.EventCacheTriggered {
			cacheEventsTriggered = append(cacheEventsTriggered, data)
		}
	}

	flags := &cfg.FlagStorage{
		// S3 Config (LocalStack)
		Endpoint: "http://localhost:4566",
		Backend: &cfg.S3Config{
			Region:    "us-east-1",
			AccessKey: "test",
			SecretKey: "test",
		},

		// Basic config
		DirMode:  0755,
		FileMode: 0644,
		Uid:      uint32(os.Getuid()),
		Gid:      uint32(os.Getgid()),

		// Staged write mode
		StagedWriteModeEnabled:      true,
		StagedWritePath:             stagedDir,
		StagedWriteDebounce:         50 * time.Millisecond, // Short for testing
		StagedWriteFlushTimeout:     10 * time.Second,
		StagedWriteFlushSize:        1 * 1024 * 1024, // 1MB chunks
		StagedWriteFlushInterval:    100 * time.Millisecond,
		StagedWriteFlushConcurrency: 2,

		// Cache config
		CacheThroughModeEnabled:       true, // Enable caching on write!
		ExternalCacheClient:           mockCache,
		ExternalCacheStreamingEnabled: false,
		HashAttr:                      "hash",
		MinFileSizeForHashKB:          1, // Cache everything >= 1KB

		// Memory settings
		MemoryLimit:    100 * 1024 * 1024,
		MaxFlushers:    4,
		MaxParallelParts: 2,
		StatCacheTTL:   1 * time.Second,
		HTTPTimeout:    10 * time.Second,

		// Part sizes
		PartSizes: []cfg.PartSizeConfig{
			{PartSize: 5 * 1024 * 1024, PartCount: 10000},
		},

		// Event callback
		EventCallback: eventCallback,
	}

	// Create filesystem
	ctx := context.Background()
	fs, err := NewGoofys(ctx, "test-bucket", flags)
	if err != nil {
		t.Fatalf("Failed to create filesystem: %v", err)
	}
	defer fs.Shutdown()

	// Test: Write a file through staged write
	t.Run("WriteAndCacheFile", func(t *testing.T) {
		testData := []byte("Hello from staged write! This is test data for caching.")
		expectedHash := sha256.Sum256(testData)
		expectedHashStr := hex.EncodeToString(expectedHash[:])

		t.Logf("Test data size: %d bytes", len(testData))
		t.Logf("Expected hash: %s", expectedHashStr)

		// Create root inode and file inode
		root := fs.inodes[1] // Root inode
		if root == nil {
			t.Fatal("Root inode not found")
		}

		// Create test file
		fileName := "test-file.txt"
		inode := NewInode(fs, root, fileName)
		inode.Attributes.Size = 0
		
		// Add to filesystem
		fs.mu.Lock()
		fs.nextInodeID++
		inode.Id = fs.nextInodeID
		fs.inodes[inode.Id] = inode
		fs.mu.Unlock()

		// Create file handle
		fh := NewFileHandle(inode)

		// Write data
		t.Log("Writing data to file...")
		err := fh.WriteFile(0, testData, false)
		if err != nil {
			t.Fatalf("Failed to write data: %v", err)
		}

		// Wait for staged file to be created
		time.Sleep(200 * time.Millisecond)

		// Check if staged file exists
		stagedPath := filepath.Join(stagedDir, fileName)
		if _, err := os.Stat(stagedPath); err != nil {
			t.Errorf("Staged file not created at %s: %v", stagedPath, err)
		} else {
			t.Logf("✓ Staged file created at %s", stagedPath)
		}

		// Sync file (should trigger flush and caching)
		t.Log("Syncing file to S3...")
		err = inode.SyncFile()
		if err != nil {
			t.Fatalf("Failed to sync file: %v", err)
		}

		// Wait for cache processing
		time.Sleep(1 * time.Second)

		// Verify hash was computed
		inode.mu.Lock()
		hash, hasHash := inode.userMetadata["hash"]
		inode.mu.Unlock()

		if !hasHash {
			t.Error("❌ Hash not computed for file")
		} else {
			t.Logf("✓ Hash computed: %s", string(hash))
			if string(hash) != expectedHashStr {
				t.Errorf("Hash mismatch: expected %s, got %s", expectedHashStr, string(hash))
			}
		}

		// Check cache operations
		mockCache.mu.Lock()
		ops := mockCache.operations
		mockCache.mu.Unlock()

		t.Logf("Cache operations: %+v", ops)

		// Verify file was stored in cache
		if len(ops) == 0 {
			t.Error("❌ No cache operations performed")
		} else {
			hasStore := false
			for _, op := range ops {
				if op == "StoreContentFromS3" {
					hasStore = true
					break
				}
			}
			if !hasStore {
				t.Error("❌ File was not stored in external cache")
			} else {
				t.Log("✓ File stored in external cache")
			}
		}

		// Verify cache events were triggered
		if len(cacheEventsTriggered) == 0 {
			t.Error("❌ No cache events triggered")
		} else {
			t.Logf("✓ %d cache event(s) triggered", len(cacheEventsTriggered))
		}

		// Verify data is in cache
		mockCache.mu.Lock()
		cachedData, inCache := mockCache.storage[expectedHashStr]
		mockCache.mu.Unlock()

		if !inCache {
			t.Error("❌ Data not found in cache storage")
		} else {
			t.Logf("✓ Data found in cache (size: %d bytes)", len(cachedData))
			if string(cachedData) != string(testData) {
				t.Error("❌ Cached data doesn't match original")
			} else {
				t.Log("✓ Cached data matches original")
			}
		}
	})

	t.Run("ReadFromCache", func(t *testing.T) {
		// TODO: Test reading back from cache
		t.Log("Cache read test would go here")
	})
}

// TrackingMockCache tracks all operations for testing
type TrackingMockCache struct {
	storage    map[string][]byte
	operations []string
	mu         sync.Mutex
}

func (m *TrackingMockCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.operations = append(m.operations, "GetContent")

	data, ok := m.storage[hash]
	if !ok {
		return nil, fmt.Errorf("content not found")
	}

	if offset >= int64(len(data)) {
		return nil, fmt.Errorf("offset out of range")
	}

	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}

	result := make([]byte, end-offset)
	copy(result, data[offset:end])
	return result, nil
}

func (m *TrackingMockCache) GetContentStream(hash string, offset int64, length int64, opts struct {
	RoutingKey string
}) (chan []byte, error) {
	m.mu.Lock()
	m.operations = append(m.operations, "GetContentStream")
	m.mu.Unlock()

	data, err := m.GetContent(hash, offset, length, opts)
	if err != nil {
		return nil, err
	}

	ch := make(chan []byte, 1)
	ch <- data
	close(ch)
	return ch, nil
}

func (m *TrackingMockCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.operations = append(m.operations, "StoreContent")

	var buffer []byte
	for chunk := range chunks {
		buffer = append(buffer, chunk...)
	}
	m.storage[hash] = buffer
	return hash, nil
}

func (m *TrackingMockCache) StoreContentFromS3(source struct {
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
	m.mu.Lock()
	defer m.mu.Unlock()

	m.operations = append(m.operations, "StoreContentFromS3")

	// For testing, we simulate successful storage
	// In real implementation, this would fetch from S3
	hash := opts.RoutingKey

	// Simulate storing the content
	testData := []byte("Simulated S3 content for: " + source.Path)
	m.storage[hash] = testData

	return hash, nil
}
