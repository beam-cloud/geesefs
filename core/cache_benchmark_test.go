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
)

// MockCache simulates a high-performance content cache
type MockCache struct {
	storage map[string][]byte
	hits    int64
	misses  int64
	mu      sync.RWMutex
}

func NewMockCache() *MockCache {
	return &MockCache{
		storage: make(map[string][]byte),
	}
}

func (m *MockCache) GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error) {
	m.mu.RLock()
	data, ok := m.storage[hash]
	m.mu.RUnlock()

	if !ok {
		m.mu.Lock()
		m.misses++
		m.mu.Unlock()
		return nil, fmt.Errorf("content not found")
	}

	m.mu.Lock()
	m.hits++
	m.mu.Unlock()

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

func (m *MockCache) GetContentStream(hash string, offset int64, length int64, opts struct {
	RoutingKey string
}) (chan []byte, error) {
	data, err := m.GetContent(hash, offset, length, opts)
	if err != nil {
		return nil, err
	}

	ch := make(chan []byte, 1)
	ch <- data
	close(ch)
	return ch, nil
}

func (m *MockCache) StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error) {
	var buffer []byte
	for chunk := range chunks {
		buffer = append(buffer, chunk...)
	}
	m.mu.Lock()
	m.storage[hash] = buffer
	m.mu.Unlock()
	return hash, nil
}

func (m *MockCache) StoreContentFromS3(source struct {
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
	return opts.RoutingKey, nil
}

func (m *MockCache) Stats() (hits, misses int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hits, m.misses
}

// Benchmark cache read throughput
func BenchmarkCacheReadThroughput(b *testing.B) {
	sizes := []int{
		4 * 1024,       // 4KB
		64 * 1024,      // 64KB
		1024 * 1024,    // 1MB
		5 * 1024 * 1024, // 5MB
	}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("Size_%dKB", size/1024), func(b *testing.B) {
			cache := NewMockCache()
			testData := make([]byte, size)
			for i := range testData {
				testData[i] = byte(i % 256)
			}

			hash := "test-hash"
			cache.storage[hash] = testData

			b.ResetTimer()
			b.SetBytes(int64(size))

			for i := 0; i < b.N; i++ {
				_, err := cache.GetContent(hash, 0, int64(size), struct{ RoutingKey string }{RoutingKey: hash})
				if err != nil {
					b.Fatal(err)
				}
			}

			hits, misses := cache.Stats()
			b.ReportMetric(float64(hits)/float64(hits+misses)*100, "%cache_hit")
		})
	}
}

// Benchmark concurrent cache reads
func BenchmarkCacheConcurrentReads(b *testing.B) {
	concurrencyLevels := []int{1, 4, 8, 16}
	size := 1024 * 1024 // 1MB

	for _, concurrency := range concurrencyLevels {
		b.Run(fmt.Sprintf("Concurrency_%d", concurrency), func(b *testing.B) {
			cache := NewMockCache()
			testData := make([]byte, size)
			for i := range testData {
				testData[i] = byte(i % 256)
			}

			hash := "test-hash"
			cache.storage[hash] = testData

			b.ResetTimer()
			b.SetBytes(int64(size))
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_, err := cache.GetContent(hash, 0, int64(size), struct{ RoutingKey string }{RoutingKey: hash})
					if err != nil {
						b.Fatal(err)
					}
				}
			})

			hits, misses := cache.Stats()
			b.ReportMetric(float64(hits)/float64(hits+misses)*100, "%cache_hit")
		})
	}
}

// Benchmark streaming vs non-streaming cache reads
func BenchmarkCacheStreamingVsNonStreaming(b *testing.B) {
	size := 5 * 1024 * 1024 // 5MB

	b.Run("NonStreaming", func(b *testing.B) {
		cache := NewMockCache()
		testData := make([]byte, size)
		hash := "test-hash"
		cache.storage[hash] = testData

		b.ResetTimer()
		b.SetBytes(int64(size))

		for i := 0; i < b.N; i++ {
			_, err := cache.GetContent(hash, 0, int64(size), struct{ RoutingKey string }{RoutingKey: hash})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Streaming", func(b *testing.B) {
		cache := NewMockCache()
		testData := make([]byte, size)
		hash := "test-hash"
		cache.storage[hash] = testData

		b.ResetTimer()
		b.SetBytes(int64(size))

		for i := 0; i < b.N; i++ {
			ch, err := cache.GetContentStream(hash, 0, int64(size), struct{ RoutingKey string }{RoutingKey: hash})
			if err != nil {
				b.Fatal(err)
			}
			for range ch {
				// Consume data
			}
		}
	})
}

// Benchmark buffer allocation strategies
func BenchmarkBufferAllocation(b *testing.B) {
	size := 1024 * 1024 // 1MB

	b.Run("MakeSlice", func(b *testing.B) {
		b.SetBytes(int64(size))
		for i := 0; i < b.N; i++ {
			buf := make([]byte, size)
			_ = buf
		}
	})

	b.Run("MakeWithCapacity", func(b *testing.B) {
		b.SetBytes(int64(size))
		for i := 0; i < b.N; i++ {
			buf := make([]byte, 0, size)
			_ = buf
		}
	})

	b.Run("Preallocated", func(b *testing.B) {
		pool := &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, size)
				return &buf
			},
		}
		b.ResetTimer()
		b.SetBytes(int64(size))

		for i := 0; i < b.N; i++ {
			buf := pool.Get().(*[]byte)
			pool.Put(buf)
		}
	})
}
