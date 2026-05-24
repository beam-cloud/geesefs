// Copyright 2015 - 2017 Ka-Hing Cheung
// Copyright 2021 Yandex LLC
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

//go:build !windows

package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

func TestExternalCacheLocalPageRegionReadUsesMmap(t *testing.T) {
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, []byte("abcdef"), 0644); err != nil {
		t.Fatal(err)
	}

	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		localPageRegions: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]struct {
			Path   string
			Offset int64
			Length int
		}, error) {
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || length != 3 {
				t.Fatalf("unexpected local page request: hash=%q routing=%q offset=%d length=%d", hash, opts.RoutingKey, offset, length)
			}
			return []struct {
				Path   string
				Offset int64
				Length int
			}{{Path: pagePath, Offset: 1, Length: 3}}, nil
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 3
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("expected mmap cleanup callback")
	}
	defer cleanup()
	if bytesRead != 3 {
		t.Fatalf("expected 3 bytes read, got %d", bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("bcd")) {
		t.Fatalf("unexpected data: %q", got)
	}
}

func TestExternalCacheLocalPageRegionReadUsesOpenHandleWindow(t *testing.T) {
	pagePath := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(pagePath, []byte("abcdef"), 0644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		localPageRegions: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]struct {
			Path   string
			Offset int64
			Length int
		}, error) {
			calls++
			if hash != "hash" || opts.RoutingKey != "hash" || offset != 0 || length != 6 {
				t.Fatalf("unexpected local page request: hash=%q routing=%q offset=%d length=%d", hash, opts.RoutingKey, offset, length)
			}
			return []struct {
				Path   string
				Offset int64
				Length int
			}{{Path: pagePath, Offset: 0, Length: 6}}, nil
		},
	}
	fs := newUnitFS(flags)
	inode := NewInode(fs, nil, "file")
	inode.Attributes.Size = 6
	inode.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash")}
	fh := NewFileHandle(inode)
	defer fs.closeExternalPageMmapCache()

	data, bytesRead, cleanup, err := fh.ReadFileWithCallback(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != 3 {
		t.Fatalf("expected 3 bytes read, got %d", bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("unexpected data: %q", got)
	}
	if cleanup != nil {
		cleanup()
	}

	data, bytesRead, cleanup, err = fh.ReadFileWithCallback(3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != 3 {
		t.Fatalf("expected 3 bytes read, got %d", bytesRead)
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("def")) {
		t.Fatalf("unexpected data: %q", got)
	}
	if cleanup != nil {
		cleanup()
	}
	if calls != 1 {
		t.Fatalf("expected one local page lookup, got %d", calls)
	}
}

func TestExternalCacheLocalPageRegionWindowSeparatesHashes(t *testing.T) {
	dir := t.TempDir()
	pageA := filepath.Join(dir, "page-a")
	pageB := filepath.Join(dir, "page-b")
	if err := os.WriteFile(pageA, []byte("aaaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pageB, []byte("bbbb"), 0644); err != nil {
		t.Fatal(err)
	}

	flags := cfg.DefaultFlags()
	flags.ExternalCacheClient = &fakeContentCache{
		localPageRegions: func(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]struct {
			Path   string
			Offset int64
			Length int
		}, error) {
			path := pageA
			if hash == "hash-b" {
				path = pageB
			}
			return []struct {
				Path   string
				Offset int64
				Length int
			}{{Path: path, Offset: 0, Length: int(length)}}, nil
		},
	}
	fs := newUnitFS(flags)
	defer fs.closeExternalPageMmapCache()

	inodeA := NewInode(fs, nil, "file-a")
	inodeA.Attributes.Size = 4
	inodeA.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash-a")}
	fhA := NewFileHandle(inodeA)

	inodeB := NewInode(fs, nil, "file-b")
	inodeB.Attributes.Size = 4
	inodeB.userMetadata = map[string][]byte{flags.HashAttr: []byte("hash-b")}
	fhB := NewFileHandle(inodeB)

	data, _, cleanup, err := fhA.ReadFileWithCallback(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("aaaa")) {
		t.Fatalf("unexpected data for hash-a: %q", got)
	}

	data, _, cleanup, err = fhB.ReadFileWithCallback(0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("bbbb")) {
		t.Fatalf("unexpected data for hash-b: %q", got)
	}
}

func TestExternalPageMmapCacheEvictsUnreferencedEntries(t *testing.T) {
	dir := t.TempDir()
	pageA := filepath.Join(dir, "page-a")
	pageB := filepath.Join(dir, "page-b")
	if err := os.WriteFile(pageA, []byte("aaaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pageB, []byte("bbbb"), 0644); err != nil {
		t.Fatal(err)
	}

	cache := newExternalPageMmapCache(4)
	defer cache.close()

	regionsA := []struct {
		Path   string
		Offset int64
		Length int
	}{{Path: pageA, Offset: 0, Length: 4}}
	if err := cache.insertWindow("hash-a", 0, regionsA); err != nil {
		t.Fatal(err)
	}
	data, cleanup, ok := cache.lookup("hash-a", 0, 4)
	if !ok {
		t.Fatal("expected hash-a lookup")
	}
	if got := bytes.Join(data, nil); !bytes.Equal(got, []byte("aaaa")) {
		t.Fatalf("unexpected data for hash-a: %q", got)
	}
	cleanup()

	regionsB := []struct {
		Path   string
		Offset int64
		Length int
	}{{Path: pageB, Offset: 0, Length: 4}}
	if err := cache.insertWindow("hash-b", 0, regionsB); err != nil {
		t.Fatal(err)
	}

	cache.mu.Lock()
	entries := len(cache.entries)
	mappedBytes := cache.mappedBytes
	cache.mu.Unlock()
	if entries != 1 || mappedBytes != 4 {
		t.Fatalf("expected one mapped page after eviction, got entries=%d mappedBytes=%d", entries, mappedBytes)
	}
}
