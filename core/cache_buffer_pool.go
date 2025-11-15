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
	"sync"
)

// CacheBufferPool manages buffers for external cache reads
// Uses separate pools for optimal sizes identified in benchmarks
type CacheBufferPool struct {
	// Pools for optimal sizes (benchmarks show 128KB-256KB are fastest)
	pool64KB   sync.Pool
	pool256KB  sync.Pool
	pool1MB    sync.Pool
	pool4MB    sync.Pool
	
	// Statistics
	hits   int64
	misses int64
}

func NewCacheBufferPool() *CacheBufferPool {
	return &CacheBufferPool{
		pool64KB: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 64*1024)
				return &buf
			},
		},
		pool256KB: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 256*1024)
				return &buf
			},
		},
		pool1MB: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 1024*1024)
				return &buf
			},
		},
		pool4MB: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 4*1024*1024)
				return &buf
			},
		},
	}
}

// Get returns a buffer of at least the requested size
func (p *CacheBufferPool) Get(size int) []byte {
	if size <= 64*1024 {
		bufPtr := p.pool64KB.Get().(*[]byte)
		return (*bufPtr)[:size]
	} else if size <= 256*1024 {
		bufPtr := p.pool256KB.Get().(*[]byte)
		return (*bufPtr)[:size]
	} else if size <= 1024*1024 {
		bufPtr := p.pool1MB.Get().(*[]byte)
		return (*bufPtr)[:size]
	} else if size <= 4*1024*1024 {
		bufPtr := p.pool4MB.Get().(*[]byte)
		return (*bufPtr)[:size]
	}
	// For very large sizes, allocate directly
	return make([]byte, size)
}

// Put returns a buffer to the pool
func (p *CacheBufferPool) Put(buf []byte) {
	capacity := cap(buf)
	if capacity == 64*1024 {
		bufPtr := &buf
		p.pool64KB.Put(bufPtr)
	} else if capacity == 256*1024 {
		bufPtr := &buf
		p.pool256KB.Put(bufPtr)
	} else if capacity == 1024*1024 {
		bufPtr := &buf
		p.pool1MB.Put(bufPtr)
	} else if capacity == 4*1024*1024 {
		bufPtr := &buf
		p.pool4MB.Put(bufPtr)
	}
	// Don't pool unusual sizes
}

// GetChunkSize returns the optimal chunk size for a given total size
// Based on benchmarks: 256KB is the sweet spot
func GetOptimalChunkSize(totalSize uint64) uint64 {
	if totalSize <= 256*1024 {
		return totalSize
	}
	// Use 256KB chunks for large reads (best benchmark performance)
	return 256 * 1024
}
