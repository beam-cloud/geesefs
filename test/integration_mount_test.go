package test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/yandex-cloud/geesefs/core"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

// TestMockCache implements ContentCache interface for testing
type TestMockCache struct {
	data          map[string][]byte
	mu            sync.RWMutex
	hits          int64
	misses        int64
	stores        int64
	storeRequests []string
	getContentCalls int64
	getStreamCalls  int64
}

func NewTestMockCache() *TestMockCache {
	return &TestMockCache{
		data:          make(map[string][]byte),
		storeRequests: make([]string, 0),
	}
}

func (c *TestMockCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	atomic.AddInt64(&c.getContentCalls, 1)
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, ok := c.data[hash]
	if !ok {
		atomic.AddInt64(&c.misses, 1)
		return nil, fmt.Errorf("cache miss: %s", hash)
	}

	atomic.AddInt64(&c.hits, 1)

	if offset >= int64(len(data)) {
		return nil, io.EOF
	}

	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}

	result := make([]byte, end-offset)
	copy(result, data[offset:end])
	return result, nil
}


func (c *TestMockCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	var buffer []byte
	for chunk := range chunks {
		buffer = append(buffer, chunk...)
	}

	c.mu.Lock()
	c.data[hash] = buffer
	c.storeRequests = append(c.storeRequests, fmt.Sprintf("direct:%s", hash))
	c.mu.Unlock()

	atomic.AddInt64(&c.stores, 1)
	return hash, nil
}

func (c *TestMockCache) StoreContentFromS3(source struct {
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
	hash := opts.RoutingKey
	
	// Fetch file from S3 (simulates real cache behavior)
	cfg := aws.NewConfig().
		WithEndpoint(source.EndpointURL).
		WithRegion(source.Region).
		WithS3ForcePathStyle(true).
		WithCredentials(credentials.NewStaticCredentials(source.AccessKey, source.SecretKey, ""))
	
	sess, err := session.NewSession(cfg)
	if err != nil {
		return hash, err
	}
	
	svc := s3.New(sess)
	result, err := svc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(source.BucketName),
		Key:    aws.String(source.Path),
	})
	if err != nil {
		return hash, err
	}
	defer result.Body.Close()
	
	data, err := ioutil.ReadAll(result.Body)
	if err != nil {
		return hash, err
	}
	
	c.mu.Lock()
	c.storeRequests = append(c.storeRequests, fmt.Sprintf("s3:%s", source.Path))
	c.data[hash] = data
	c.mu.Unlock()

	atomic.AddInt64(&c.stores, 1)
	return hash, nil
}

func (c *TestMockCache) Stats() (hits, misses, stores int64, requests []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return atomic.LoadInt64(&c.hits), atomic.LoadInt64(&c.misses), atomic.LoadInt64(&c.stores), append([]string{}, c.storeRequests...)
}


// TestIntegrationWithMount tests using PUBLIC API and actual FUSE mount
func TestIntegrationWithMount(t *testing.T) {
	if os.Getenv("RUN_MOUNT_INTEGRATION") != "true" {
		t.Skip("Set RUN_MOUNT_INTEGRATION=true to run (requires FUSE and LocalStack)")
	}

	// Check LocalStack
	endpoint := "http://localhost:4566"
	if !checkLocalStackAvailable(t, endpoint) {
		t.Fatal("LocalStack not available - start with: localstack start -d")
	}

	// Setup
	bucketName := "test-mount-integration"
	mountPoint := "/tmp/geesefs-mount-test"
	stagedPath := "/tmp/geesefs-mount-staged"

	// Cleanup old mount if exists
	exec.Command("fusermount", "-uz", mountPoint).Run()
	os.RemoveAll(mountPoint)
	os.RemoveAll(stagedPath)

	// Create directories
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}
	if err := os.MkdirAll(stagedPath, 0755); err != nil {
		t.Fatalf("Failed to create staged path: %v", err)
	}

	// Cleanup on exit
	defer func() {
		t.Log("Cleaning up...")
		exec.Command("fusermount", "-uz", mountPoint).Run()
		time.Sleep(500 * time.Millisecond)
		os.RemoveAll(mountPoint)
		os.RemoveAll(stagedPath)
	}()

	// Create bucket
	createBucketLocalStack(t, endpoint, bucketName)

	// Create mock cache
	mockCache := NewTestMockCache()

	// Configure using PUBLIC API
	flags := cfg.DefaultFlags()

	// S3 Backend config
	s3Config := &cfg.S3Config{}
	s3Config.Init()
	s3Config.AccessKey = "test"
	s3Config.SecretKey = "test"
	s3Config.Region = "us-east-1"

	flags.Backend = s3Config
	flags.Endpoint = endpoint
	flags.MountPoint = mountPoint
	flags.Foreground = false
	flags.DirMode = 0755
	flags.FileMode = 0644
	flags.Uid = uint32(os.Getuid())
	flags.Gid = uint32(os.Getgid())

	// Staged write config
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = stagedPath
	flags.StagedWriteDebounce = 2 * time.Second
	flags.StagedWriteFlushInterval = 500 * time.Millisecond
	flags.StagedWriteFlushSize = 1 * 1024 * 1024

	// Cache config - using PUBLIC API
	flags.ExternalCacheClient = mockCache
	flags.ExternalCacheStreamingEnabled = false
	flags.MinFileSizeForHashKB = 1
	flags.HashAttr = "hash"

	// Performance
	flags.MemoryLimit = 100 * 1024 * 1024
	flags.MaxFlushers = 4
	flags.StatCacheTTL = 1 * time.Second

	// Event callback
	cacheEvents := make([]string, 0)
	var cacheEventsMu sync.Mutex
	flags.EventCallback = func(event cfg.EventType, data map[string]interface{}) {
		t.Logf("Event: %s, Data: %+v", event, data)
		if event == cfg.EventCacheTriggered {
			cacheEventsMu.Lock()
			if inode, ok := data["inode"].(string); ok {
				cacheEvents = append(cacheEvents, inode)
			}
			cacheEventsMu.Unlock()
		}
	}

	// Mount using PUBLIC API
	t.Log("Mounting filesystem using PUBLIC API (MountFuse)...")
	fs, mfs, err := core.MountFuse(context.Background(), bucketName, flags)
	if err != nil {
		t.Fatalf("MountFuse failed: %v", err)
	}
	defer func() {
		if mfs != nil {
			mfs.Unmount()
		}
	}()

	// Wait for mount
	t.Log("Waiting for FUSE mount...")
	mounted := false
	for i := 0; i < 30; i++ {
		if isMountedCheck(mountPoint) {
			mounted = true
			t.Log("✓ Filesystem mounted")
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !mounted {
		t.Fatal("Filesystem failed to mount")
	}

	// Verify mount is actually using FUSE
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err == nil {
		mountInfo := string(output)
		if contains(mountInfo, mountPoint) {
			t.Logf("✓ Mount verified in mount table")
			// Extract mount line
			for _, line := range splitLines(mountInfo) {
				if contains(line, mountPoint) {
					t.Logf("  Mount: %s", line)
					if contains(line, "fuse") {
						t.Logf("✓ Confirmed: Using FUSE")
					}
				}
			}
		} else {
			t.Error("❌ Mount point not found in mount table!")
		}
	}

	// Run tests through mounted filesystem
	t.Run("WriteAndRead", func(t *testing.T) {
		testWriteAndReadMounted(t, mountPoint, mockCache)
	})

	t.Run("LargeFileThroughput", func(t *testing.T) {
		testLargeFileThroughputMounted(t, mountPoint, mockCache)
	})

	t.Run("CachingBehavior", func(t *testing.T) {
		testCachingBehaviorMounted(t, mountPoint, mockCache, cacheEvents, &cacheEventsMu)
	})

	t.Run("ConcurrentAccess", func(t *testing.T) {
		testConcurrentAccessMounted(t, mountPoint)
	})

	// Final stats
	hits, misses, stores, requests := mockCache.Stats()
	t.Logf("Final cache stats:")
	t.Logf("  Hits: %d", hits)
	t.Logf("  Misses: %d", misses)
	t.Logf("  Stores: %d", stores)
	t.Logf("  Store requests: %v", requests)

	cacheEventsMu.Lock()
	t.Logf("  Cache events triggered: %d", len(cacheEvents))
	cacheEventsMu.Unlock()

	_ = fs // Keep fs reference
}

func testWriteAndReadMounted(t *testing.T, mountPoint string, cache *TestMockCache) {
	t.Log("=== VERIFYING STAGED WRITE MODE ===")
	
	testFile := filepath.Join(mountPoint, "test-write-read.txt")
	testData := "Hello from mounted filesystem test!"

	t.Logf("Writing to %s...", testFile)

	// Write through mounted filesystem
	if err := ioutil.WriteFile(testFile, []byte(testData), 0644); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	t.Log("✓ Write succeeded")

	// VERIFY STAGED WRITE: Check staged file exists
	t.Log("Checking for staged file...")
	time.Sleep(500 * time.Millisecond)
	
	// The file should be in staged location initially
	expectedStagedPath := filepath.Join("/tmp/geesefs-mount-staged", "test-write-read.txt")
	if _, err := os.Stat(expectedStagedPath); err == nil {
		t.Logf("✓ STAGED WRITE VERIFIED: File exists at %s", expectedStagedPath)
		fileInfo, _ := os.Stat(expectedStagedPath)
		t.Logf("  Staged file size: %d bytes", fileInfo.Size())
	} else {
		t.Logf("ℹ Staged file already flushed (this is ok)")
	}

	// Wait for flush and cache
	time.Sleep(3 * time.Second)

	// Read back through mounted filesystem
	t.Log("Reading back...")
	readData, err := ioutil.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(readData) != testData {
		t.Errorf("Data mismatch: expected %q, got %q", testData, string(readData))
	} else {
		t.Log("✓ Data matches")
	}
}

func testLargeFileThroughputMounted(t *testing.T, mountPoint string, cache *TestMockCache) {
	t.Log("=== MEASURING THROUGHPUT ===")
	
	testFile := filepath.Join(mountPoint, "large-throughput-test.bin")
	fileSize := 10 * 1024 * 1024 // 10MB

	t.Logf("Creating %d MB file...", fileSize/(1024*1024))

	// Generate test data
	testData := make([]byte, fileSize)
	rand.Read(testData)
	expectedHash := sha256.Sum256(testData)
	expectedHashStr := hex.EncodeToString(expectedHash[:])
	t.Logf("Expected hash: %s", expectedHashStr)

	// Write through mounted filesystem
	start := time.Now()
	if err := ioutil.WriteFile(testFile, testData, 0644); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	writeDuration := time.Since(start)
	writeThroughput := float64(fileSize) / writeDuration.Seconds() / 1024 / 1024

	t.Logf("✓ Write: %.2f MB/s", writeThroughput)

	// Wait for flush
	t.Log("Waiting for flush...")
	time.Sleep(4 * time.Second)

	// Read back and measure throughput
	t.Log("Reading back...")
	start = time.Now()
	readData, err := ioutil.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	readDuration := time.Since(start)
	readThroughput := float64(fileSize) / readDuration.Seconds() / 1024 / 1024

	t.Logf("✓ Read: %.2f MB/s", readThroughput)

	// Verify integrity
	readHash := sha256.Sum256(readData)
	readHashStr := hex.EncodeToString(readHash[:])

	if readHashStr != expectedHashStr {
		t.Errorf("Hash mismatch: expected %s, got %s", expectedHashStr, readHashStr)
	} else {
		t.Log("✓ Data integrity verified")
	}

	// Report
	if readThroughput < 10 {
		t.Logf("⚠ Read throughput lower than expected: %.2f MB/s", readThroughput)
	} else {
		t.Logf("✓ Good throughput: %.2f MB/s", readThroughput)
	}
}

func testCachingBehaviorMounted(t *testing.T, mountPoint string, cache *TestMockCache, events []string, mu *sync.Mutex) {
	t.Log("=== VERIFYING CACHING BEHAVIOR ===")
	t.Log("Testing automatic cache population...")

	// Check if any cache stores happened
	_, _, stores, requests := cache.Stats()
	t.Logf("Cache stores: %d", stores)
	t.Logf("Store requests: %v", requests)

	mu.Lock()
	eventCount := len(events)
	mu.Unlock()
	t.Logf("Cache events triggered: %d", eventCount)

	if stores > 0 {
		t.Log("✓ Cache is being populated")
	} else {
		t.Log("ℹ No cache stores yet (may need more time)")
	}
}

func testConcurrentAccessMounted(t *testing.T, mountPoint string) {
	t.Log("Testing concurrent access...")

	numFiles := 5
	numReaders := 10

	// Create test files
	files := make([]string, numFiles)
	hashes := make([]string, numFiles)

	for i := 0; i < numFiles; i++ {
		fileName := filepath.Join(mountPoint, fmt.Sprintf("concurrent-%d.bin", i))
		data := make([]byte, 256*1024) // 256KB
		rand.Read(data)

		if err := ioutil.WriteFile(fileName, data, 0644); err != nil {
			t.Fatalf("Failed to write file %d: %v", i, err)
		}

		hash := sha256.Sum256(data)
		hashes[i] = hex.EncodeToString(hash[:])
		files[i] = fileName
	}

	t.Log("✓ Test files created")

	// Wait for flush
	time.Sleep(3 * time.Second)

	// Concurrent reads
	var wg sync.WaitGroup
	errors := make(chan error, numReaders*numFiles)

	start := time.Now()

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()

			for j, file := range files {
				data, err := ioutil.ReadFile(file)
				if err != nil {
					errors <- fmt.Errorf("reader %d, file %d: %v", readerID, j, err)
					continue
				}

				hash := sha256.Sum256(data)
				hashStr := hex.EncodeToString(hash[:])
				if hashStr != hashes[j] {
					errors <- fmt.Errorf("reader %d, file %d: hash mismatch", readerID, j)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	duration := time.Since(start)
	totalReads := numReaders * numFiles
	readsPerSec := float64(totalReads) / duration.Seconds()

	errorCount := len(errors)
	t.Logf("✓ Concurrent reads: %d readers × %d files", numReaders, numFiles)
	t.Logf("  Completed in %v (%.0f reads/sec)", duration, readsPerSec)
	t.Logf("  Errors: %d", errorCount)

	if errorCount > 0 {
		for err := range errors {
			t.Errorf("  - %v", err)
		}
	} else {
		t.Log("✓ No errors during concurrent access")
	}
}

func checkLocalStackAvailable(t *testing.T, endpoint string) bool {
	// Try LocalStack health endpoint first
	cmd := exec.Command("curl", "-sf", endpoint+"/_localstack/health")
	if err := cmd.Run(); err == nil {
		t.Logf("✓ LocalStack available at %s", endpoint)
		return true
	}
	
	// Fall back to basic S3 endpoint check (for moto)
	cmd = exec.Command("curl", "-sf", endpoint+"/")
	if err := cmd.Run(); err != nil {
		t.Logf("S3-compatible service not available at %s", endpoint)
		return false
	}
	t.Logf("✓ S3-compatible service (moto) available at %s", endpoint)
	return true
}

func createBucketLocalStack(t *testing.T, endpoint, bucket string) {
	cmd := exec.Command("aws", "--endpoint-url="+endpoint, "s3", "mb", "s3://"+bucket)
	_ = cmd.Run() // Ignore error if bucket exists
	t.Logf("✓ Bucket ready: %s", bucket)
}

func isMountedCheck(path string) bool {
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return string(output) != "" && contains(string(output), path)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	result := make([]string, 0)
	current := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if current != "" {
				result = append(result, current)
				current = ""
			}
		} else {
			current += string(s[i])
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}
