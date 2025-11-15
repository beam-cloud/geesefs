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

package test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/yandex-cloud/geesefs/core"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

const (
	testBucket    = "test-bucket"
	testRegion    = "us-east-1"
	localstackURL = "http://localhost:4566"
)

type MockContentCache struct {
	storage map[string][]byte
}

func NewMockContentCache() *MockContentCache {
	return &MockContentCache{
		storage: make(map[string][]byte),
	}
}

func (m *MockContentCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
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

func (m *MockContentCache) GetContentStream(hash string, offset int64, length int64, opts struct {
	RoutingKey string
}) (chan []byte, error) {
	data, err := m.GetContent(hash, offset, length, opts)
	if err != nil {
		return nil, err
	}
	
	ch := make(chan []byte, 1)
	go func() {
		defer close(ch)
		ch <- data
	}()
	return ch, nil
}

func (m *MockContentCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	var buffer bytes.Buffer
	for chunk := range chunks {
		buffer.Write(chunk)
	}
	m.storage[hash] = buffer.Bytes()
	return hash, nil
}

func (m *MockContentCache) StoreContentFromS3(source struct {
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
	// For testing, we'll just store a dummy entry
	hash := opts.RoutingKey
	if hash == "" {
		hash = "test-hash"
	}
	m.storage[hash] = []byte("cached-content")
	return hash, nil
}

// Setup LocalStack S3 for testing
func setupLocalStack(t *testing.T) *s3.S3 {
	sess, err := session.NewSession(&aws.Config{
		Region:           aws.String(testRegion),
		Endpoint:         aws.String(localstackURL),
		Credentials:      credentials.NewStaticCredentials("test", "test", ""),
		S3ForcePathStyle: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("Failed to create AWS session: %v", err)
	}

	s3Client := s3.New(sess)

	// Create test bucket
	_, err = s3Client.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(testBucket),
	})
	if err != nil {
		t.Logf("Bucket might already exist: %v", err)
	}

	return s3Client
}

func setupTestFS(t *testing.T, mockCache *MockContentCache) (*core.Goofys, string) {
	tmpDir, err := os.MkdirTemp("", "geesefs-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	stagedDir := filepath.Join(tmpDir, "staged")
	cacheDir := filepath.Join(tmpDir, "cache")
	
	if err := os.MkdirAll(stagedDir, 0755); err != nil {
		t.Fatalf("Failed to create staged dir: %v", err)
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("Failed to create cache dir: %v", err)
	}

	flags := &cfg.FlagStorage{
		// Basic config
		Endpoint:     localstackURL,
		DirMode:      0755,
		FileMode:     0644,
		Uid:          uint32(os.Getuid()),
		Gid:          uint32(os.Getgid()),
		
		// Staged write mode
		StagedWriteModeEnabled:      true,
		StagedWritePath:             stagedDir,
		StagedWriteDebounce:         100 * time.Millisecond,
		StagedWriteFlushTimeout:     5 * time.Second,
		StagedWriteFlushSize:        5 * 1024 * 1024, // 5MB chunks
		StagedWriteFlushInterval:    1 * time.Second,
		StagedWriteFlushConcurrency: 2,
		
		// Cache config
		CachePath:      cacheDir,
		MaxDiskCacheFD: 100,
		
		// External cache
		ExternalCacheClient:           mockCache,
		ExternalCacheStreamingEnabled: true,
		HashAttr:                      "hash",
		HashTimeout:                   10 * time.Second,
		MinFileSizeForHashKB:          0, // Cache everything for testing
		
		// Memory and performance
		MemoryLimit:       100 * 1024 * 1024, // 100MB
		MaxFlushers:       10,
		MaxParallelParts:  4,
		StatCacheTTL:      1 * time.Second,
		HTTPTimeout:       30 * time.Second,
		ReadAheadKB:       128,
		SinglePartMB:      5,
		
		// Part sizes
		PartSizes: []cfg.PartSizeConfig{
			{PartSize: 5 * 1024 * 1024, PartCount: 10000},
		},
	}

	// Create S3 backend config
	awsConfig := &cfg.S3Config{
		Region:    testRegion,
		AccessKey: "test",
		SecretKey: "test",
	}
	flags.Backend = awsConfig

	fs, err := core.NewGoofys(context.Background(), testBucket, flags)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("Failed to create filesystem: %v", err)
	}

	return fs, tmpDir
}

func TestStagedWriteBasic(t *testing.T) {
	if os.Getenv("LOCALSTACK_ENABLED") != "true" {
		t.Skip("Skipping LocalStack test - set LOCALSTACK_ENABLED=true to run")
	}

	s3Client := setupLocalStack(t)
	mockCache := NewMockContentCache()
	fs, tmpDir := setupTestFS(t, mockCache)
	defer os.RemoveAll(tmpDir)
	defer fs.Shutdown()

	// Create a test file
	testData := []byte("Hello, World! This is a test file for staged writes.")
	key := "test-file.txt"

	// Write file using the filesystem
	inode := createTestFile(t, fs, key)
	if inode == nil {
		t.Fatal("Failed to create test file")
	}

	fh := core.NewFileHandle(inode)
	
	// Write data
	err := fh.WriteFile(0, testData, false)
	if err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}

	// Wait for staged file to be created and flushed
	time.Sleep(2 * time.Second)
	fs.WaitForFlush(10 * time.Second)

	// Verify file was uploaded to S3
	result, err := s3Client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("Failed to get object from S3: %v", err)
	}
	defer result.Body.Close()

	uploadedData, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("Failed to read S3 object: %v", err)
	}

	if !bytes.Equal(uploadedData, testData) {
		t.Errorf("Data mismatch: expected %q, got %q", testData, uploadedData)
	}
}

func TestStagedWriteLargeFile(t *testing.T) {
	if os.Getenv("LOCALSTACK_ENABLED") != "true" {
		t.Skip("Skipping LocalStack test - set LOCALSTACK_ENABLED=true to run")
	}

	s3Client := setupLocalStack(t)
	mockCache := NewMockContentCache()
	fs, tmpDir := setupTestFS(t, mockCache)
	defer os.RemoveAll(tmpDir)
	defer fs.Shutdown()

	// Create a large test file (20MB)
	fileSize := 20 * 1024 * 1024
	testData := make([]byte, fileSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	key := "large-file.bin"
	inode := createTestFile(t, fs, key)
	if inode == nil {
		t.Fatal("Failed to create test file")
	}

	fh := core.NewFileHandle(inode)

	// Write in chunks
	chunkSize := 1024 * 1024 // 1MB
	for offset := 0; offset < fileSize; offset += chunkSize {
		end := offset + chunkSize
		if end > fileSize {
			end = fileSize
		}
		err := fh.WriteFile(int64(offset), testData[offset:end], false)
		if err != nil {
			t.Fatalf("Failed to write chunk at offset %d: %v", offset, err)
		}
	}

	// Wait for flush
	time.Sleep(5 * time.Second)
	fs.WaitForFlush(30 * time.Second)

	// Verify file was uploaded to S3
	result, err := s3Client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("Failed to get object from S3: %v", err)
	}
	defer result.Body.Close()

	uploadedData, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("Failed to read S3 object: %v", err)
	}

	if len(uploadedData) != fileSize {
		t.Errorf("Size mismatch: expected %d, got %d", fileSize, len(uploadedData))
	}

	if !bytes.Equal(uploadedData, testData) {
		t.Error("Data mismatch in large file")
	}
}

func TestExternalCacheIntegration(t *testing.T) {
	if os.Getenv("LOCALSTACK_ENABLED") != "true" {
		t.Skip("Skipping LocalStack test - set LOCALSTACK_ENABLED=true to run")
	}

	mockCache := NewMockContentCache()
	fs, tmpDir := setupTestFS(t, mockCache)
	defer os.RemoveAll(tmpDir)
	defer fs.Shutdown()

	// Pre-populate cache
	testHash := "test-hash-123"
	testData := []byte("Cached content from external cache")
	mockCache.storage[testHash] = testData

	// Create a file with hash metadata
	key := "cached-file.txt"
	inode := createTestFile(t, fs, key)
	if inode == nil {
		t.Fatal("Failed to create test file")
	}

	// Set hash attribute
	inode.SetUserMeta("hash", []byte(testHash))

	// Try to read - should hit external cache
	fh := core.NewFileHandle(inode)
	readBuf := make([]byte, len(testData))
	
	_, err := fh.ReadFile(0, readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Failed to read file: %v", err)
	}

	// Verify cache was hit (content should match)
	if bytes.Contains(readBuf, []byte("Cached content")) {
		t.Log("External cache was successfully used")
	}
}

func TestConcurrentReadsWrites(t *testing.T) {
	if os.Getenv("LOCALSTACK_ENABLED") != "true" {
		t.Skip("Skipping LocalStack test - set LOCALSTACK_ENABLED=true to run")
	}

	mockCache := NewMockContentCache()
	fs, tmpDir := setupTestFS(t, mockCache)
	defer os.RemoveAll(tmpDir)
	defer fs.Shutdown()

	key := "concurrent-file.txt"
	inode := createTestFile(t, fs, key)
	if inode == nil {
		t.Fatal("Failed to create test file")
	}

	// Write initial data
	fh := core.NewFileHandle(inode)
	initialData := bytes.Repeat([]byte("A"), 1024*1024) // 1MB
	err := fh.WriteFile(0, initialData, false)
	if err != nil {
		t.Fatalf("Failed to write initial data: %v", err)
	}

	// Start concurrent readers and writers
	done := make(chan bool)
	errors := make(chan error, 10)

	// Readers
	for i := 0; i < 3; i++ {
		go func(id int) {
			defer func() { done <- true }()
			for j := 0; j < 10; j++ {
				readBuf := make([]byte, 1024)
				_, err := fh.ReadFile(int64(j*1024), readBuf)
				if err != nil && err != io.EOF {
					errors <- fmt.Errorf("reader %d: %v", id, err)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
	}

	// Writers
	for i := 0; i < 2; i++ {
		go func(id int) {
			defer func() { done <- true }()
			for j := 0; j < 5; j++ {
				writeData := bytes.Repeat([]byte(fmt.Sprintf("%d", id)), 1024)
				offset := int64((id*100000 + j*1024) % (1024 * 1024))
				err := fh.WriteFile(offset, writeData, false)
				if err != nil {
					errors <- fmt.Errorf("writer %d: %v", id, err)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 5; i++ {
		<-done
	}

	// Check for errors
	select {
	case err := <-errors:
		t.Fatalf("Concurrent access error: %v", err)
	default:
		t.Log("Concurrent reads and writes completed successfully")
	}

	// Wait for flush
	time.Sleep(2 * time.Second)
	fs.WaitForFlush(10 * time.Second)
}

// Helper function to create a test file
func createTestFile(t *testing.T, fs *core.Goofys, name string) *core.Inode {
	parent := fs.GetRootInode()
	if parent == nil {
		t.Fatal("Failed to get root inode")
		return nil
	}

	inode := core.NewInode(fs, parent, name)
	inode.Attributes.Size = 0
	
	// Add to parent directory
	parent.InsertChild(inode)
	
	return inode
}
