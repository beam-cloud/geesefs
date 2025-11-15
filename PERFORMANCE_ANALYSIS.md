# Cache Performance Analysis

## Baseline Results

```
BenchmarkCacheReadThroughput/Size_4KB-4          4743.56 MB/s    4096 B/op   1 allocs/op
BenchmarkCacheReadThroughput/Size_64KB-4         5842.67 MB/s   65536 B/op   1 allocs/op
BenchmarkCacheReadThroughput/Size_1024KB-4       2875.88 MB/s 1048577 B/op   1 allocs/op ⚠️
BenchmarkCacheReadThroughput/Size_5120KB-4       4931.54 MB/s 5242885 B/op   1 allocs/op
BenchmarkCacheConcurrentReads/Concurrency_1-4    4742.88 MB/s
BenchmarkCacheConcurrentReads/Concurrency_4-4    4863.56 MB/s
BenchmarkCacheStreamingVsNonStreaming/NonStreaming 3476.79 MB/s  5242881 B/op  1 allocs/op
BenchmarkCacheStreamingVsNonStreaming/Streaming    3109.03 MB/s  5243003 B/op  3 allocs/op ⚠️
```

## Identified Bottlenecks

### 1. Streaming Path Inefficiency (file.go:419-423)
```go
// Current: Inefficient
buf = make([]byte, 0, size)  // Allocate with capacity
for chunk := range contentChan {
    buf = append(buf, chunk...)  // Multiple appends cause copies
}
```
**Impact**: Streaming is 10% slower than non-streaming, uses 3x allocations

### 2. Lock Contention (file.go:441-447)
```go
// Current: Lock held during buffer add + broadcast
inode.mu.Lock()
allocated += inode.buffers.Add(offset, buf, BUF_CLEAN, false)
if inode.readCond != nil {
    inode.readCond.Broadcast()
}
inode.mu.Unlock()
```
**Impact**: Blocks concurrent readers during buffer operations

### 3. 1MB Performance Anomaly
1MB reads are slowest (2875 MB/s vs 5842 MB/s for 64KB)
**Possible causes**: Buffer alignment, memory copying, cache effects

## Optimization Strategy (L7 Approach)

### Principle: Simple > Clever
1. Fix obvious inefficiencies first
2. Eliminate unnecessary work
3. Reduce allocations/copies
4. Minimize lock time

### Optimizations to Implement

#### 1. Fix Streaming Buffer Allocation
```go
// Before: grow by append
buf = make([]byte, 0, size)
for chunk := range contentChan {
    buf = append(buf, chunk...)
}

// After: direct copy into pre-allocated buffer
buf = make([]byte, size)
offset := 0
for chunk := range contentChan {
    n := copy(buf[offset:], chunk)
    offset += n
}
buf = buf[:offset]
```
**Expected**: 10-15% improvement, reduce allocs from 3 to 1

#### 2. Minimize Lock Duration
```go
// Before: lock during buffer add
inode.mu.Lock()
allocated += inode.buffers.Add(...)
if inode.readCond != nil {
    inode.readCond.Broadcast()
}
inode.mu.Unlock()

// After: prepare outside lock
allocated = inode.buffers.Add(...)  // If safe without lock
inode.mu.Lock()
// Only lock for notification
if inode.readCond != nil {
    inode.readCond.Broadcast()
}
inode.mu.Unlock()
```
**Expected**: Better concurrent throughput

#### 3. Default to Non-Streaming
Streaming is slower - only use when truly needed
**Expected**: Immediate 10% gain for most workloads

## Target Goals

- 4KB-64KB: Maintain >5000 MB/s
- 1MB-5MB: Achieve >5000 MB/s (up from 2875-4931)
- Concurrent: Achieve >5000 MB/s with minimal degradation
- Streaming: Match non-streaming performance

## Implementation Priority

1. ✅ Fix streaming buffer allocation (highest impact, simple)
2. ✅ Reduce lock contention (good impact, simple)
3. ✅ Document streaming performance (zero cost)
