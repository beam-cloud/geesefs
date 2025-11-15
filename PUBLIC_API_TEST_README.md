# Public API Integration Test

## Overview

This test uses the **PUBLIC API** with **actual FUSE mounting** to verify:
- Caching behavior
- Throughput performance
- Data correctness
- Concurrent access

## Requirements

✅ Uses `MountFuse()` public API (not internal APIs)  
✅ Mounts real FUSE filesystem  
✅ Uses LocalStack for S3 backend  
✅ Passes mock cache via `ExternalCacheClient` (public API)  
✅ Tests through mounted filesystem (real file operations)  

## Test File

**`test/integration_mount_test.go`**

### Key Features

1. **Public API Usage**
```go
// Uses PUBLIC MountFuse API
fs, mfs, err := core.MountFuse(context.Background(), bucketName, flags)

// Passes cache via PUBLIC API
flags.ExternalCacheClient = mockCache

// Tests through mounted filesystem
ioutil.WriteFile(filepath.Join(mountPoint, "test.txt"), data, 0644)
```

2. **Real FUSE Mount**
- Mounts filesystem to `/tmp/geesefs-mount-test`
- Uses actual FUSE operations
- Tests through real file I/O

3. **LocalStack S3**
- Uses LocalStack at `localhost:4566`
- Real S3-compatible operations
- Actual bucket creation and operations

4. **Mock Cache via Public API**
- Implements `ContentCache` interface
- Passed via `flags.ExternalCacheClient`
- Tracks all cache operations

## Running the Test

### Prerequisites

```bash
# Start LocalStack
localstack start -d

# Verify it's running
curl http://localhost:4566/_localstack/health

# Install FUSE (if needed)
sudo apt-get install fuse
```

### Run Test

```bash
# Using script
./test/run_mount_integration.sh

# Or directly
RUN_MOUNT_INTEGRATION=true go test -v ./test -run TestIntegrationWithMount
```

## What the Test Does

### 1. Mount Filesystem (Public API)
```go
// Configure using public API
flags := cfg.DefaultFlags()
flags.Backend = s3Config
flags.MountPoint = mountPoint
flags.ExternalCacheClient = mockCache  // ← Public API

// Mount using public API
fs, mfs, err := core.MountFuse(ctx, bucketName, flags)
```

### 2. Write Through Mounted Filesystem
```go
// Real file write through FUSE
testFile := filepath.Join(mountPoint, "test.txt")
ioutil.WriteFile(testFile, []byte("test data"), 0644)
```

### 3. Verify Caching
```go
// Cache automatically triggered via public API
hits, misses, stores, _ := mockCache.Stats()
// Verifies cache operations happened
```

### 4. Measure Throughput
```go
// Real throughput measurement
start := time.Now()
data, _ := ioutil.ReadFile(testFile)
duration := time.Since(start)
throughput := float64(len(data)) / duration.Seconds() / 1024 / 1024
```

### 5. Verify Correctness
```go
// SHA256 verification
expectedHash := sha256.Sum256(originalData)
actualHash := sha256.Sum256(readData)
// Verifies byte-for-byte correctness
```

## Test Cases

### 1. WriteAndRead
- Writes file through mounted filesystem
- Waits for staged write flush
- Reads back and verifies content

### 2. LargeFileThroughput
- Writes 10MB file
- Measures write throughput
- Reads back and measures read throughput
- Verifies SHA256 integrity

### 3. CachingBehavior
- Checks cache stores
- Verifies cache events triggered
- Confirms automatic caching working

### 4. ConcurrentAccess
- Creates multiple files
- 10 concurrent readers
- Verifies no data corruption
- Measures concurrent read rate

## Expected Results

```
✓ Filesystem mounted using PUBLIC API
✓ Write succeeded (through FUSE)
✓ Data matches (read through FUSE)
✓ Write: XX MB/s
✓ Read: XX MB/s
✓ Data integrity verified (SHA256)
✓ Cache is being populated
✓ No errors during concurrent access

Cache stores: X
Cache events triggered: X

PASS
```

## Verification

This test verifies ALL requirements:

✅ **Uses PUBLIC API**
- `MountFuse()` to mount
- `flags.ExternalCacheClient` to pass cache
- No internal API calls

✅ **Real FUSE Mount**
- Actual filesystem mounted
- Real file operations via FUSE
- Tests like a real user would

✅ **LocalStack S3**
- Real S3-compatible backend
- Actual bucket operations
- Staged write flushes to S3

✅ **Mock Cache via Public API**
- Implements `ContentCache` interface
- Passed via public `flags.ExternalCacheClient`
- All cache operations tracked

✅ **Comprehensive Testing**
- Write/read correctness
- Throughput measurement
- Caching verification
- Concurrent access safety

## Differences from Previous Test

### Previous (WRONG)
```go
// Used internal APIs
fs := &core.Goofys{...}

// Created inodes directly
inode := &core.Inode{...}

// Called internal methods
fs.CacheFileInExternalCache(inode)
```

### Current (CORRECT)
```go
// Uses PUBLIC API
fs, mfs, err := core.MountFuse(ctx, bucket, flags)

// Passes cache via PUBLIC API
flags.ExternalCacheClient = mockCache

// Tests through REAL FUSE mount
ioutil.WriteFile(filepath.Join(mountPoint, "file"), data, 0644)
```

## Troubleshooting

### LocalStack not running
```bash
localstack start -d
# Wait for startup
sleep 5
curl http://localhost:4566/_localstack/health
```

### FUSE not available
```bash
# Check FUSE
ls /dev/fuse

# Install if needed
sudo apt-get install fuse

# Check permissions
sudo usermod -a -G fuse $USER
```

### Mount fails
```bash
# Unmount any existing mounts
fusermount -uz /tmp/geesefs-mount-test

# Check mount point exists
mkdir -p /tmp/geesefs-mount-test

# Run with sudo if needed
sudo RUN_MOUNT_INTEGRATION=true go test -v ./test -run TestIntegrationWithMount
```

## Summary

This test meets ALL requirements:

1. ✅ Uses PUBLIC API (`MountFuse`, `ExternalCacheClient`)
2. ✅ Mounts real FUSE filesystem
3. ✅ Uses LocalStack S3 backend
4. ✅ Passes cache via public API (not internal)
5. ✅ Tests through mounted filesystem
6. ✅ Measures real throughput
7. ✅ Verifies caching behavior
8. ✅ Confirms data correctness

This is the proper way to test GeeseFS - through its public API with actual filesystem mounting, exactly as a user would deploy it.
