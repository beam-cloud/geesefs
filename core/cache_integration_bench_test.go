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

// Benchmark the actual loadFromExternalCache function
func BenchmarkLoadFromExternalCache(b *testing.B) {
	sizes := []int{
		64 * 1024,      // 64KB
		1024 * 1024,    // 1MB
		5 * 1024 * 1024, // 5MB
	}

	for _, size := range sizes {
		for _, streaming := range []bool{false, true} {
			name := fmt.Sprintf("Size_%dKB_Streaming_%v", size/1024, streaming)
			b.Run(name, func(b *testing.B) {
				cache := NewMockCache()
				testData := make([]byte, size)
				for i := range testData {
					testData[i] = byte(i % 256)
				}

				hash := "test-hash-123"
				cache.storage[hash] = testData

				fs := &Goofys{
					flags: &cfg.FlagStorage{
						ExternalCacheClient:           cache,
						ExternalCacheStreamingEnabled: streaming,
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
					userMetadata: map[string][]byte{
						"hash": []byte(hash),
					},
					Attributes: InodeAttributes{Size: uint64(size)},
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
					// Clear buffers for next iteration
					inode.mu.Lock()
					inode.buffers.RemoveRange(0, uint64(size), nil)
					inode.mu.Unlock()
				}

				hits, misses := cache.Stats()
				b.ReportMetric(float64(hits)/float64(hits+misses)*100, "%cache_hit")
			})
		}
	}
}

// Benchmark concurrent cache reads through actual loadFromExternalCache
func BenchmarkLoadFromExternalCacheConcurrent(b *testing.B) {
	size := 1024 * 1024 // 1MB
	concurrency := []int{1, 4, 8, 16}

	for _, streaming := range []bool{false, true} {
		for _, conc := range concurrency {
			name := fmt.Sprintf("Concurrency_%d_Streaming_%v", conc, streaming)
			b.Run(name, func(b *testing.B) {
				cache := NewMockCache()
				testData := make([]byte, size)
				for i := range testData {
					testData[i] = byte(i % 256)
				}

				hash := "test-hash-123"
				cache.storage[hash] = testData

				fs := &Goofys{
					flags: &cfg.FlagStorage{
						ExternalCacheClient:           cache,
						ExternalCacheStreamingEnabled: streaming,
						HashAttr:                      "hash",
						MemoryLimit:                   100 * 1024 * 1024,
						PartSizes: []cfg.PartSizeConfig{
							{PartSize: 5 * 1024 * 1024, PartCount: 10000},
						},
					},
					bufferPool: NewBufferPool(100*1024*1024, 100*1024*1024),
				}

				// Create separate inodes for each concurrent goroutine
				inodes := make([]*Inode, conc)
				for i := 0; i < conc; i++ {
					inode := &Inode{
						fs: fs,
						mu: sync.Mutex{},
						userMetadata: map[string][]byte{
							"hash": []byte(hash),
						},
						Attributes: InodeAttributes{Size: uint64(size)},
					}
					inode.buffers.helpers = inode
					inode.readCond = sync.NewCond(&inode.mu)
					inodes[i] = inode
				}

				b.ResetTimer()
				b.SetBytes(int64(size))

				b.RunParallel(func(pb *testing.PB) {
					// Each goroutine gets its own inode to avoid lock contention on the same inode
					threadID := 0
					for pb.Next() {
						inode := inodes[threadID%conc]
						_, _, err := inode.loadFromExternalCache(0, uint64(size), hash)
						if err != nil {
							b.Fatal(err)
						}
						// Clear buffers for next iteration
						inode.mu.Lock()
						inode.buffers.RemoveRange(0, uint64(size), nil)
						inode.mu.Unlock()
						threadID++
					}
				})

				hits, misses := cache.Stats()
				b.ReportMetric(float64(hits)/float64(hits+misses)*100, "%cache_hit")
			})
		}
	}
}
