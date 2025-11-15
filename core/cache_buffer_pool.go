package core

import "sync"

// CacheBufferPool manages reusable buffers for external cache reads.
// Reduces GC pressure by pooling commonly-used buffer sizes.
type CacheBufferPool struct {
	pool64KB  sync.Pool
	pool256KB sync.Pool
	pool1MB   sync.Pool
	pool4MB   sync.Pool
}

func NewCacheBufferPool() *CacheBufferPool {
	return &CacheBufferPool{
		pool64KB:  sync.Pool{New: func() interface{} { buf := make([]byte, 64*1024); return &buf }},
		pool256KB: sync.Pool{New: func() interface{} { buf := make([]byte, 256*1024); return &buf }},
		pool1MB:   sync.Pool{New: func() interface{} { buf := make([]byte, 1024*1024); return &buf }},
		pool4MB:   sync.Pool{New: func() interface{} { buf := make([]byte, 4*1024*1024); return &buf }},
	}
}

// Get returns a buffer of at least the requested size.
func (p *CacheBufferPool) Get(size int) []byte {
	var pool *sync.Pool
	switch {
	case size <= 64*1024:
		pool = &p.pool64KB
	case size <= 256*1024:
		pool = &p.pool256KB
	case size <= 1024*1024:
		pool = &p.pool1MB
	case size <= 4*1024*1024:
		pool = &p.pool4MB
	default:
		return make([]byte, size) // Allocate directly for oversized requests
	}
	
	bufPtr := pool.Get().(*[]byte)
	return (*bufPtr)[:size]
}

// Put returns a buffer to the appropriate pool.
func (p *CacheBufferPool) Put(buf []byte) {
	bufPtr := &buf
	switch cap(buf) {
	case 64 * 1024:
		p.pool64KB.Put(bufPtr)
	case 256 * 1024:
		p.pool256KB.Put(bufPtr)
	case 1024 * 1024:
		p.pool1MB.Put(bufPtr)
	case 4 * 1024 * 1024:
		p.pool4MB.Put(bufPtr)
	// Don't pool unusual sizes - let GC handle them
	}
}

// GetOptimalChunkSize returns the optimal chunk size for cache reads.
// Returns 256KB for large files based on performance benchmarks.
func GetOptimalChunkSize(totalSize uint64) uint64 {
	const optimalChunkSize = 256 * 1024
	if totalSize <= optimalChunkSize {
		return totalSize
	}
	return optimalChunkSize
}
