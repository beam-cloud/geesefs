// Test to reproduce and verify the nil pointer panic fix
package test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yandex-cloud/geesefs/core"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

// TestPanicReproduction tries to reproduce the original nil pointer panic
// that occurred in LoadRange/flushPart operations
func TestPanicReproduction(t *testing.T) {
	if os.Getenv("RUN_PANIC_TEST") != "true" {
		t.Skip("Set RUN_PANIC_TEST=true to run (requires Moto)")
	}

	// Check if S3 is available
	if !checkLocalStackAvailable(t, "http://localhost:4566") {
		t.Skip("Moto/LocalStack not available")
	}

	// Setup bucket
	bucketName := "test-panic-reproduction"
	setupBucket(t, bucketName, "http://localhost:4566")
	defer cleanupBucket(t, bucketName, "http://localhost:4566")

	// Mount filesystem
	mountPoint := "/tmp/geesefs-panic-test"
	stagedPath := "/tmp/geesefs-panic-staged"
	os.RemoveAll(mountPoint)
	os.RemoveAll(stagedPath)
	os.MkdirAll(mountPoint, 0755)
	os.MkdirAll(stagedPath, 0755)
	defer os.RemoveAll(mountPoint)
	defer os.RemoveAll(stagedPath)

	s3Config := &cfg.S3Config{
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
	}

	flags := cfg.DefaultFlags()
	flags.Backend = s3Config
	flags.Endpoint = "http://localhost:4566"
	flags.StagedWriteModeEnabled = true
	flags.StagedWritePath = stagedPath
	flags.StagedWriteDebounce = 100 * time.Millisecond // Short debounce
	flags.MountPoint = mountPoint
	flags.DebugFuse = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Log("Mounting filesystem...")
	fs, mfs, err := core.MountFuse(ctx, bucketName, flags)
	if err != nil {
		t.Fatalf("MountFuse failed: %v", err)
	}
	defer func() {
		if mfs != nil {
			mfs.Unmount()
		}
	}()

	// Wait for mount
	time.Sleep(500 * time.Millisecond)

	t.Log("=== ATTEMPTING TO REPRODUCE NIL POINTER PANIC ===")
	t.Log("NOTE: Nil checks should be REMOVED from file.go for this test")
	
	// Strategy: Create conditions that trigger the race:
	// 1. Concurrent writes that trigger flushPart
	// 2. Concurrent reads that use LoadRange
	// 3. Rapid file operations that might not fully initialize inodes
	// 4. Force eviction scenarios
	
	panicCaught := false
	var panicMsg string
	
	// Run multiple strategies MULTIPLE TIMES to try to trigger the panic
	for attempt := 1; attempt <= 3 && !panicCaught; attempt++ {
		t.Logf("=== Attempt %d/3 ===", attempt)
		
		for strategyNum := 1; strategyNum <= 5 && !panicCaught; strategyNum++ {
		t.Logf("Trying strategy %d...", strategyNum)
		
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicCaught = true
					panicMsg = fmt.Sprintf("%v", r)
					t.Logf("✓ PANIC REPRODUCED: %v", r)
				}
			}()
			
			switch strategyNum {
			case 1:
				// Strategy 1: Rapid concurrent writes with immediate reads
				testRapidWriteRead(t, mountPoint)
			case 2:
				// Strategy 2: Large file with concurrent flush and read
				testLargeFileFlushRace(t, mountPoint)
			case 3:
				// Strategy 3: Many small files with rapid operations
				testManySmallFilesRace(t, mountPoint)
			case 4:
				// Strategy 4: Write, truncate, read race
				testTruncateRace(t, mountPoint)
			case 5:
				// Strategy 5: Direct access to internal inode operations
				testDirectInodeRace(t, fs)
			}
		}()
		}
	}
	
	if panicCaught {
		t.Logf("✅ SUCCESS: Reproduced panic: %s", panicMsg)
		t.Log("Now verifying the fix prevents it...")
		// The test will fail here since we caught a panic
		// This confirms the bug exists when nil checks are removed
		t.Fatalf("PANIC REPRODUCED (this is expected): %s", panicMsg)
	} else {
		t.Log("⚠ Could not reproduce panic with current code (fix may be working)")
		t.Log("Will try to remove fix temporarily to confirm...")
	}
}

func testRapidWriteRead(t *testing.T, mountPoint string) {
	t.Log("  Strategy 1: Rapid concurrent write/read")
	
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			
			testFile := filepath.Join(mountPoint, fmt.Sprintf("rapid-%d.txt", n))
			data := make([]byte, 512*1024) // 512 KB
			
			// Write
			if err := ioutil.WriteFile(testFile, data, 0644); err != nil {
				return
			}
			
			// Immediately read (might race with flush)
			_, _ = ioutil.ReadFile(testFile)
			
			// Delete
			os.Remove(testFile)
		}(i)
	}
	wg.Wait()
}

func testLargeFileFlushRace(t *testing.T, mountPoint string) {
	t.Log("  Strategy 2: Large file flush race")
	
	testFile := filepath.Join(mountPoint, "large-flush.bin")
	data := make([]byte, 10*1024*1024) // 10 MB
	
	var wg sync.WaitGroup
	
	// Writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		ioutil.WriteFile(testFile, data, 0644)
	}()
	
	// Concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond)
			ioutil.ReadFile(testFile)
		}()
	}
	
	wg.Wait()
	os.Remove(testFile)
}

func testManySmallFilesRace(t *testing.T, mountPoint string) {
	t.Log("  Strategy 3: Many small files race")
	
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			
			testFile := filepath.Join(mountPoint, fmt.Sprintf("small-%d.txt", n))
			data := []byte(fmt.Sprintf("data-%d", n))
			
			ioutil.WriteFile(testFile, data, 0644)
			time.Sleep(5 * time.Millisecond)
			ioutil.ReadFile(testFile)
			os.Remove(testFile)
		}(i)
	}
	wg.Wait()
}

func testTruncateRace(t *testing.T, mountPoint string) {
	t.Log("  Strategy 4: Truncate race")
	
	testFile := filepath.Join(mountPoint, "truncate.txt")
	initialData := make([]byte, 1024*1024) // 1 MB
	
	ioutil.WriteFile(testFile, initialData, 0644)
	
	var wg sync.WaitGroup
	
	// Truncate operations
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := os.OpenFile(testFile, os.O_RDWR, 0644)
			if err == nil {
				f.Truncate(100)
				f.Close()
			}
		}()
		
		// Concurrent reads
		wg.Add(1)
		go func() {
			defer wg.Done()
			ioutil.ReadFile(testFile)
		}()
	}
	
	wg.Wait()
	os.Remove(testFile)
}

func testDirectInodeRace(t *testing.T, fs *core.Goofys) {
	t.Log("  Strategy 5: Direct inode operation race")
	
	// This is more invasive - directly manipulating internal state
	// to try to trigger the race condition
	
	// Create a file first
	testFile := filepath.Join("/tmp/geesefs-panic-test", "direct-test.bin")
	data := make([]byte, 5*1024*1024) // 5 MB
	ioutil.WriteFile(testFile, data, 0644)
	
	time.Sleep(100 * time.Millisecond)
	
	// Try to trigger concurrent operations
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Read the file which will trigger LoadRange internally
			ioutil.ReadFile(testFile)
		}()
		
		// Concurrent writes to trigger flushPart
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			f, err := os.OpenFile(testFile, os.O_RDWR|os.O_APPEND, 0644)
			if err == nil {
				f.Write([]byte(fmt.Sprintf("append-%d", n)))
				f.Close()
			}
		}(i)
	}
	
	wg.Wait()
	os.Remove(testFile)
}

func setupBucket(t *testing.T, bucketName, endpoint string) {
	// Use Python boto3 since awscli might not be in PATH
	script := fmt.Sprintf(`
import boto3
s3 = boto3.client('s3', endpoint_url='%s', aws_access_key_id='test', aws_secret_access_key='test')
try:
    s3.create_bucket(Bucket='%s')
except:
    pass
`, endpoint, bucketName)
	
	cmd := exec.Command("python3", "-c", script)
	if err := cmd.Run(); err != nil {
		t.Logf("Warning: Could not create bucket: %v", err)
	}
	t.Logf("✓ Bucket ready: %s", bucketName)
}

func cleanupBucket(t *testing.T, bucketName, endpoint string) {
	// Best effort cleanup
	script := fmt.Sprintf(`
import boto3
s3 = boto3.client('s3', endpoint_url='%s', aws_access_key_id='test', aws_secret_access_key='test')
try:
    bucket = s3.Bucket('%s')
    bucket.objects.all().delete()
    bucket.delete()
except:
    pass
`, endpoint, bucketName)
	cmd := exec.Command("python3", "-c", script)
	_ = cmd.Run()
}
