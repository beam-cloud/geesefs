package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync/atomic"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

// lazyReadIdentity pins a staged read to the immutable object view observed by
// the metadata HEAD which preceded the read. The cache is populated before the
// hash is published, and the metadata COPY is guarded by this ETag.
type lazyReadIdentity struct {
	path       string
	objectPath string
	etag       string
	size       uint64
}

type lazyReadStage struct {
	identity   lazyReadIdentity
	file       *os.File
	path       string
	nextOffset uint64
	hasher     hash.Hash
}

func (fs *Goofys) claimLazyRead(identity lazyReadIdentity) bool {
	fs.lazyReadClaimsMu.Lock()
	defer fs.lazyReadClaimsMu.Unlock()
	if fs.lazyReadClaims == nil {
		fs.lazyReadClaims = make(map[lazyReadIdentity]struct{})
	}
	if _, exists := fs.lazyReadClaims[identity]; exists {
		return false
	}
	fs.lazyReadClaims[identity] = struct{}{}
	return true
}

func (fs *Goofys) releaseLazyReadClaim(identity lazyReadIdentity) {
	fs.lazyReadClaimsMu.Lock()
	delete(fs.lazyReadClaims, identity)
	fs.lazyReadClaimsMu.Unlock()
}

func (inode *Inode) lazyReadIdentityLocked() (lazyReadIdentity, bool) {
	fs := inode.fs
	if fs == nil || fs.flags == nil || !fs.flags.CacheThroughModeEnabled ||
		fs.flags.ExternalCacheClient == nil || fs.flags.HashAttr == "" ||
		fs.flags.StagedWritePath == "" {
		return lazyReadIdentity{}, false
	}
	if _, ok := fs.flags.ExternalCacheClient.(cfg.ContentCacheStoreLocalPath); !ok {
		return lazyReadIdentity{}, false
	}
	if inode.CacheState != ST_CACHED || inode.isDir() || inode.StagedFile != nil ||
		inode.buffers.AnyUnclean() || inode.userMetadataDirty != 0 ||
		inode.oldParent != nil || inode.renamingTo || !inode.hashMetadataChecked {
		return lazyReadIdentity{}, false
	}
	if inode.Attributes.Size < fs.flags.MinFileSizeForHashKB*1024 ||
		inode.Attributes.Size == 0 || inode.Attributes.Size != inode.knownSize ||
		inode.knownETag == "" {
		return lazyReadIdentity{}, false
	}
	if inode.userMetadata != nil && len(inode.userMetadata[fs.flags.HashAttr]) > 0 {
		return lazyReadIdentity{}, false
	}

	_, objectPath := inode.cloud()
	return lazyReadIdentity{
		path:       inode.FullName(),
		objectPath: objectPath,
		etag:       inode.knownETag,
		size:       inode.knownSize,
	}, true
}

func (inode *Inode) matchesLazyReadIdentityLocked(identity lazyReadIdentity) bool {
	current, ok := inode.lazyReadIdentityLocked()
	return ok && current == identity
}

func (fh *FileHandle) newLazyReadStage(identity lazyReadIdentity) (*lazyReadStage, error) {
	dir := fh.inode.fs.flags.StagedWritePath
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, ".geesefs-lazy-read-*")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return &lazyReadStage{
		identity: identity,
		file:     file,
		path:     file.Name(),
		hasher:   sha256.New(),
	}, nil
}

// recordLazyRead is deliberately fail-open. It only observes successful bytes
// already selected for the caller; staging failures never change the read
// result. Only a contiguous offset-zero read can become cache metadata.
func (fh *FileHandle) recordLazyRead(offset, requested uint64, data [][]byte, bytesRead int, readErr error) {
	fh.lazyReadMu.Lock()
	defer fh.lazyReadMu.Unlock()

	if fh.lazyReadDisabled {
		return
	}
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil {
		fh.abandonLazyReadLocked("foreground read failed", readErr)
		return
	}
	if bytesRead == 0 {
		if requested > 0 && fh.lazyReadStage != nil && fh.lazyReadStage.nextOffset < fh.lazyReadStage.identity.size {
			fh.abandonLazyReadLocked("foreground read ended before the complete object was observed", nil)
		}
		return
	}
	if bytesRead < 0 {
		fh.abandonLazyReadLocked("foreground read returned a negative byte count", nil)
		return
	}

	if fh.lazyReadStage == nil {
		if offset != 0 {
			fh.lazyReadDisabled = true
			return
		}
		fh.inode.mu.Lock()
		identity, ok := fh.inode.lazyReadIdentityLocked()
		fh.inode.mu.Unlock()
		if !ok {
			return
		}
		if !fh.inode.fs.claimLazyRead(identity) {
			fh.lazyReadDisabled = true
			return
		}
		stage, err := fh.newLazyReadStage(identity)
		if err != nil {
			fh.inode.fs.releaseLazyReadClaim(identity)
			fh.lazyReadDisabled = true
			log.Warnf("geesefs lazy read staging unavailable: path=%q size=%d err=%v", identity.path, identity.size, err)
			return
		}
		fh.lazyReadStage = stage
		log.Debugf("geesefs lazy read staging started: path=%q etag=%q size=%d local_source=%q", identity.path, identity.etag, identity.size, stage.path)
	}

	stage := fh.lazyReadStage
	if offset != stage.nextOffset || uint64(bytesRead) > stage.identity.size-stage.nextOffset {
		fh.abandonLazyReadLocked(fmt.Sprintf("non-contiguous read: expected_offset=%d actual_offset=%d bytes=%d", stage.nextOffset, offset, bytesRead), nil)
		return
	}

	remaining := bytesRead
	for _, chunk := range data {
		if remaining == 0 {
			break
		}
		writeSize := len(chunk)
		if writeSize > remaining {
			writeSize = remaining
		}
		if writeSize == 0 {
			continue
		}
		n, err := stage.file.Write(chunk[:writeSize])
		if err != nil || n != writeSize {
			if err == nil {
				err = fmt.Errorf("short local write: wrote %d of %d bytes", n, writeSize)
			}
			fh.abandonLazyReadLocked("failed to stage foreground bytes", err)
			return
		}
		_, _ = stage.hasher.Write(chunk[:writeSize])
		remaining -= writeSize
	}
	if remaining != 0 {
		fh.abandonLazyReadLocked(fmt.Sprintf("read buffers were %d bytes short", remaining), nil)
		return
	}

	stage.nextOffset += uint64(bytesRead)
	if stage.nextOffset != stage.identity.size {
		return
	}
	if err := stage.file.Close(); err != nil {
		stage.file = nil
		fh.abandonLazyReadLocked("failed to close completed staging file", err)
		return
	}
	stage.file = nil
	hashString := hex.EncodeToString(stage.hasher.Sum(nil))
	fh.lazyReadStage = nil
	fh.lazyReadDisabled = true

	log.Debugf("geesefs lazy read staging complete: path=%q etag=%q size=%d hash=%q local_source=%q", stage.identity.path, stage.identity.etag, stage.identity.size, hashString, stage.path)
	go fh.inode.fs.finishLazyReadStage(fh.inode, stage, hashString)
}

func (fh *FileHandle) abandonLazyRead(reason string, err error) {
	fh.lazyReadMu.Lock()
	defer fh.lazyReadMu.Unlock()
	fh.abandonLazyReadLocked(reason, err)
}

// LOCKS_REQUIRED(fh.lazyReadMu)
func (fh *FileHandle) abandonLazyReadLocked(reason string, err error) {
	stage := fh.lazyReadStage
	fh.lazyReadStage = nil
	fh.lazyReadDisabled = true
	if stage == nil {
		return
	}
	if stage.file != nil {
		_ = stage.file.Close()
		stage.file = nil
	}
	if removeErr := os.Remove(stage.path); removeErr != nil && !os.IsNotExist(removeErr) {
		log.Warnf("geesefs lazy read staging cleanup failed: path=%q local_source=%q err=%v", stage.identity.path, stage.path, removeErr)
	}
	fh.inode.fs.releaseLazyReadClaim(stage.identity)
	if err != nil {
		log.Debugf("geesefs lazy read staging abandoned: path=%q reason=%q err=%v", stage.identity.path, reason, err)
	} else {
		log.Debugf("geesefs lazy read staging abandoned: path=%q reason=%q", stage.identity.path, reason)
	}
}

func (fs *Goofys) finishLazyReadStage(inode *Inode, stage *lazyReadStage, hashString string) {
	removeLocal := true
	releaseClaim := true
	defer func() {
		if removeLocal {
			if err := os.Remove(stage.path); err != nil && !os.IsNotExist(err) {
				log.Warnf("geesefs lazy read completed source cleanup failed: path=%q local_source=%q err=%v", stage.identity.path, stage.path, err)
			}
		}
		if releaseClaim {
			fs.releaseLazyReadClaim(stage.identity)
		}
	}()

	inode.mu.Lock()
	if !inode.matchesLazyReadIdentityLocked(stage.identity) {
		inode.mu.Unlock()
		log.Debugf("geesefs lazy read identity changed before revalidation: path=%q etag=%q size=%d", stage.identity.path, stage.identity.etag, stage.identity.size)
		return
	}
	cloud, currentObjectPath := inode.cloud()
	inode.mu.Unlock()
	if currentObjectPath != stage.identity.objectPath {
		return
	}

	head, err := cloud.HeadBlob(&HeadBlobInput{Key: stage.identity.objectPath})
	if err != nil {
		log.Debugf("geesefs lazy read identity revalidation failed: path=%q err=%v", stage.identity.path, err)
		return
	}
	if head.ETag == nil || *head.ETag != stage.identity.etag || head.Size != stage.identity.size {
		log.Debugf("geesefs lazy read identity changed after read: path=%q expected_etag=%q actual_etag=%q expected_size=%d actual_size=%d", stage.identity.path, stage.identity.etag, NilStr(head.ETag), stage.identity.size, head.Size)
		return
	}
	remoteMetadata := unescapeMetadata(head.Metadata)
	if remoteHash := string(remoteMetadata[fs.flags.HashAttr]); remoteHash != "" {
		if remoteHash == hashString {
			inode.mu.Lock()
			if inode.matchesLazyReadIdentityLocked(stage.identity) {
				inode.setMetadata(head.Metadata)
				inode.hashMetadataChecked = true
			}
			inode.mu.Unlock()
			log.Debugf("geesefs lazy read hash already published during revalidation: path=%q hash=%q", stage.identity.path, remoteHash)
		} else {
			log.Warnf("geesefs lazy read remote hash mismatch during revalidation: path=%q computed=%q remote=%q", stage.identity.path, hashString, remoteHash)
		}
		return
	}

	inode.mu.Lock()
	identityMatches := inode.matchesLazyReadIdentityLocked(stage.identity)
	inode.mu.Unlock()
	if !identityMatches {
		return
	}

	reservedCacheStatus := fs.reserveExternalCacheStore(inode, hashString)
	event := cacheEvent{
		path:             stage.identity.path,
		size:             stage.identity.size,
		hash:             hashString,
		inode:            inode,
		localSourcePath:  stage.path,
		removeLocalAfter: true,
		lazyReadIdentity: &stage.identity,
		skipCacheStatus:  !reservedCacheStatus,
	}
	select {
	case fs.cacheEventChan <- event:
		removeLocal = false
		releaseClaim = false
		atomic.AddInt64(&fs.stats.cacheEventsQueued, 1)
		log.Debugf("geesefs lazy read cache event queued: path=%q hash=%q size=%d local_source=%q", event.path, event.hash, event.size, event.localSourcePath)
	default:
		atomic.AddInt64(&fs.stats.cacheEventsDropped, 1)
		fs.clearCacheEventStatus(event)
		log.Warnf("geesefs lazy read cache event queue is full: path=%q hash=%q", event.path, event.hash)
	}
}

func (inode *Inode) publishLazyReadHash(identity lazyReadIdentity, hashString string) bool {
	inode.mu.Lock()
	defer inode.mu.Unlock()

	if !inode.matchesLazyReadIdentityLocked(identity) {
		log.Debugf("geesefs lazy read metadata publish skipped after identity change: path=%q hash=%q", identity.path, hashString)
		return false
	}
	if inode.userMetadata == nil {
		inode.userMetadata = make(map[string][]byte)
	}
	if existing := string(inode.userMetadata[inode.fs.flags.HashAttr]); existing != "" {
		return existing == hashString
	}

	inode.userMetadata[inode.fs.flags.HashAttr] = []byte(hashString)
	inode.hashMetadataChecked = true
	inode.hashMetadataDirty = true
	inode.sendHashUpdateMeta()
	return true
}
