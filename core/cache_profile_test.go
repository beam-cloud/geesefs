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
	"fmt"
	"sync"
	"testing"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

// Benchmark different aspects of the cache read path
func BenchmarkCacheReadComponents(b *testing.B) {
	size := 1024 * 1024 // 1MB
	
	b.Run("MockCacheGetContent", func(b *testing.B) {
		cache := NewMockCache()
		testData := make([]byte, size)
		hash := "test-hash"
		cache.storage[hash] = testData
		
		b.ResetTimer()
		b.SetBytes(int64(size))
		
		for i := 0; i < b.N; i++ {
			_, err := cache.GetContent(hash, 0, int64(size), struct{ RoutingKey string }{})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	
	b.Run("BuffersAdd", func(b *testing.B) {
		fs := &Goofys{
			flags: &cfg.FlagStorage{
				MemoryLimit: 100 * 1024 * 1024,
				PartSizes: []cfg.PartSizeConfig{
					{PartSize: 5 * 1024 * 1024, PartCount: 10000},
				},
			},
			bufferPool: NewBufferPool(100*1024*1024, 100*1024*1024),
		}
		
		inode := &Inode{
			fs: fs,
			mu: sync.Mutex{},
			Attributes: InodeAttributes{Size: uint64(size)},
		}
		inode.buffers.helpers = inode
		
		testData := make([]byte, size)
		
		b.ResetTimer()
		b.SetBytes(int64(size))
		
		for i := 0; i < b.N; i++ {
			inode.mu.Lock()
			inode.buffers.Add(0, testData, BUF_CLEAN, false)
			inode.buffers.RemoveRange(0, uint64(size), nil)
			inode.mu.Unlock()
		}
	})
	
	b.Run("BuffersAddWithCopy", func(b *testing.B) {
		fs := &Goofys{
			flags: &cfg.FlagStorage{
				MemoryLimit: 100 * 1024 * 1024,
				PartSizes: []cfg.PartSizeConfig{
					{PartSize: 5 * 1024 * 1024, PartCount: 10000},
				},
			},
			bufferPool: NewBufferPool(100*1024*1024, 100*1024*1024),
		}
		
		inode := &Inode{
			fs: fs,
			mu: sync.Mutex{},
			Attributes: InodeAttributes{Size: uint64(size)},
		}
		inode.buffers.helpers = inode
		
		testData := make([]byte, size)
		
		b.ResetTimer()
		b.SetBytes(int64(size))
		
		for i := 0; i < b.N; i++ {
			inode.mu.Lock()
			inode.buffers.Add(0, testData, BUF_CLEAN, true) // With copy
			inode.buffers.RemoveRange(0, uint64(size), nil)
			inode.mu.Unlock()
		}
	})
	
	b.Run("FullPath_NoCopy", func(b *testing.B) {
		cache := NewMockCache()
		testData := make([]byte, size)
		hash := "test-hash"
		cache.storage[hash] = testData
		
		fs := &Goofys{
			flags: &cfg.FlagStorage{
				ExternalCacheClient:           cache,
				ExternalCacheStreamingEnabled: false,
				HashAttr:                      "hash",
				MemoryLimit:                   100 * 1024 * 1024,
				PartSizes: []cfg.PartSizeConfig{
					{PartSize: 5 * 1024 * 1024, PartCount: 10000},
				},
			},
			bufferPool: NewBufferPool(100*1024*1024, 100*1024*1024),
		}
		
		inode := &Inode{
			fs: fs,
			mu: sync.Mutex{},
			userMetadata: map[string][]byte{"hash": []byte(hash)},
			Attributes:   InodeAttributes{Size: uint64(size)},
		}
		inode.buffers.helpers = inode
		inode.readCond = sync.NewCond(&inode.mu)
		
		b.ResetTimer()
		b.SetBytes(int64(size))
		
		for i := 0; i < b.N; i++ {
			_, _, err := inode.loadFromExternalCache(0, uint64(size), hash)
			if err != nil {
				b.Fatal(err)
			}
			inode.mu.Lock()
			inode.buffers.RemoveRange(0, uint64(size), nil)
			inode.mu.Unlock()
		}
	})
}

// Benchmark different buffer sizes to find optimal performance characteristics
func BenchmarkCacheReadSizes(b *testing.B) {
	sizes := []int{
		4 * 1024,        // 4KB
		16 * 1024,       // 16KB
		64 * 1024,       // 64KB
		128 * 1024,      // 128KB
		256 * 1024,      // 256KB
		512 * 1024,      // 512KB
		1024 * 1024,     // 1MB
		2 * 1024 * 1024, // 2MB
		4 * 1024 * 1024, // 4MB
		8 * 1024 * 1024, // 8MB
	}
	
	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%dKB", size/1024), func(b *testing.B) {
			cache := NewMockCache()
			testData := make([]byte, size)
			hash := "test-hash"
			cache.storage[hash] = testData
			
			fs := &Goofys{
				flags: &cfg.FlagStorage{
					ExternalCacheClient:           cache,
					ExternalCacheStreamingEnabled: false,
					HashAttr:                      "hash",
					MemoryLimit:                   100 * 1024 * 1024,
					PartSizes: []cfg.PartSizeConfig{
						{PartSize: 5 * 1024 * 1024, PartCount: 10000},
					},
				},
				bufferPool: NewBufferPool(100*1024*1024, 100*1024*1024),
			}
			
			inode := &Inode{
				fs:           fs,
				mu:           sync.Mutex{},
				userMetadata: map[string][]byte{"hash": []byte(hash)},
				Attributes:   InodeAttributes{Size: uint64(size)},
			}
			inode.buffers.helpers = inode
			inode.readCond = sync.NewCond(&inode.mu)
			
			b.ResetTimer()
			b.SetBytes(int64(size))
			
			for i := 0; i < b.N; i++ {
				_, _, err := inode.loadFromExternalCache(0, uint64(size), hash)
				if err != nil {
					b.Fatal(err)
				}
				inode.mu.Lock()
				inode.buffers.RemoveRange(0, uint64(size), nil)
				inode.mu.Unlock()
			}
			
			ops := float64(b.N)
			b.ReportMetric(ops*float64(size)/b.Elapsed().Seconds()/1024/1024, "MB/s")
		})
	}
}

// Benchmark lock contention
func BenchmarkLockContention(b *testing.B) {
	size := 1024 * 1024
	
	b.Run("WithLock", func(b *testing.B) {
		var mu sync.Mutex
		data := make([]byte, size)
		
		b.ResetTimer()
		b.SetBytes(int64(size))
		
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				mu.Lock()
				_ = data
				mu.Unlock()
			}
		})
	})
	
	b.Run("WithRWMutexRead", func(b *testing.B) {
		var mu sync.RWMutex
		data := make([]byte, size)
		
		b.ResetTimer()
		b.SetBytes(int64(size))
		
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				mu.RLock()
				_ = data
				mu.RUnlock()
			}
		})
	})
}
