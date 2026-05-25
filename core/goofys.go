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

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/yandex-cloud/geesefs/core/cfg"

	"context"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"

	"github.com/jacobsa/fuse/fuseops"

	"net/http"

	"github.com/sirupsen/logrus"
)

// goofys is a Filey System written in Go. All the backend data is
// stored on S3 as is. It's a Filey System instead of a File System
// because it makes minimal effort at being POSIX
// compliant. Particularly things that are difficult to support on S3
// or would translate into more than one round-trip would either fail
// (rename non-empty dir) or faked (no per-file permission). goofys
// does not have a on disk data cache, and consistency model is
// close-to-open.

type cacheEvent struct {
	path             string
	size             uint64
	hash             string
	inode            *Inode
	localSourcePath  string
	removeLocalAfter bool
}

const externalCacheStoreAttempts = 5
const externalCacheStoreChunkSize = 4 * 1024 * 1024

var externalCacheStoreRetryDelay = func(attempt int) time.Duration {
	delay := time.Duration(250*(1<<(attempt-1))) * time.Millisecond
	if delay > 2*time.Second {
		return 2 * time.Second
	}
	return delay
}

type Goofys struct {
	ctx    context.Context
	bucket string

	flags *cfg.FlagStorage

	umask uint32

	rootAttrs InodeAttributes

	bufferPool *BufferPool
	wantFree   int32

	shutdown   int32
	shutdownCh chan struct{}

	// A lock protecting the state of the file system struct itself (distinct
	// from per-inode locks). Should be always taken after any inode locks.
	mu sync.RWMutex

	flusherMu    sync.Mutex
	flusherCond  *sync.Cond
	flushPending int32

	// The next inode ID to hand out. We assume that this will never overflow,
	// since even if we were handing out inode IDs at 4 GHz, it would still take
	// over a century to do so.
	//
	// GUARDED_BY(mu)
	nextInodeID fuseops.InodeID

	// The collection of live inodes, keyed by inode ID. No ID less than
	// fuseops.RootInodeID is ever used.
	//
	// INVARIANT: For all keys k, fuseops.RootInodeID <= k < nextInodeID
	// INVARIANT: For all keys k, inodes[k].ID() == k
	// INVARIANT: inodes[fuseops.RootInodeID] is missing or of type inode.DirInode
	// INVARIANT: For all v, if IsDirName(v.Name()) then v is inode.DirInode
	//
	// GUARDED_BY(mu)
	inodes map[fuseops.InodeID]*Inode

	inodesByTime map[int64]map[fuseops.InodeID]bool

	// Inflight changes are tracked to skip them in parallel listings
	// Required because we don't have guarantees about listing & change ordering
	inflightListingId int
	inflightListings  map[int]map[string]bool
	inflightChanges   map[string]int

	nextHandleID fuseops.HandleID
	dirHandles   map[fuseops.HandleID]*DirHandle

	fileHandles map[fuseops.HandleID]*FileHandle

	activeFlushers  int64
	flushRetrySet   int32
	hasNewWrites    uint64
	flushPriorities []int64

	forgotCnt uint32

	cleanQueue BufferQueue
	inodeQueue InodeQueue

	zeroBuf []byte

	diskFdQueue *FDQueue

	stats OpStats

	NotifyCallback func(notifications []interface{})

	cacheEventChan  chan cacheEvent
	cachingStatus   map[string]bool
	cachingStatusMu sync.Mutex

	externalPageMmapCache   *externalPageMmapCache
	externalPageMmapCacheMu sync.Mutex

	stagedFiles sync.Map
}

type OpStats struct {
	reads                    int64
	readBytes                int64
	readSlow                 int64
	readErrors               int64
	readHandlerCount         int64
	readHandlerNanos         int64
	readCallbackCount        int64
	readCallbackBytes        int64
	readCallbackNanos        int64
	readHits                 int64
	readBufferHits           int64
	readBufferBytes          int64
	externalPageAttempts     int64
	externalPageHits         int64
	externalPageMisses       int64
	externalPageMmapFailures int64
	externalPageBytes        int64
	externalPageLookupCount  int64
	externalPageLookupNanos  int64
	externalPageMmapCount    int64
	externalPageMmapNanos    int64
	externalReadIntoAttempts int64
	externalReadIntoHits     int64
	externalReadIntoMisses   int64
	externalReadIntoBytes    int64
	externalStreamAttempts   int64
	externalStreamHits       int64
	externalStreamMisses     int64
	externalStreamBytes      int64
	externalUnaryAttempts    int64
	externalUnaryHits        int64
	externalUnaryMisses      int64
	externalUnaryBytes       int64
	cacheEventsQueued        int64
	cacheEventsStarted       int64
	cacheEventsSuccess       int64
	cacheEventsErrors        int64
	cacheEventsMismatch      int64
	cacheEventsDropped       int64
	cacheEventsBytes         int64
	cloudReadRequests        int64
	cloudReadBytes           int64
	writes                   int64
	flushes                  int64
	metadataReads            int64
	metadataWrites           int64
	noops                    int64
	evicts                   int64
	ts                       time.Time
}

var s3Log = cfg.GetLogger("s3")
var log = cfg.GetLogger("main")
var fuseLog = cfg.GetLogger("fuse")

func NewBackend(bucket string, flags *cfg.FlagStorage) (cloud StorageBackend, err error) {
	if flags.Backend == nil {
		flags.Backend = (&cfg.S3Config{}).Init()
	}

	if config, ok := flags.Backend.(*cfg.AZBlobConfig); ok {
		cloud, err = NewAZBlob(bucket, config)
	} else if config, ok := flags.Backend.(*cfg.ADLv1Config); ok {
		cloud, err = NewADLv1(bucket, flags, config)
	} else if config, ok := flags.Backend.(*cfg.ADLv2Config); ok {
		cloud, err = NewADLv2(bucket, flags, config)
	} else if config, ok := flags.Backend.(*cfg.S3Config); ok {
		if strings.HasSuffix(flags.Endpoint, "/storage.googleapis.com") {
			cloud, err = NewGCS3(bucket, flags, config)
		} else {
			cloud, err = NewS3(bucket, flags, config)
		}
	} else {
		err = fmt.Errorf("Unknown backend config: %T", flags.Backend)
	}

	return
}

type BucketSpec struct {
	Scheme string
	Bucket string
	Prefix string
}

func ParseBucketSpec(bucket string) (spec BucketSpec, err error) {
	if strings.Index(bucket, "://") != -1 {
		var u *url.URL
		u, err = url.Parse(bucket)
		if err != nil {
			return
		}

		spec.Scheme = u.Scheme
		spec.Bucket = u.Host
		if u.User != nil {
			// wasb url can be wasb://container@storage-end-point
			// we want to return the entire thing as bucket
			spec.Bucket = u.User.String() + "@" + u.Host
		}
		spec.Prefix = u.Path
	} else {
		spec.Scheme = "s3"

		colon := strings.Index(bucket, ":")
		if colon != -1 {
			spec.Prefix = bucket[colon+1:]
			spec.Bucket = bucket[0:colon]
		} else {
			spec.Bucket = bucket
		}
	}

	spec.Prefix = strings.Trim(spec.Prefix, "/")
	if spec.Prefix != "" {
		spec.Prefix += "/"
	}
	return
}

func NewGoofys(ctx context.Context, bucketName string, flags *cfg.FlagStorage) (*Goofys, error) {
	if flags.DebugFuse || flags.DebugMain {
		log.Level = logrus.DebugLevel
	}
	if flags.DebugFuse {
		fuseLog.Level = logrus.DebugLevel
	}
	if flags.DebugS3 {
		cfg.SetCloudLogLevel(logrus.DebugLevel)
	}

	if flags.Backend == nil {
		if spec, err := ParseBucketSpec(bucketName); err == nil {
			switch spec.Scheme {
			case "adl":
				auth, err := cfg.AzureAuthorizerConfig{
					Log: cfg.GetLogger("adlv1"),
				}.Authorizer()
				if err != nil {
					err = fmt.Errorf("couldn't load azure credentials: %v",
						err)
					return nil, err
				}
				flags.Backend = &cfg.ADLv1Config{
					Endpoint:   spec.Bucket,
					Authorizer: auth,
				}
				// adlv1 doesn't really have bucket
				// names, but we will rebuild the
				// prefix
				bucketName = ""
				if spec.Prefix != "" {
					bucketName = ":" + spec.Prefix
				}
			case "wasb":
				config, err := cfg.AzureBlobConfig(flags.Endpoint, spec.Bucket, "blob")
				if err != nil {
					return nil, err
				}
				flags.Backend = &config
				if config.Container != "" {
					bucketName = config.Container
				} else {
					bucketName = spec.Bucket
				}
				if config.Prefix != "" {
					spec.Prefix = config.Prefix
				}
				if spec.Prefix != "" {
					bucketName += ":" + spec.Prefix
				}
			case "abfs":
				config, err := cfg.AzureBlobConfig(flags.Endpoint, spec.Bucket, "dfs")
				if err != nil {
					return nil, err
				}
				flags.Backend = &config
				if config.Container != "" {
					bucketName = config.Container
				} else {
					bucketName = spec.Bucket
				}
				if config.Prefix != "" {
					spec.Prefix = config.Prefix
				}
				if spec.Prefix != "" {
					bucketName += ":" + spec.Prefix
				}

				flags.Backend = &cfg.ADLv2Config{
					Endpoint:   config.Endpoint,
					Authorizer: &config,
				}
				bucketName = spec.Bucket
				if spec.Prefix != "" {
					bucketName += ":" + spec.Prefix
				}
			}
		}
	}
	return newGoofys(ctx, bucketName, flags, NewBackend)
}

func newGoofys(ctx context.Context, bucket string, flags *cfg.FlagStorage,
	newBackend func(string, *cfg.FlagStorage) (StorageBackend, error)) (*Goofys, error) {

	// Set up the basic struct.
	fs := &Goofys{
		ctx:              ctx,
		bucket:           bucket,
		flags:            flags,
		umask:            0122,
		shutdownCh:       make(chan struct{}),
		zeroBuf:          make([]byte, 1048576),
		inflightChanges:  make(map[string]int),
		inflightListings: make(map[int]map[string]bool),
		stats: OpStats{
			ts: time.Now(),
		},
		flushPriorities: make([]int64, MAX_FLUSH_PRIORITY+1),
		cacheEventChan:  make(chan cacheEvent, 10000),
		cachingStatus:   make(map[string]bool),
		cachingStatusMu: sync.Mutex{},
	}

	var prefix string
	colon := strings.Index(bucket, ":")
	if colon != -1 {
		prefix = bucket[colon+1:]
		prefix = strings.Trim(prefix, "/")
		if prefix != "" {
			prefix += "/"
		}

		fs.bucket = bucket[0:colon]
		bucket = fs.bucket
	}

	if flags.DebugS3 {
		s3Log.Level = logrus.DebugLevel
	}

	cloud, err := newBackend(bucket, flags)
	if err != nil {
		return nil, fmt.Errorf("Unable to setup backend: %v", err)
	}

	randomObjectName := prefix + (RandStringBytesMaskImprSrc(32))
	err = cloud.Init(randomObjectName)
	if err != nil {
		return nil, fmt.Errorf("Unable to access '%v': %v", bucket, err)
	}
	cloud.MultipartExpire(&MultipartExpireInput{})

	now := time.Now()
	fs.rootAttrs = InodeAttributes{
		Size:  4096,
		Ctime: now,
		Mtime: now,
	}

	if os.Getenv("GOGC") == "" {
		// Set garbage collection ratio to 20 instead of 100 by default.
		debug.SetGCPercent(20)
	}

	fs.bufferPool = NewBufferPool(int64(flags.MemoryLimit), uint64(flags.GCInterval)<<20)
	fs.bufferPool.FreeSomeCleanBuffers = func(size int64) (int64, bool) {
		return fs.FreeSomeCleanBuffers(size)
	}

	fs.nextInodeID = fuseops.RootInodeID + 1
	fs.inodes = make(map[fuseops.InodeID]*Inode)
	fs.inodesByTime = make(map[int64]map[fuseops.InodeID]bool)
	root := NewInode(fs, nil, "")
	root.refcnt = 1
	root.Id = fuseops.RootInodeID
	root.ToDir()
	root.dir.cloud = cloud
	root.dir.mountPrefix = prefix
	root.userMetadata = make(map[string][]byte)
	root.Attributes.Mtime = fs.rootAttrs.Mtime
	root.Attributes.Ctime = fs.rootAttrs.Ctime

	fs.inodes[fuseops.RootInodeID] = root

	fs.nextHandleID = 1
	fs.dirHandles = make(map[fuseops.HandleID]*DirHandle)

	fs.fileHandles = make(map[fuseops.HandleID]*FileHandle)

	fs.flusherCond = sync.NewCond(&fs.flusherMu)
	go fs.Flusher()
	if fs.flags.StatsInterval > 0 {
		go fs.StatPrinter()
	}

	if fs.flags.CachePath != "" {
		fs.diskFdQueue = NewFDQueue(int(fs.flags.MaxDiskCacheFD))
		if fs.flags.MaxDiskCacheFD > 0 {
			go fs.FDCloser()
		}
	}

	go fs.MetaEvictor()
	go fs.StagedFileFlusher()
	go fs.processCacheEvents()

	return fs, nil
}

func (fs *Goofys) processCacheEvents() {
	for {
		select {
		case <-fs.shutdownCh:
			for {
				select {
				case cacheEvent := <-fs.cacheEventChan:
					fs.processCacheEvent(cacheEvent)
				default:
					return
				}
			}
		case cacheEvent := <-fs.cacheEventChan:
			fs.processCacheEvent(cacheEvent)
		}
	}
}

func (fs *Goofys) processCacheEvent(cacheEvent cacheEvent) {
	started := time.Now()
	atomic.AddInt64(&fs.stats.cacheEventsStarted, 1)
	atomic.AddInt64(&fs.stats.cacheEventsBytes, int64(cacheEvent.size))
	source := "s3"
	if cacheEvent.localSourcePath != "" {
		source = "local"
	}
	if cacheEvent.hash == "" {
		log.Errorf("No hash found for inode, not caching inode in external cache: %v", cacheEvent.path)
		atomic.AddInt64(&fs.stats.cacheEventsErrors, 1)
		return
	}
	log.Debugf("geesefs external cache store start: path=%q hash=%q size=%d source=%s local_source=%q queue_depth=%d", cacheEvent.path, cacheEvent.hash, cacheEvent.size, source, cacheEvent.localSourcePath, len(fs.cacheEventChan))

	if cacheEvent.size > 0 {
		var (
			hash string
			err  error
		)
		for attempt := 1; attempt <= externalCacheStoreAttempts; attempt++ {
			hash, err = fs.storeCacheEventContent(cacheEvent)
			if err == nil && hash == cacheEvent.hash {
				break
			}
			if attempt == externalCacheStoreAttempts {
				break
			}
			delay := externalCacheStoreRetryDelay(attempt)
			if err != nil {
				log.Debugf("geesefs external cache store retry: path=%q hash=%q source=%s size=%d attempt=%d/%d delay=%s err=%v", cacheEvent.path, cacheEvent.hash, source, cacheEvent.size, attempt, externalCacheStoreAttempts, delay, err)
			} else {
				log.Debugf("geesefs external cache store retry: path=%q hash=%q actual=%q source=%s size=%d attempt=%d/%d delay=%s err=hash_mismatch", cacheEvent.path, cacheEvent.hash, hash, source, cacheEvent.size, attempt, externalCacheStoreAttempts, delay)
			}
			time.Sleep(delay)
		}
		if err != nil {
			atomic.AddInt64(&fs.stats.cacheEventsErrors, 1)
			log.Warnf("geesefs external cache store result: status=error path=%q hash=%q source=%s size=%d elapsed=%s err=%v", cacheEvent.path, cacheEvent.hash, source, cacheEvent.size, time.Since(started).Truncate(time.Millisecond), err)
		} else if hash != cacheEvent.hash {
			atomic.AddInt64(&fs.stats.cacheEventsMismatch, 1)
			log.Warnf("geesefs external cache store result: status=hash_mismatch path=%q expected=%q actual=%q source=%s size=%d elapsed=%s", cacheEvent.path, cacheEvent.hash, hash, source, cacheEvent.size, time.Since(started).Truncate(time.Millisecond))
			fs.clearCachingStatus(cacheEvent.hash)
		} else if hash == cacheEvent.hash {
			atomic.AddInt64(&fs.stats.cacheEventsSuccess, 1)
			log.Debugf("geesefs external cache store result: status=ok path=%q hash=%q source=%s size=%d elapsed=%s", cacheEvent.path, cacheEvent.hash, source, cacheEvent.size, time.Since(started).Truncate(time.Millisecond))
			if cacheEvent.inode != nil {
				cacheEvent.inode.dropCleanBuffersAfterExternalCacheStore(cacheEvent.hash)
			}
		}

		fs.clearCachingStatus(cacheEvent.hash)
	} else {
		log.Debugf("geesefs external cache store result: status=empty path=%q hash=%q source=%s elapsed=%s", cacheEvent.path, cacheEvent.hash, source, time.Since(started).Truncate(time.Millisecond))
		fs.clearCachingStatus(cacheEvent.hash)
	}

	if cacheEvent.removeLocalAfter {
		err := os.RemoveAll(cacheEvent.localSourcePath)
		if err != nil {
			log.Warnf("Failed to remove staged cache source %v: %v", cacheEvent.localSourcePath, err)
		}
	}
}

func (fs *Goofys) storeCacheEventContent(cacheEvent cacheEvent) (string, error) {
	if cacheEvent.localSourcePath != "" {
		return fs.storeContentFromLocalFile(cacheEvent)
	}

	s3, ok := fs.flags.Backend.(*cfg.S3Config)
	if !ok {
		return "", fmt.Errorf("backend is not S3, not caching inode in external cache: %v", cacheEvent.path)
	}
	return fs.flags.ExternalCacheClient.StoreContentFromS3(struct {
		Path        string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}{
		Path:        cacheEvent.path,
		BucketName:  fs.bucket,
		Region:      s3.Region,
		EndpointURL: fs.flags.Endpoint,
		AccessKey:   s3.AccessKey,
		SecretKey:   s3.SecretKey,
	}, struct {
		RoutingKey string
		Lock       bool
	}{RoutingKey: cacheEvent.hash, Lock: true})
}

func (fs *Goofys) CacheFileInExternalCache(inode *Inode) {
	fs.CacheFileInExternalCacheFromSource(inode, "", false)
}

func (fs *Goofys) reserveExternalCacheStore(inode *Inode, hashString string) bool {
	fs.cachingStatusMu.Lock()
	if fs.cachingStatus[hashString] {
		fs.cachingStatusMu.Unlock()
		return false
	}
	fs.cachingStatus[hashString] = true
	fs.cachingStatusMu.Unlock()

	if fs.flags.EventCallback != nil {
		fs.flags.EventCallback(cfg.EventCacheTriggered, map[string]interface{}{
			"inode": inode.FullName(),
			"hash":  hashString,
		})
	}

	return true
}

func (fs *Goofys) CacheFileInExternalCacheFromSource(inode *Inode, localSourcePath string, removeLocalAfter bool) bool {
	if inode.userMetadata == nil {
		log.Errorf("No metadata found for inode, not caching inode in external cache: %v", inode.FullName())
		return false
	}
	hash, ok := inode.userMetadata[fs.flags.HashAttr]
	if !ok {
		log.Errorf("No hash found for inode, not caching inode in external cache: %v", inode.FullName())
		return false
	}

	hashString := string(hash)

	if !fs.reserveExternalCacheStore(inode, hashString) {
		return false
	}

	event := cacheEvent{
		path:             inode.FullName(),
		size:             inode.Attributes.Size,
		hash:             hashString,
		inode:            inode,
		localSourcePath:  localSourcePath,
		removeLocalAfter: removeLocalAfter,
	}

	// Submit cache event
	log.Debugf("Submitting cache event for file: %v", inode.FullName())
	select {
	case fs.cacheEventChan <- event:
		atomic.AddInt64(&fs.stats.cacheEventsQueued, 1)
		return true
	default:
		log.Warnf("External cache event queue is full, skipping cache for %v", inode.FullName())
		atomic.AddInt64(&fs.stats.cacheEventsDropped, 1)
		fs.clearCachingStatus(hashString)
		return false
	}
}

// LOCKS_REQUIRED(inode.mu)
func (fs *Goofys) CacheFileInExternalCacheFromBuffersLocked(inode *Inode) bool {
	if !fs.flags.CacheThroughModeEnabled || fs.flags.ExternalCacheClient == nil || inode.StagedFile != nil {
		return false
	}
	if inode.userMetadata == nil {
		log.Errorf("No metadata found for inode, not caching inode in external cache: %v", inode.FullName())
		return false
	}
	hash, ok := inode.userMetadata[fs.flags.HashAttr]
	if !ok || len(hash) == 0 {
		log.Errorf("No hash found for inode, not caching inode in external cache: %v", inode.FullName())
		return false
	}

	hashString := string(hash)
	if !fs.reserveExternalCacheStore(inode, hashString) {
		return false
	}

	path := inode.FullName()
	size := inode.Attributes.Size
	atomic.AddInt64(&fs.stats.cacheEventsQueued, 1)
	atomic.AddInt64(&fs.stats.cacheEventsStarted, 1)
	atomic.AddInt64(&fs.stats.cacheEventsBytes, int64(size))
	log.Debugf("geesefs external cache store start: path=%q hash=%q size=%d source=flushed_buffers queue_depth=%d", path, hashString, size, len(fs.cacheEventChan))

	var actualHash string
	var err error
	for attempt := 1; attempt <= externalCacheStoreAttempts; attempt++ {
		inode.mu.Unlock()
		actualHash, err = fs.storeContentFromInodeBuffers(inode, path, hashString, size)
		inode.mu.Lock()
		if err == nil && actualHash == hashString {
			atomic.AddInt64(&fs.stats.cacheEventsSuccess, 1)
			log.Debugf("geesefs external cache store result: status=ok path=%q hash=%q source=flushed_buffers size=%d", path, hashString, size)
			return true
		}

		if attempt == externalCacheStoreAttempts {
			break
		}

		delay := externalCacheStoreRetryDelay(attempt)
		if err != nil {
			log.Debugf("geesefs external cache store retry: path=%q hash=%q source=flushed_buffers size=%d attempt=%d/%d delay=%s err=%v", path, hashString, size, attempt, externalCacheStoreAttempts, delay, err)
		} else {
			log.Debugf("geesefs external cache store retry: path=%q hash=%q actual=%q source=flushed_buffers size=%d attempt=%d/%d delay=%s err=hash_mismatch", path, hashString, actualHash, size, attempt, externalCacheStoreAttempts, delay)
		}
		inode.mu.Unlock()
		time.Sleep(delay)
		inode.mu.Lock()
	}

	if err != nil {
		atomic.AddInt64(&fs.stats.cacheEventsErrors, 1)
		log.Warnf("geesefs external cache store result: status=error path=%q hash=%q source=flushed_buffers size=%d err=%v", path, hashString, size, err)
	} else {
		atomic.AddInt64(&fs.stats.cacheEventsMismatch, 1)
		log.Warnf("geesefs external cache store result: status=hash_mismatch path=%q expected=%q actual=%q source=flushed_buffers size=%d", path, hashString, actualHash, size)
	}
	fs.clearCachingStatus(hashString)
	return false
}

func (fs *Goofys) storeContentFromInodeBuffers(inode *Inode, path, hash string, size uint64) (string, error) {
	chunks := make(chan []byte, 2)
	done := make(chan struct{})
	readErr := make(chan error, 1)

	go func() {
		defer close(chunks)
		readErr <- fs.streamInodeBufferChunks(inode, path, hash, size, chunks, done)
	}()

	actualHash, err := fs.flags.ExternalCacheClient.StoreContent(chunks, hash, struct{ RoutingKey string }{RoutingKey: hash})
	close(done)
	if readErrValue := <-readErr; err == nil && readErrValue != nil {
		err = readErrValue
	}
	return actualHash, err
}

func (fs *Goofys) streamInodeBufferChunks(inode *Inode, path, hash string, size uint64, chunks chan<- []byte, done <-chan struct{}) error {
	for offset := uint64(0); offset < size; {
		chunkSize := uint64(externalCacheStoreChunkSize)
		if remaining := size - offset; chunkSize > remaining {
			chunkSize = remaining
		}

		chunk, err := inode.copyCacheThroughChunk(path, hash, size, offset, chunkSize)
		if err != nil {
			return err
		}

		select {
		case chunks <- chunk:
		case <-done:
			return nil
		}

		offset += chunkSize
	}
	return nil
}

func (inode *Inode) validateCacheThroughSourceLocked(path, hash string, size uint64) error {
	if inode.FullName() != path {
		return fmt.Errorf("inode path changed during cache-through: expected %q got %q", path, inode.FullName())
	}
	if inode.Attributes.Size != size {
		return fmt.Errorf("inode size changed during cache-through: expected %d got %d", size, inode.Attributes.Size)
	}
	if inode.StagedFile != nil {
		return fmt.Errorf("inode has staged file during cache-through: %s", path)
	}
	if inode.userMetadata == nil || string(inode.userMetadata[inode.fs.flags.HashAttr]) != hash {
		return fmt.Errorf("inode hash changed during cache-through: %s", path)
	}
	return nil
}

func (inode *Inode) copyCacheThroughChunk(path, hash string, totalSize, offset, size uint64) ([]byte, error) {
	inode.mu.Lock()
	defer inode.mu.Unlock()

	if err := inode.validateCacheThroughSourceLocked(path, hash, totalSize); err != nil {
		return nil, err
	}
	inode.LockRange(offset, size, false)
	defer inode.UnlockRange(offset, size, false)

	if err := inode.loadDiskBackedBuffers(offset, size); err != nil {
		return nil, err
	}
	if err := inode.validateCacheThroughSourceLocked(path, hash, totalSize); err != nil {
		return nil, err
	}

	reader, _, err := inode.getMultiReader(offset, size)
	if err != nil {
		return nil, err
	}
	chunk := make([]byte, int(size))
	n, err := io.ReadFull(reader, chunk)
	if err != nil {
		return nil, err
	}
	if n != len(chunk) {
		return nil, fmt.Errorf("short cache-through buffer read: path=%q offset=%d size=%d read=%d", path, offset, size, n)
	}
	return chunk, nil
}

func (fs *Goofys) storeContentFromLocalFile(event cacheEvent) (string, error) {
	if localStore, ok := fs.flags.ExternalCacheClient.(cfg.ContentCacheStoreLocalPath); ok && localStore != nil {
		return localStore.StoreContentFromLocalPath(struct {
			Path      string
			CachePath string
		}{
			Path:      event.localSourcePath,
			CachePath: event.path,
		}, struct {
			RoutingKey string
			Lock       bool
		}{RoutingKey: event.hash, Lock: true})
	}

	file, err := os.Open(event.localSourcePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	chunks := make(chan []byte, 2)
	done := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(readErr)

		buf := make([]byte, 4*1024*1024)
		for {
			n, err := file.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case chunks <- chunk:
				case <-done:
					readErr <- nil
					return
				}
			}
			if err == io.EOF {
				readErr <- nil
				return
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	hash, err := fs.flags.ExternalCacheClient.StoreContent(chunks, event.hash, struct{ RoutingKey string }{RoutingKey: event.hash})
	close(done)
	if readErrValue := <-readErr; err == nil && readErrValue != nil {
		err = readErrValue
	}
	return hash, err
}

func (fs *Goofys) clearCachingStatus(hash string) {
	fs.cachingStatusMu.Lock()
	defer fs.cachingStatusMu.Unlock()
	delete(fs.cachingStatus, hash)
}

func (fs *Goofys) Shutdown() {
	atomic.StoreInt32(&fs.shutdown, 1)
	fs.closeExternalPageMmapCache()
	close(fs.shutdownCh)
	fs.WakeupFlusher()
	if fs.diskFdQueue != nil {
		fs.diskFdQueue.cond.Broadcast()
	}
}

// from https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-golang
func RandStringBytesMaskImprSrc(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyz0123456789"
	const (
		letterIdxBits = 6                    // 6 bits to represent a letter index
		letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
		letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
	)
	src := rand.NewSource(time.Now().UnixNano())
	b := make([]byte, n)
	// A src.Int63() generates 63 random bits, enough for letterIdxMax characters!
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}

	return string(b)
}

func (fs *Goofys) SigUsr1() {
	fs.mu.RLock()

	log.Infof("forgot %v inodes", fs.forgotCnt)
	log.Infof("%v inodes", len(fs.inodes))
	fs.mu.RUnlock()
	debug.FreeOSMemory()
}

// Find the given inode. Panic if it doesn't exist.
//
// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) getInodeOrDie(id fuseops.InodeID) (inode *Inode) {
	fs.mu.RLock()
	inode = fs.inodes[id]
	fs.mu.RUnlock()
	if inode == nil {
		panic(fmt.Sprintf("Unknown inode: %v", id))
	}

	return
}

func (fs *Goofys) AddDirHandle(dh *DirHandle) fuseops.HandleID {
	fs.mu.Lock()
	handleID := fs.nextHandleID
	fs.nextHandleID++
	fs.dirHandles[handleID] = dh
	fs.mu.Unlock()
	return handleID
}

func (fs *Goofys) AddFileHandle(fh *FileHandle) fuseops.HandleID {
	fs.mu.Lock()
	handleID := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handleID] = fh
	fs.mu.Unlock()
	return handleID
}

func avgDuration(nanos, count int64) time.Duration {
	if count <= 0 {
		return 0
	}
	return time.Duration(nanos / count).Truncate(time.Microsecond)
}

func (fs *Goofys) StatPrinter() {
	for atomic.LoadInt32(&fs.shutdown) == 0 {
		select {
		case <-time.After(fs.flags.StatsInterval):
		case <-fs.shutdownCh:
			return
		}
		now := time.Now()
		d := now.Sub(fs.stats.ts).Seconds()
		reads := atomic.SwapInt64(&fs.stats.reads, 0)
		readBytes := atomic.SwapInt64(&fs.stats.readBytes, 0)
		readSlow := atomic.SwapInt64(&fs.stats.readSlow, 0)
		readErrors := atomic.SwapInt64(&fs.stats.readErrors, 0)
		readHandlerCount := atomic.SwapInt64(&fs.stats.readHandlerCount, 0)
		readHandlerNanos := atomic.SwapInt64(&fs.stats.readHandlerNanos, 0)
		readCallbackCount := atomic.SwapInt64(&fs.stats.readCallbackCount, 0)
		readCallbackBytes := atomic.SwapInt64(&fs.stats.readCallbackBytes, 0)
		readCallbackNanos := atomic.SwapInt64(&fs.stats.readCallbackNanos, 0)
		readHits := atomic.SwapInt64(&fs.stats.readHits, 0)
		readBufferHits := atomic.SwapInt64(&fs.stats.readBufferHits, 0)
		readBufferBytes := atomic.SwapInt64(&fs.stats.readBufferBytes, 0)
		externalPageAttempts := atomic.SwapInt64(&fs.stats.externalPageAttempts, 0)
		externalPageHits := atomic.SwapInt64(&fs.stats.externalPageHits, 0)
		externalPageMisses := atomic.SwapInt64(&fs.stats.externalPageMisses, 0)
		externalPageMmapFailures := atomic.SwapInt64(&fs.stats.externalPageMmapFailures, 0)
		externalPageBytes := atomic.SwapInt64(&fs.stats.externalPageBytes, 0)
		externalPageLookupCount := atomic.SwapInt64(&fs.stats.externalPageLookupCount, 0)
		externalPageLookupNanos := atomic.SwapInt64(&fs.stats.externalPageLookupNanos, 0)
		externalPageMmapCount := atomic.SwapInt64(&fs.stats.externalPageMmapCount, 0)
		externalPageMmapNanos := atomic.SwapInt64(&fs.stats.externalPageMmapNanos, 0)
		externalReadIntoAttempts := atomic.SwapInt64(&fs.stats.externalReadIntoAttempts, 0)
		externalReadIntoHits := atomic.SwapInt64(&fs.stats.externalReadIntoHits, 0)
		externalReadIntoMisses := atomic.SwapInt64(&fs.stats.externalReadIntoMisses, 0)
		externalReadIntoBytes := atomic.SwapInt64(&fs.stats.externalReadIntoBytes, 0)
		externalStreamAttempts := atomic.SwapInt64(&fs.stats.externalStreamAttempts, 0)
		externalStreamHits := atomic.SwapInt64(&fs.stats.externalStreamHits, 0)
		externalStreamMisses := atomic.SwapInt64(&fs.stats.externalStreamMisses, 0)
		externalStreamBytes := atomic.SwapInt64(&fs.stats.externalStreamBytes, 0)
		externalUnaryAttempts := atomic.SwapInt64(&fs.stats.externalUnaryAttempts, 0)
		externalUnaryHits := atomic.SwapInt64(&fs.stats.externalUnaryHits, 0)
		externalUnaryMisses := atomic.SwapInt64(&fs.stats.externalUnaryMisses, 0)
		externalUnaryBytes := atomic.SwapInt64(&fs.stats.externalUnaryBytes, 0)
		cacheEventsQueued := atomic.SwapInt64(&fs.stats.cacheEventsQueued, 0)
		cacheEventsStarted := atomic.SwapInt64(&fs.stats.cacheEventsStarted, 0)
		cacheEventsSuccess := atomic.SwapInt64(&fs.stats.cacheEventsSuccess, 0)
		cacheEventsErrors := atomic.SwapInt64(&fs.stats.cacheEventsErrors, 0)
		cacheEventsMismatch := atomic.SwapInt64(&fs.stats.cacheEventsMismatch, 0)
		cacheEventsDropped := atomic.SwapInt64(&fs.stats.cacheEventsDropped, 0)
		cacheEventsBytes := atomic.SwapInt64(&fs.stats.cacheEventsBytes, 0)
		cloudReadRequests := atomic.SwapInt64(&fs.stats.cloudReadRequests, 0)
		cloudReadBytes := atomic.SwapInt64(&fs.stats.cloudReadBytes, 0)
		writes := atomic.SwapInt64(&fs.stats.writes, 0)
		flushes := atomic.SwapInt64(&fs.stats.flushes, 0)
		metadataReads := atomic.SwapInt64(&fs.stats.metadataReads, 0)
		metadataWrites := atomic.SwapInt64(&fs.stats.metadataWrites, 0)
		noops := atomic.SwapInt64(&fs.stats.noops, 0)
		evicts := atomic.SwapInt64(&fs.stats.evicts, 0)
		fs.mu.RLock()
		inodeCount := len(fs.inodes)
		fs.mu.RUnlock()
		fs.stats.ts = now
		readsOr1 := float64(reads)
		if reads == 0 {
			readsOr1 = 1
		}
		hasActivity := reads+readBytes+readSlow+readErrors+readHandlerCount+readCallbackCount+readBufferHits+externalPageAttempts+externalPageHits+externalPageMisses+externalPageMmapFailures+externalReadIntoAttempts+externalStreamAttempts+externalUnaryAttempts+cacheEventsQueued+cacheEventsStarted+cacheEventsSuccess+cacheEventsErrors+cacheEventsMismatch+cacheEventsDropped+cloudReadRequests+writes+flushes+metadataReads+metadataWrites+noops+evicts > 0
		if !hasActivity {
			continue
		}

		log.Debugf(
			"I/O: %.2f read/s, %.2f MiB/s, %.2f %% hits, %.2f write/s; metadata: %.2f read/s, %.2f write/s, %.2f noop/s, %v alive, %.2f evict/s; %.2f flush/s",
			float64(reads)/d,
			float64(readBytes)/(1024*1024)/d,
			float64(readHits)/readsOr1*100,
			float64(writes)/d,
			float64(metadataReads)/d,
			float64(metadataWrites)/d,
			float64(noops)/d,
			inodeCount,
			float64(evicts)/d,
			float64(flushes)/d,
		)
		if readSlow+readErrors+readBufferHits+readHandlerCount+readCallbackCount+externalPageAttempts+externalReadIntoAttempts+externalStreamAttempts+externalUnaryAttempts+cacheEventsQueued+cacheEventsStarted+cacheEventsSuccess+cacheEventsErrors+cacheEventsMismatch+cacheEventsDropped+cloudReadRequests > 0 {
			log.Debugf(
				"geesefs read path summary: fuse_reads=%d fuse_read=%.2fMiB slow=%d errors=%d timing(handler_count=%d handler_avg=%s callback_count=%d callback=%.2fMiB callback_avg=%s) buffer_hit=%d buffer=%.2fMiB mmap_page(attempt=%d hit=%d miss=%d mmap_fail=%d %.2fMiB lookup_count=%d lookup_avg=%s mmap_count=%d mmap_avg=%s) read_into(attempt=%d hit=%d miss=%d %.2fMiB) stream(attempt=%d hit=%d miss=%d %.2fMiB) unary(attempt=%d hit=%d miss=%d %.2fMiB) cache_event(queued=%d started=%d ok=%d err=%d mismatch=%d dropped=%d %.2fMiB) cloud(req=%d %.2fMiB)",
				reads,
				float64(readBytes)/(1024*1024),
				readSlow,
				readErrors,
				readHandlerCount,
				avgDuration(readHandlerNanos, readHandlerCount),
				readCallbackCount,
				float64(readCallbackBytes)/(1024*1024),
				avgDuration(readCallbackNanos, readCallbackCount),
				readBufferHits,
				float64(readBufferBytes)/(1024*1024),
				externalPageAttempts,
				externalPageHits,
				externalPageMisses,
				externalPageMmapFailures,
				float64(externalPageBytes)/(1024*1024),
				externalPageLookupCount,
				avgDuration(externalPageLookupNanos, externalPageLookupCount),
				externalPageMmapCount,
				avgDuration(externalPageMmapNanos, externalPageMmapCount),
				externalReadIntoAttempts,
				externalReadIntoHits,
				externalReadIntoMisses,
				float64(externalReadIntoBytes)/(1024*1024),
				externalStreamAttempts,
				externalStreamHits,
				externalStreamMisses,
				float64(externalStreamBytes)/(1024*1024),
				externalUnaryAttempts,
				externalUnaryHits,
				externalUnaryMisses,
				float64(externalUnaryBytes)/(1024*1024),
				cacheEventsQueued,
				cacheEventsStarted,
				cacheEventsSuccess,
				cacheEventsErrors,
				cacheEventsMismatch,
				cacheEventsDropped,
				float64(cacheEventsBytes)/(1024*1024),
				cloudReadRequests,
				float64(cloudReadBytes)/(1024*1024),
			)
		}
	}
}

// Close unneeded cache FDs
func (fs *Goofys) FDCloser() {
	for atomic.LoadInt32(&fs.shutdown) == 0 {
		fs.diskFdQueue.CloseExtra()
	}
}

// Try to reclaim some clean buffers
func (fs *Goofys) FreeSomeCleanBuffers(origSize int64) (int64, bool) {
	freed := int64(0)
	// Free at least 5 MB
	size := origSize
	if size < 5*1024*1024 {
		size = 5 * 1024 * 1024
	}
	var inode *Inode
	var cleanEnd, cleanQueueID uint64
	for freed < size {
		inode, cleanEnd, cleanQueueID = fs.cleanQueue.NextClean(cleanQueueID)
		if cleanQueueID == 0 {
			break
		}
		inode.mu.Lock()
		toFs := -1
		buf := inode.buffers.Get(cleanEnd)
		// Never evict buffers flushed in an incomplete (last) part
		if buf != nil && (buf.state == BUF_CLEAN || buf.state == BUF_FLUSHED_FULL) &&
			buf.ptr != nil && !inode.IsRangeLocked(buf.offset, buf.length, false) {
			fs.tryEvictToDisk(inode, buf, &toFs)
			if buf.state == BUF_FLUSHED_FULL && !buf.onDisk {
				inode.mu.Unlock()
				continue
			}
			allocated, _ := inode.buffers.EvictFromMemory(buf)
			if allocated != 0 {
				fs.bufferPool.UseUnlocked(allocated, false)
				freed -= allocated
			}
		}
		inode.mu.Unlock()
		if freed >= size {
			break
		}
	}
	haveDirty := fs.inodeQueue.Size() > 0
	if freed < origSize && haveDirty {
		fs.bufferPool.mu.Unlock()
		atomic.AddInt32(&fs.wantFree, 1)
		fs.WakeupFlusherAndWait(true)
		atomic.AddInt32(&fs.wantFree, -1)
		fs.bufferPool.mu.Lock()
	}
	return freed, haveDirty
}

// FIXME: Implement disk cache size limit, add another btree.Map-based
// "LRU" queue to delete old files from the disk.
func (fs *Goofys) tryEvictToDisk(inode *Inode, buf *FileBuffer, toFs *int) {
	if fs.flags.CachePath != "" && !buf.onDisk {
		if *toFs == -1 {
			*toFs = 1
		}
		if *toFs > 0 {
			// Evict to disk
			err := inode.OpenCacheFD()
			if err != nil {
				*toFs = 0
			} else {
				_, err := inode.DiskCacheFD.WriteAt(buf.data, int64(buf.offset))
				if err != nil {
					*toFs = 0
					log.Errorf("Couldn't write %v bytes at offset %v to %v: %v",
						len(buf.data), buf.offset, fs.flags.CachePath+"/"+inode.FullName(), err)
				} else {
					buf.onDisk = true
				}
			}
		}
	}
}

func (fs *Goofys) WakeupFlusherAndWait(wait bool) {
	fs.flusherMu.Lock()
	fs.flushPending = 1
	fs.flusherCond.Broadcast()
	// Note: waiting here is not reliable as we might wake up on someone else's signal
	// The caller should implement their own synchronization if they need to wait
	// for a specific flush to complete
	fs.flusherMu.Unlock()
}

func (fs *Goofys) WakeupFlusher() {
	fs.WakeupFlusherAndWait(false)
}

func (fs *Goofys) ScheduleRetryFlush() {
	if atomic.CompareAndSwapInt32(&fs.flushRetrySet, 0, 1) {
		time.AfterFunc(fs.flags.RetryInterval, func() {
			atomic.StoreInt32(&fs.flushRetrySet, 0)
			// Wakeup flusher after retry interval
			fs.WakeupFlusher()
		})
	}
}

// Flusher goroutine.
// Overall algorithm:
//  1. File opened => reads and writes just populate cache
//  2. File closed => flush it
//     Created or fully overwritten =>
//     => Less than 5 MB => upload in a single part
//     => More than 5 MB => upload using multipart
//     Updated => CURRENTLY:
//     => Less than 5 MB => upload in a single part
//     => More than 5 MB => update using multipart copy
//     Also we can't update less than 5 MB because it's the minimal part size
//  3. Fsync triggered => intermediate full flush (same algorithm)
//  4. Dirty memory limit reached => without on-disk cache we have to flush the whole object.
//     With on-disk cache we can unload some dirty buffers to disk.
func (fs *Goofys) Flusher() {
	var inodeID, nextQueueID uint64
	priority := 1
	for atomic.LoadInt32(&fs.shutdown) == 0 {
		fs.flusherMu.Lock()
		// Wait only if there's no work to do
		for fs.flushPending == 0 && atomic.LoadInt32(&fs.shutdown) == 0 {
			fs.flusherCond.Wait()
		}
		fs.flushPending = 0
		fs.flusherMu.Unlock()
		attempts := 1
		if priority > 1 || priority == 1 && nextQueueID != 0 {
			attempts = 2
		}
		curPriorityOk := false
		for i := 1; i <= priority; i++ {
			curPriorityOk = curPriorityOk || atomic.LoadInt64(&fs.flushPriorities[priority]) > 0
		}
		for attempts > 0 && atomic.LoadInt64(&fs.activeFlushers) < fs.flags.MaxFlushers {
			inodeID, nextQueueID = fs.inodeQueue.Next(nextQueueID)
			if inodeID == 0 {
				if curPriorityOk {
					break
				}
				priority++
				if priority > MAX_FLUSH_PRIORITY {
					attempts--
					priority = 1
				}
				if curPriorityOk {
					break
				}
			} else {
				if atomic.CompareAndSwapUint64(&fs.hasNewWrites, 1, 0) {
					// restart from the beginning
					inodeID, nextQueueID = 0, 0
					priority = 1
					attempts = 1
					curPriorityOk = atomic.LoadInt64(&fs.flushPriorities[1]) > 0
					continue
				}
				fs.mu.RLock()
				inode := fs.inodes[fuseops.InodeID(inodeID)]
				fs.mu.RUnlock()
				started := false
				if inode != nil {
					started = inode.TryFlush(priority)
				}
				curPriorityOk = curPriorityOk || started
			}
		}
	}
}

func (fs *Goofys) EvictEntry(id fuseops.InodeID) bool {
	fs.mu.RLock()
	childTmp := fs.inodes[id]
	fs.mu.RUnlock()
	if childTmp == nil ||
		childTmp.Id == fuseops.RootInodeID ||
		atomic.LoadInt32(&childTmp.fileHandles) > 0 ||
		atomic.LoadInt32(&childTmp.CacheState) > ST_DEAD ||
		childTmp.isDir() && atomic.LoadInt64(&childTmp.dir.ModifiedChildren) > 0 {
		return false
	}
	if !childTmp.mu.TryLock() {
		return false
	}
	// We CAN evict inodes which are still referenced by the kernel,
	// but only if they're expired!
	if childTmp.ExpireTime.After(time.Now()) {
		childTmp.mu.Unlock()
		return false
	}
	tmpParent := childTmp.Parent
	// Respect locking order: parent before child, inode before fs
	childTmp.mu.Unlock()
	if !tmpParent.mu.TryLock() {
		return false
	}
	if !childTmp.mu.TryLock() {
		tmpParent.mu.Unlock()
		return false
	}
	if childTmp.Parent != tmpParent ||
		atomic.LoadInt32(&tmpParent.fileHandles) > 0 {
		childTmp.mu.Unlock()
		tmpParent.mu.Unlock()
		return false
	}
	found := tmpParent.findChildUnlocked(childTmp.Name)
	if found == childTmp {
		tmpParent.removeChildUnlocked(childTmp)
		// Mark directory listing as unfinished
		tmpParent.dir.DirTime = time.Time{}
		tmpParent.dir.forgetDuringList = true
	}
	childTmp.resetCache()
	childTmp.SetCacheState(ST_DEAD)
	// Drop inode
	fs.mu.Lock()
	childTmp.resetExpireTime()
	delete(fs.inodes, childTmp.Id)
	fs.forgotCnt += 1
	fs.mu.Unlock()
	childTmp.mu.Unlock()
	tmpParent.mu.Unlock()
	return true
}

func (fs *Goofys) StagedFileFlusher() {
	ticker := time.NewTicker(fs.flags.StagedWriteFlushInterval)
	defer ticker.Stop()

	sem := make(chan struct{}, fs.flags.StagedWriteFlushConcurrency)

	for {
		select {
		case <-ticker.C:
			stagedFilesExist := false
			fs.stagedFiles.Range(func(key, value interface{}) bool {
				stagedFilesExist = true

				inode := value.(*Inode)
				if inode.StagedFile != nil && inode.StagedFile.ReadyToFlush() {
					select {
					case sem <- struct{}{}:
						log.Debugf("StagedFileFlusher: queued to flush %s", inode.FullName())

						go func(inode *Inode) {
							defer func() { <-sem }()
							fs.flushStagedFile(inode)
						}(inode)
					default:
						// Concurrency limit reached, do not start more
						log.Debugf("StagedFileFlusher: concurrency limit reached, skipping %s", inode.FullName())
					}
				}
				return true
			})

			if stagedFilesExist {
				fs.WakeupFlusher()
			}
		case <-fs.shutdownCh:
			return
		}
	}
}

func (fs *Goofys) flushStagedFile(inode *Inode) error {
	return fs.flushStagedFileDirect(inode)
}

func (fs *Goofys) flushStagedFileBuffered(inode *Inode) (err error) {
	inode.mu.Lock()

	stagedFile := inode.StagedFile
	if stagedFile == nil {
		inode.fs.stagedFiles.Delete(inode.Id)
		inode.mu.Unlock()
		return nil
	}

	stagedFile.mu.Lock()
	if stagedFile.flushing {
		stagedFile.mu.Unlock()
		inode.mu.Unlock()
		return nil
	}

	if inode.flushError != nil {
		if time.Since(inode.flushErrorTime) < inode.fs.flags.RetryInterval {
			err = inode.flushError
			stagedFile.mu.Unlock()
			inode.mu.Unlock()
			return err
		}
		inode.flushError = nil
	}

	totalSize := int64(inode.Attributes.Size)

	stagedFile.flushing = true
	stagedFile.shouldFlush = true
	stagedFile.mu.Unlock()

	inode.mu.Unlock()

	defer func() {
		if err != nil {
			stagedFile.ResetFlushForRetry()
			fs.WakeupFlusher()
		}
	}()

	fs.WakeupFlusher()
	offset := int64(0)

	for offset < totalSize {
		chunkSize := fs.flags.StagedWriteFlushSize
		if totalSize-offset < int64(fs.flags.StagedWriteFlushSize) {
			chunkSize = uint64(totalSize - offset)
		}

		// Lock this part's range while we're reading / flushing it
		inode.mu.Lock()
		inode.LockRange(uint64(offset), chunkSize, true)
		inode.mu.Unlock()

		var n int
		var readErr error
		{
			stagedFile.mu.Lock()
			if stagedFile.FD == nil {
				stagedFile.mu.Unlock()
				inode.mu.Lock()
				inode.UnlockRange(uint64(offset), chunkSize, true)
				inode.mu.Unlock()
				err = syscall.EAGAIN
				return err
			}
			buf := make([]byte, chunkSize)
			n, readErr = stagedFile.FD.ReadAt(buf, offset)
			stagedFile.mu.Unlock()

			if readErr != nil && readErr != io.EOF {
				log.Errorf("Error reading from staged file: %v", readErr)
				inode.mu.Lock()
				inode.UnlockRange(uint64(offset), chunkSize, true)
				inode.mu.Unlock()
				err = readErr
				return err
			}

			if n == 0 {
				inode.mu.Lock()
				inode.UnlockRange(uint64(offset), chunkSize, true)
				inode.mu.Unlock()
				if offset < totalSize {
					err = io.ErrUnexpectedEOF
				}
				return err
			}

			// Check if readers want to interrupt this flush
			inode.mu.Lock()
			canProceed := inode.checkPauseWritersInterruptible()
			if !canProceed {
				// Readers are waiting - yield to them and retry later
				log.Debugf("Staged file flush interrupted by readers for %s, will retry", inode.FullName())
				inode.UnlockRange(uint64(offset), chunkSize, true)

				// Reset flushing state so it can be retried
				stagedFile.mu.Lock()
				stagedFile.flushing = false
				stagedFile.mu.Unlock()

				inode.mu.Unlock()
				return nil // Exit and let readers proceed, staged file will be retried later
			}
			inode.mu.Unlock()

			copyData := len(buf) < cap(buf)-4096
			err = stagedFile.FH.WriteFile(offset, buf[:n], copyData)
			if err != nil {
				log.Errorf("Error writing staged data data for flush: %v", err)
				inode.mu.Lock()
				inode.UnlockRange(uint64(offset), chunkSize, true)
				inode.mu.Unlock()
				return err
			}
		}

		// Unlock this part's range after it's ready to flush
		inode.mu.Lock()
		inode.UnlockRange(uint64(offset), chunkSize, true)
		inode.mu.Unlock()

		offset += int64(n)
		if readErr == io.EOF {
			break
		}
	}

	err = inode.SyncFile()
	if err != nil {
		log.Errorf("Error syncing staged file: %v", err)
		return err
	}

	localPath, hasLocalPath := stagedFile.Path()
	preserveForCache := false
	if fs.flags.CacheThroughModeEnabled && fs.flags.ExternalCacheClient != nil && hasLocalPath {
		preserveForCache = fs.CacheFileInExternalCacheFromSource(inode, localPath, true)
		if preserveForCache {
			stagedFile.PreserveForCache(localPath)
		}
	}

	var (
		hash []byte
		size uint64
	)
	inode.mu.Lock()
	if inode.userMetadata != nil {
		hash = inode.userMetadata[fs.flags.HashAttr]
	}
	size = inode.Attributes.Size
	if inode.StagedFile == stagedFile {
		inode.StagedFile = nil
	}
	inode.fs.stagedFiles.Delete(inode.Id)
	inode.mu.Unlock()

	stagedFile.Cleanup()

	if fs.flags.EventCallback != nil {
		fs.flags.EventCallback(cfg.EventStagedFileUploaded, map[string]interface{}{
			"inode": stagedFile.FH.inode.FullName(),
			"hash":  hash,
			"size":  size,
		})
	}

	return nil
}

func (fs *Goofys) flushStagedFileDirect(inode *Inode) (err error) {
	inode.mu.Lock()

	stagedFile := inode.StagedFile
	if stagedFile == nil {
		inode.fs.stagedFiles.Delete(inode.Id)
		inode.mu.Unlock()
		return nil
	}

	stagedFile.mu.Lock()
	if stagedFile.flushing {
		stagedFile.mu.Unlock()
		inode.mu.Unlock()
		return nil
	}
	if stagedFile.FD == nil {
		stagedFile.mu.Unlock()
		inode.fs.stagedFiles.Delete(inode.Id)
		inode.mu.Unlock()
		return nil
	}

	localPath := stagedFile.FD.Name()
	if syncErr := stagedFile.FD.Sync(); syncErr != nil {
		stagedFile.mu.Unlock()
		inode.mu.Unlock()
		return syncErr
	}
	stagedFile.flushing = true
	stagedFile.shouldFlush = true
	stagedFile.mu.Unlock()

	totalSize := inode.Attributes.Size
	cloud, key := inode.cloud()
	if inode.oldParent != nil {
		_, key = inode.oldParent.cloud()
		key = appendChildName(key, inode.oldName)
	}
	contentType := inode.fs.flags.GetMimeType(inode.FullName())
	inode.mu.Unlock()

	defer func() {
		if err != nil {
			stagedFile.ResetFlushForRetry()
			fs.WakeupFlusher()
		}
	}()

	var hash []byte
	if fs.flags.HashAttr != "" && totalSize >= fs.flags.MinFileSizeForHashKB*1024 {
		hashString, hashErr := hashLocalFile(localPath)
		if hashErr != nil {
			return hashErr
		}
		hash = []byte(hashString)
	}

	inode.mu.Lock()
	if hash != nil {
		if inode.userMetadata == nil {
			inode.userMetadata = make(map[string][]byte)
		}
		inode.userMetadata[fs.flags.HashAttr] = hash
	}
	metadata := escapeMetadata(inode.userMetadata)
	inode.mu.Unlock()

	log.Debugf("Directly flushing staged file: inode=%s size=%d path=%s", inode.FullName(), totalSize, localPath)
	resp, err := fs.uploadStagedFileDirect(cloud, key, localPath, totalSize, contentType, metadata)
	if err != nil {
		log.Warnf("Failed direct staged file flush for %s: %v", inode.FullName(), err)
		inode.mu.Lock()
		inode.recordFlushError(err)
		inode.mu.Unlock()
		return err
	}

	inode.mu.Lock()
	inode.recordFlushError(nil)
	inode.updateFromFlush(totalSize, resp.ETag, resp.LastModified, resp.StorageClass)
	inode.userMetadataDirty = 0
	inode.hashMetadataDirty = false
	inode.hashMetadataSync = false
	inode.buffers.SetFlushedClean()
	if inode.CacheState == ST_CREATED || inode.CacheState == ST_MODIFIED {
		if !inode.isStillDirty() {
			inode.SetCacheState(ST_CACHED)
		} else {
			inode.SetCacheState(ST_MODIFIED)
		}
	}
	inode.mu.Unlock()

	preserveForCache := false
	if fs.flags.CacheThroughModeEnabled && fs.flags.ExternalCacheClient != nil {
		preserveForCache = fs.CacheFileInExternalCacheFromSource(inode, localPath, true)
		if preserveForCache {
			stagedFile.PreserveForCache(localPath)
		}
	}

	inode.mu.Lock()
	var size uint64
	size = inode.Attributes.Size
	if inode.StagedFile == stagedFile {
		inode.StagedFile = nil
	}
	inode.fs.stagedFiles.Delete(inode.Id)
	inode.mu.Unlock()

	stagedFile.Cleanup()

	if fs.flags.EventCallback != nil {
		fs.flags.EventCallback(cfg.EventStagedFileUploaded, map[string]interface{}{
			"inode": stagedFile.FH.inode.FullName(),
			"hash":  hash,
			"size":  size,
		})
	}

	log.Debugf("Direct staged file flush complete: inode=%s size=%d hash=%s preserved_for_cache=%v", inode.FullName(), totalSize, string(hash), preserveForCache)
	return nil
}

func hashLocalFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(hasher, file, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (fs *Goofys) uploadStagedFileDirect(cloud StorageBackend, key, localPath string, size uint64, contentType *string, metadata map[string]*string) (*MultipartBlobCommitOutput, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if size <= fs.flags.SinglePartMB*1024*1024 {
		resp, err := cloud.PutBlob(&PutBlobInput{
			Key:         key,
			Metadata:    metadata,
			ContentType: contentType,
			Body:        file,
			Size:        PUInt64(size),
		})
		if err != nil {
			return nil, err
		}
		return &MultipartBlobCommitOutput{
			ETag:         resp.ETag,
			LastModified: resp.LastModified,
			StorageClass: resp.StorageClass,
			RequestId:    resp.RequestId,
		}, nil
	}

	mpu, err := cloud.MultipartBlobBegin(&MultipartBlobBeginInput{
		Key:         key,
		Metadata:    metadata,
		ContentType: contentType,
	})
	if err != nil {
		return nil, err
	}

	type stagedPart struct {
		partNum uint32
		offset  uint64
		size    uint64
	}
	type stagedPartResult struct {
		partNum uint32
		partID  *string
		err     error
	}

	parts := make([]stagedPart, 0, fs.partNum(size))
	var partNum uint32
	for offset := uint64(0); offset < size; {
		_, partSize := fs.partRange(uint64(partNum))
		if remaining := size - offset; partSize > remaining {
			partSize = remaining
		}
		parts = append(parts, stagedPart{partNum: partNum + 1, offset: offset, size: partSize})
		partNum++
		offset += partSize
	}

	parallelism := int(fs.flags.MaxParallelParts)
	if parallelism <= 0 {
		parallelism = 1
	}
	if parallelism > len(parts) {
		parallelism = len(parts)
	}
	if parallelism < 1 {
		parallelism = 1
	}

	log.Debugf("Direct staged multipart upload start: key=%s size=%d parts=%d parallelism=%d", key, size, len(parts), parallelism)

	tasks := make(chan stagedPart)
	results := make(chan stagedPartResult, len(parts))
	var wg sync.WaitGroup
	for worker := 0; worker < parallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for part := range tasks {
				partStarted := time.Now()
				partReader := io.NewSectionReader(file, int64(part.offset), int64(part.size))
				partResp, partErr := cloud.MultipartBlobAdd(&MultipartBlobAddInput{
					Commit:     mpu,
					PartNumber: part.partNum,
					Body:       partReader,
					Size:       part.size,
					Offset:     part.offset,
				})
				if partErr != nil {
					log.Warnf(
						"Direct staged multipart upload part failed: key=%s part=%d offset=%d size=%d elapsed=%s err=%v",
						key,
						part.partNum,
						part.offset,
						part.size,
						time.Since(partStarted).Truncate(time.Millisecond),
						partErr,
					)
					results <- stagedPartResult{partNum: part.partNum, err: partErr}
					continue
				}
				elapsed := time.Since(partStarted)
				if elapsed > 5*time.Second || part.partNum == 1 || part.partNum == uint32(len(parts)) {
					log.Debugf(
						"Direct staged multipart upload part complete: key=%s part=%d/%d offset=%d size=%d elapsed=%s mbps=%.2f",
						key,
						part.partNum,
						len(parts),
						part.offset,
						part.size,
						elapsed.Truncate(time.Millisecond),
						float64(part.size)/(1024*1024)/(float64(elapsed)/float64(time.Second)),
					)
				}
				partID := (*string)(nil)
				if partResp != nil {
					partID = partResp.PartId
				}
				results <- stagedPartResult{partNum: part.partNum, partID: partID}
			}
		}()
	}

	go func() {
		for _, part := range parts {
			tasks <- part
		}
		close(tasks)
		wg.Wait()
		close(results)
	}()

	var uploadErr error
	for result := range results {
		if result.err != nil && uploadErr == nil {
			uploadErr = result.err
		}
		if result.err == nil && int(result.partNum)-1 < len(mpu.Parts) {
			mpu.Parts[result.partNum-1] = result.partID
		}
	}
	if uploadErr != nil {
		_, _ = cloud.MultipartBlobAbort(mpu)
		return nil, uploadErr
	}

	mpu.NumParts = uint32(len(parts))
	mpu.Size = &size
	resp, err := cloud.MultipartBlobCommit(mpu)
	if err != nil {
		_, _ = cloud.MultipartBlobAbort(mpu)
		return nil, err
	}
	return resp, nil
}

func (fs *Goofys) WaitForFlush() {
	timeout := fs.flags.StagedWriteFlushTimeout

	if fs.flags.StagedWriteModeEnabled {
		timeoutTimer := time.NewTimer(timeout)
		defer timeoutTimer.Stop()

		for {
			hasStaged := false
			startedFlush := false

			fs.stagedFiles.Range(func(key, value interface{}) bool {
				inode := value.(*Inode)

				stagedFile := inode.StagedFile
				if stagedFile != nil {
					log.Debugf("Waiting for flush to complete: inode=%s, lastWriteAt=%v, lastReadAt=%v, flushing=%v, shouldFlush=%v",
						inode.FullName(),
						stagedFile.lastWriteAt,
						stagedFile.lastReadAt,
						stagedFile.flushing,
						stagedFile.shouldFlush,
					)

					hasStaged = true
					if stagedFile.flushing {
					} else {
						// Shutdown/unmount must persist every staged file regardless
						// of the debounce window, otherwise recent writes can remain
						// only in the worker-local staging directory.
						startedFlush = true
						if err := fs.flushStagedFileDirect(inode); err != nil {
							log.Warnf("Direct staged flush failed for %s: %v", inode.FullName(), err)
						}
						return true
					}
				}

				return true
			})

			if !hasStaged {
				break
			}
			if startedFlush {
				continue
			}

			select {
			case <-timeoutTimer.C:
				log.Warnf("Flush did not complete within timeout of %v", timeout)
				return
			case <-time.After(1 * time.Second):
				// Continue waiting
			}
		}
	}
}

func (fs *Goofys) MetaEvictor() {
	retry := false
	var seen map[fuseops.InodeID]bool
	for atomic.LoadInt32(&fs.shutdown) == 0 {
		if !retry {
			select {
			case <-time.After(1 * time.Second):
			case <-fs.shutdownCh:
				return
			}
			seen = make(map[fuseops.InodeID]bool)
		}

		// Try to keep the number of cached inodes under control %)
		fs.mu.RLock()
		totalInodes := len(fs.inodes)
		toEvict := (totalInodes - fs.flags.EntryLimit) * 2
		if toEvict < 0 {
			fs.mu.RUnlock()
			retry = false
			continue
		}
		if toEvict < fs.flags.EntryLimit/100 {
			toEvict = fs.flags.EntryLimit / 100
		}
		if toEvict < 10 {
			toEvict = 10
		}
		expireUnix := time.Now().Add(-fs.flags.StatCacheTTL).Unix()
		var scan []fuseops.InodeID
		for tm, inodes := range fs.inodesByTime {
			if tm < expireUnix {
				for inode, _ := range inodes {
					if !seen[inode] {
						scan = append(scan, inode)
					}
					if len(scan) >= toEvict {
						break
					}
				}
			}
			if len(scan) >= toEvict {
				break
			}
		}
		fs.mu.RUnlock()
		evicted := 0
		for _, id := range scan {
			if fs.EvictEntry(id) {
				evicted++
			} else {
				seen[id] = true
			}
		}
		retry = len(scan) >= toEvict && totalInodes > fs.flags.EntryLimit
		atomic.AddInt64(&fs.stats.evicts, int64(evicted))
		if len(scan) > 0 {
			log.Debugf("metadata cache: alive %v, scanned %v, evicted %v", totalInodes, len(scan), evicted)
		}
	}
}

type Mount struct {
	// Mount Point relative to goofys's root mount.
	name    string
	cloud   StorageBackend
	prefix  string
	mounted bool
}

func (fs *Goofys) mount(mp *Inode, b *Mount) {
	if b.mounted {
		return
	}

	name := strings.Trim(b.name, "/")

	// create path for the mount. AttrTime is set to TIME_MAX so
	// they will never expire and be removed. But DirTime is not
	// so we will still consult the underlining cloud for listing
	// (which will then be merged with the cached result)

	for {
		idx := strings.Index(name, "/")
		if idx == -1 {
			break
		}
		dirName := name[0:idx]
		name = name[idx+1:]

		mp.mu.Lock()
		dirInode := mp.findChildUnlocked(dirName)
		if dirInode == nil {
			dirInode = NewInode(fs, mp, dirName)
			dirInode.ToDir()
			dirInode.SetAttrTime(TIME_MAX)
			dirInode.userMetadata = make(map[string][]byte)

			fs.insertInode(mp, dirInode)
		}
		mp.mu.Unlock()
		mp = dirInode
	}

	mp.mu.Lock()
	defer mp.mu.Unlock()

	prev := mp.findChildUnlocked(name)
	if prev == nil {
		mountInode := NewInode(fs, mp, name)
		mountInode.ToDir()
		mountInode.dir.cloud = b.cloud
		mountInode.dir.mountPrefix = b.prefix
		mountInode.SetAttrTime(TIME_MAX)
		mountInode.userMetadata = make(map[string][]byte)

		fs.insertInode(mp, mountInode)

		prev = mountInode
	} else {
		if !prev.isDir() {
			panic(fmt.Sprintf("inode %v is not a directory", prev.FullName()))
		}

		// This inode might have some cached data from a parent mount.
		// Clear this cache by resetting the DirTime.
		// Note: resetDirTimeRec should be called without holding the lock.
		prev.resetDirTimeRec()
		prev.mu.Lock()
		defer prev.mu.Unlock()
		prev.dir.cloud = b.cloud
		prev.dir.mountPrefix = b.prefix
		prev.SetAttrTime(TIME_MAX)

	}
	prev.addModified(1)
	fuseLog.Infof("mounted /%v", prev.FullName())
	b.mounted = true
}

func (fs *Goofys) MountAll(mounts []*Mount) {
	root := fs.getInodeOrDie(fuseops.RootInodeID)

	for _, m := range mounts {
		fs.mount(root, m)
	}
}

func (fs *Goofys) Mount(mount *Mount) {
	root := fs.getInodeOrDie(fuseops.RootInodeID)
	fs.mount(root, mount)
}

func (fs *Goofys) Unmount(mountPoint string) {
	mp := fs.getInodeOrDie(fuseops.RootInodeID)

	fuseLog.Infof("Attempting to unmount %v", mountPoint)
	path := strings.Split(strings.Trim(mountPoint, "/"), "/")
	for _, localName := range path {
		dirInode := mp.findChild(localName)
		if dirInode == nil || !dirInode.isDir() {
			fuseLog.Errorf("Failed to find directory:%v while unmounting %v. "+
				"Ignoring the unmount operation.", localName, mountPoint)
			return
		}
		mp = dirInode
	}
	mp.addModified(-1)
	mp.ResetForUnmount()
	return
}

func (fs *Goofys) RefreshInodeCache(inode *Inode) error {
	inode.mu.Lock()
	parent := inode.Parent
	parentId := fuseops.InodeID(0)
	if parent != nil {
		parentId = parent.Id
	}
	name := inode.Name
	inodeId := inode.Id
	inode.mu.Unlock()
	inode.resetDirTimeRec()
	var mappedErr error
	var notifications []interface{}
	if parent == nil {
		// For regular directories it's enough to send one invalidation
		// message, the kernel will send forgets for their children and
		// everything will be refreshed just fine.
		// But root directory is a special case: we should invalidate all
		// inodes in it ourselves. Basically this means that we have to do
		// a listing and notify the kernel about every file in the root
		// directory.
		dh := inode.OpenDir()
		dh.mu.Lock()
		for {
			en, err := dh.ReadDir()
			if err != nil {
				mappedErr = mapAwsError(err)
				break
			}
			if en == nil {
				break
			}
			if dh.lastInternalOffset >= 2 {
				// Delete notifications are sent by ReadDir() itself
				notifications = append(notifications, &fuseops.NotifyInvalEntry{
					Parent: inode.Id,
					Name:   en.Name,
				})
			}
			dh.Next(en.Name)
		}
		dh.CloseDir()
		dh.mu.Unlock()
		if fs.NotifyCallback != nil {
			fs.NotifyCallback(notifications)
		}
		return mappedErr
	}
	inode, err := parent.recheckInode(inode, name)
	mappedErr = mapAwsError(err)
	if mappedErr == syscall.ENOENT {
		notifications = append(notifications, &fuseops.NotifyDelete{
			Parent: parentId,
			Child:  inodeId,
			Name:   name,
		})
	} else {
		notifications = append(notifications, &fuseops.NotifyInvalEntry{
			Parent: parentId,
			Name:   name,
		})
	}
	if fs.NotifyCallback != nil {
		fs.NotifyCallback(notifications)
	}
	if mappedErr == syscall.ENOENT {
		// We don't mind if the file disappeared
		return nil
	}
	return mappedErr
}

// FIXME: Add similar write backoff (now it's handled by file/dir code)
func ReadBackoff(flags *cfg.FlagStorage, try func(attempt int) error) (err error) {
	interval := flags.ReadRetryInterval
	attempt := 1
	for {
		err = try(attempt)
		if err != nil {
			if shouldRetry(err) && (flags.ReadRetryAttempts < 1 || attempt < flags.ReadRetryAttempts) {
				attempt++
				time.Sleep(interval)
				interval = time.Duration(flags.ReadRetryMultiplier * float64(interval))
				if interval > flags.ReadRetryMax {
					interval = flags.ReadRetryMax
				}
			} else {
				break
			}
		} else {
			break
		}
	}
	return
}

func mapHttpError(status int) error {
	switch status {
	case 400:
		return syscall.EINVAL
	case 401:
		return syscall.EACCES
	case 403:
		return syscall.EACCES
	case 404:
		return syscall.ENOENT
	case 405:
		return syscall.ENOTSUP
	case http.StatusConflict:
		return syscall.EINTR
	case http.StatusRequestedRangeNotSatisfiable:
		return syscall.ERANGE
	case http.StatusPreconditionFailed:
		return syscall.EBUSY
	case 429:
		return syscall.EAGAIN
	case 503:
		return syscall.EAGAIN
	case 500:
		return syscall.EAGAIN
	default:
		return nil
	}
}

func mapAwsError(err error) error {
	if err == nil {
		return nil
	}

	if awsErr, ok := err.(awserr.Error); ok {
		switch awsErr.Code() {
		case "BucketRegionError":
			// don't need to log anything, we should detect region after
			return err
		case "NoSuchBucket":
			return syscall.ENXIO
		case "BucketAlreadyOwnedByYou":
			return syscall.EEXIST
		case "ConcurrentUpdatesPatchConflict", "ObjectVersionPatchConflict":
			return syscall.EBUSY
		}

		if reqErr, ok := err.(awserr.RequestFailure); ok {
			// A service error occurred
			err = mapHttpError(reqErr.StatusCode())
			if err != nil {
				return err
			} else {
				s3Log.Warnf("http=%v %v s3=%v request=%v\n",
					reqErr.StatusCode(), reqErr.Message(),
					awsErr.Code(), reqErr.RequestID())
				return reqErr
			}
		} else {
			// Generic AWS Error with Code, Message, and original error (if any)
			s3Log.Warnf("code=%v msg=%v, err=%v\n", awsErr.Code(), awsErr.Message(), awsErr.OrigErr())
			return awsErr
		}
	} else {
		return err
	}
}

func isNoSuchUploadError(err error) bool {
	if err == nil {
		return false
	}
	if awsErr, ok := err.(awserr.Error); ok {
		return awsErr.Code() == "NoSuchUpload"
	}
	return false
}

// note that this is NOT the same as url.PathEscape in golang 1.8,
// as this preserves / and url.PathEscape converts / to %2F
func pathEscape(path string) string {
	u := url.URL{Path: path}
	return u.EscapedPath()
}

// LOCKS_REQUIRED(fs.mu)
func (fs *Goofys) allocateInodeId() (id fuseops.InodeID) {
	id = fs.nextInodeID
	fs.nextInodeID++
	return
}

func expired(cache time.Time, ttl time.Duration) bool {
	now := time.Now()
	if cache.After(now) {
		return false
	}
	return !cache.Add(ttl).After(now)
}

// LOCKS_REQUIRED(parent.mu)
// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) insertInode(parent *Inode, inode *Inode) {
	if inode.Id != 0 {
		panic(fmt.Sprintf("inode id is set: %v %v", inode.Name, inode.Id))
	}
	fs.mu.Lock()
	inode.Id = fs.allocateInodeId()
	parent.insertChildUnlocked(inode)
	fs.inodes[inode.Id] = inode
	fs.mu.Unlock()
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) addInflightChange(key string) {
	fs.mu.Lock()
	fs.inflightChanges[key]++
	for _, v := range fs.inflightListings {
		v[key] = true
	}
	fs.mu.Unlock()
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) completeInflightChange(key string) {
	fs.mu.Lock()
	fs.inflightChanges[key]--
	if fs.inflightChanges[key] <= 0 {
		delete(fs.inflightChanges, key)
	}
	fs.mu.Unlock()
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) addInflightListing() int {
	fs.mu.Lock()
	fs.inflightListingId++
	id := fs.inflightListingId
	m := make(map[string]bool)
	for k, _ := range fs.inflightChanges {
		m[k] = true
	}
	fs.inflightListings[id] = m
	fs.mu.Unlock()
	return id
}

// For any listing, we forcibly exclude all objects modifications of which were
// started before the completion of the listing, but were not completed before
// the beginning of the listing.
// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) completeInflightListing(id int) map[string]bool {
	fs.mu.Lock()
	m := fs.inflightListings[id]
	delete(fs.inflightListings, id)
	fs.mu.Unlock()
	return m
}

func (fs *Goofys) SyncTree(parent *Inode) (err error) {
	if parent == nil || parent.Id == fuseops.RootInodeID {
		log.Infof("Flushing all changes")
		parent = nil
	} else {
		log.Infof("Flushing all changes under %v", parent.FullName())
	}
	fs.mu.RLock()
	inodes := make([]fuseops.InodeID, 0, len(fs.inodes))
	for id, inode := range fs.inodes {
		if parent == nil || parent.isParentOf(inode) {
			inodes = append(inodes, id)
		}
	}
	fs.mu.RUnlock()
	for i := 0; i < len(inodes); i++ {
		id := inodes[i]
		fs.mu.RLock()
		inode := fs.inodes[id]
		fs.mu.RUnlock()
		if inode != nil {
			inode.SyncFile()
		}
	}
	return
}

func (fs *Goofys) LookupParent(path string) (parent *Inode, child string, err error) {
	parts := strings.Split(path, "/")
	child = parts[len(parts)-1]
	fs.mu.RLock()
	parent = fs.inodes[fuseops.RootInodeID]
	fs.mu.RUnlock()
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] != "" {
			parent, err = parent.LookUpCached(parts[i])
			if err != nil {
				return
			}
			if !parent.isDir() {
				return nil, "", syscall.ENOTDIR
			}
			if atomic.LoadInt32(&parent.CacheState) == ST_DEAD {
				// Stale inode
				return nil, "", syscall.ESTALE
			}
		}
	}
	return
}

func (fs *Goofys) LookupPath(path string) (inode *Inode, err error) {
	parts := strings.Split(path, "/")
	fs.mu.RLock()
	inode = fs.inodes[fuseops.RootInodeID]
	fs.mu.RUnlock()
	for i := 0; i < len(parts); i++ {
		if parts[i] != "" {
			if !inode.isDir() {
				return nil, syscall.ENOTDIR
			}
			inode, err = inode.LookUpCached(parts[i])
			if err != nil {
				return
			}
			if atomic.LoadInt32(&inode.CacheState) == ST_DEAD {
				// Stale inode
				return nil, syscall.ESTALE
			}
		}
	}
	return
}
