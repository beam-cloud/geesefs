package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

const readThroughMaterializationTimeout = 10 * time.Minute

// readThroughIdentity pins materialization to the object version observed by
// the HEAD which preceded the read.
type readThroughIdentity struct {
	path       string
	objectPath string
	etag       string
	size       uint64
}

type readThroughMaterialization struct {
	done chan struct{}
	hash string
	err  error
}

func (inode *Inode) readThroughIdentityLocked() (readThroughIdentity, bool) {
	fs := inode.fs
	if fs == nil || fs.flags == nil || !fs.flags.CacheThroughModeEnabled ||
		fs.flags.ExternalCacheClient == nil || fs.flags.HashAttr == "" {
		return readThroughIdentity{}, false
	}
	if inode.CacheState != ST_CACHED || inode.isDir() || inode.StagedFile != nil ||
		inode.buffers.AnyUnclean() || inode.userMetadataDirty != 0 ||
		inode.oldParent != nil || inode.renamingTo || !inode.hashMetadataChecked {
		return readThroughIdentity{}, false
	}
	if inode.Attributes.Size < fs.flags.MinFileSizeForHashKB*1024 ||
		inode.Attributes.Size == 0 || inode.Attributes.Size != inode.knownSize ||
		inode.knownETag == "" {
		return readThroughIdentity{}, false
	}
	if inode.userMetadata != nil && len(inode.userMetadata[fs.flags.HashAttr]) > 0 {
		return readThroughIdentity{}, false
	}

	_, objectPath := inode.cloud()
	return readThroughIdentity{
		path:       inode.FullName(),
		objectPath: objectPath,
		etag:       inode.knownETag,
		size:       inode.knownSize,
	}, true
}

func (inode *Inode) matchesReadThroughIdentityLocked(identity readThroughIdentity) bool {
	current, ok := inode.readThroughIdentityLocked()
	return ok && current == identity
}

func (fs *Goofys) acquireReadThrough(identity readThroughIdentity) (*readThroughMaterialization, bool) {
	fs.readThroughMu.Lock()
	defer fs.readThroughMu.Unlock()
	if fs.readThroughMaterializations == nil {
		fs.readThroughMaterializations = make(map[readThroughIdentity]*readThroughMaterialization)
	}
	if materialization := fs.readThroughMaterializations[identity]; materialization != nil {
		return materialization, false
	}
	materialization := &readThroughMaterialization{done: make(chan struct{})}
	fs.readThroughMaterializations[identity] = materialization
	return materialization, true
}

func (fs *Goofys) finishReadThrough(identity readThroughIdentity, materialization *readThroughMaterialization, hash string, err error) {
	fs.readThroughMu.Lock()
	materialization.hash = hash
	materialization.err = err
	delete(fs.readThroughMaterializations, identity)
	close(materialization.done)
	fs.readThroughMu.Unlock()
}

func (fh *FileHandle) materializeReadThrough(offset uint64) {
	if offset != 0 {
		return
	}

	fh.inode.mu.Lock()
	identity, ok := fh.inode.readThroughIdentityLocked()
	fh.inode.mu.Unlock()
	if !ok {
		return
	}

	materialization, owner := fh.inode.fs.acquireReadThrough(identity)
	if owner {
		hash, err := fh.inode.fs.materializeReadThrough(fh.inode, identity)
		fh.inode.fs.finishReadThrough(identity, materialization, hash, err)
	} else {
		<-materialization.done
	}

	if materialization.err != nil {
		if owner {
			log.Warnf("geesefs read-through materialization failed: path=%q size=%d err=%v", identity.path, identity.size, materialization.err)
		}
		return
	}
	if !owner {
		fh.inode.installReadThroughHash(identity, materialization.hash)
	}
}

func (fs *Goofys) materializeReadThrough(inode *Inode, identity readThroughIdentity) (string, error) {
	s3, ok := fs.flags.Backend.(*cfg.S3Config)
	if !ok {
		return "", fmt.Errorf("backend does not support cache materialization")
	}

	routingKey := readThroughRoutingKey(fs.bucket, identity)
	started := time.Now()
	log.Infof("geesefs read-through materialization started: path=%q size=%d", identity.path, identity.size)
	source := struct {
		Path        string
		CachePath   string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}{
		Path:        identity.objectPath,
		CachePath:   "/geesefs-read-through/" + routingKey[len("geesefs-read-through:"):],
		BucketName:  fs.bucket,
		Region:      s3.Region,
		EndpointURL: fs.flags.Endpoint,
		AccessKey:   s3.AccessKey,
		SecretKey:   s3.SecretKey,
	}
	opts := struct {
		RoutingKey string
		Lock       bool
	}{RoutingKey: routingKey, Lock: true}

	var hash string
	var err error
	if localCache, ok := fs.flags.ExternalCacheClient.(cfg.ContentCacheMaterializeS3Local); ok {
		parent := fs.ctx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, readThroughMaterializationTimeout)
		hash, err = localCache.MaterializeS3Local(ctx, source, opts)
		cancel()
	} else {
		hash, err = fs.flags.ExternalCacheClient.StoreContentFromS3(source, opts)
	}
	if err != nil {
		return "", err
	}
	if !validContentHash(hash) {
		return "", fmt.Errorf("cache returned invalid content hash %q", hash)
	}

	if _, direct := fs.flags.ExternalCacheClient.(cfg.ContentCacheMaterializeS3Local); !direct {
		localCache, ok := fs.flags.ExternalCacheClient.(cfg.ContentCacheMaterializeLocal)
		if ok {
			parent := fs.ctx
			if parent == nil {
				parent = context.Background()
			}
			ctx, cancel := context.WithTimeout(parent, readThroughMaterializationTimeout)
			local, localErr := localCache.MaterializeLocal(ctx, hash, int64(identity.size), struct{ RoutingKey string }{RoutingKey: routingKey})
			cancel()
			if localErr != nil {
				return "", fmt.Errorf("local cache materialization failed: %w", localErr)
			}
			if !local {
				return "", fmt.Errorf("content was not materialized in the local cache")
			}
		}
	}

	if err := fs.publishReadThroughHash(inode, identity, hash); err != nil {
		return "", err
	}
	fs.emitExternalCacheStoredEvent(cacheEvent{path: identity.path, size: identity.size, hash: hash}, "s3")

	elapsed := time.Since(started)
	mbPerSecond := float64(identity.size) / elapsed.Seconds() / (1024 * 1024)
	log.Infof("geesefs read-through materialization completed: path=%q size=%d hash=%q elapsed=%s throughput_mib_s=%.1f", identity.path, identity.size, hash, elapsed.Truncate(time.Millisecond), mbPerSecond)
	return hash, nil
}

func readThroughRoutingKey(bucket string, identity readThroughIdentity) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", bucket, identity.objectPath, identity.etag, identity.size)))
	return "geesefs-read-through:" + hex.EncodeToString(sum[:])
}

func validContentHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (fs *Goofys) publishReadThroughHash(inode *Inode, identity readThroughIdentity, hash string) error {
	inode.mu.Lock()
	if !inode.matchesReadThroughIdentityLocked(identity) {
		inode.mu.Unlock()
		return fmt.Errorf("object changed while it was being materialized")
	}
	cloud, objectPath := inode.cloud()
	inode.mu.Unlock()
	if objectPath != identity.objectPath {
		return fmt.Errorf("object path changed while it was being materialized")
	}

	head, err := cloud.HeadBlob(&HeadBlobInput{Key: identity.objectPath})
	if err != nil {
		return fmt.Errorf("object revalidation failed: %w", err)
	}
	if head.ETag == nil || *head.ETag != identity.etag || head.Size != identity.size {
		return fmt.Errorf("object changed while it was being materialized")
	}
	remoteMetadata := unescapeMetadata(head.Metadata)
	if remoteHash := string(remoteMetadata[fs.flags.HashAttr]); remoteHash != "" && remoteHash != hash {
		return fmt.Errorf("remote content hash changed while object was being materialized")
	}

	inode.mu.Lock()
	defer inode.mu.Unlock()
	if !inode.matchesReadThroughIdentityLocked(identity) {
		return fmt.Errorf("object changed before cache metadata was published")
	}
	if remoteHash := string(remoteMetadata[fs.flags.HashAttr]); remoteHash == hash {
		inode.setMetadata(head.Metadata)
		inode.hashMetadataChecked = true
		return nil
	}
	if inode.userMetadata == nil {
		inode.userMetadata = make(map[string][]byte)
	}
	inode.userMetadata[fs.flags.HashAttr] = []byte(hash)
	inode.hashMetadataChecked = true
	inode.hashMetadataDirty = true
	inode.sendHashUpdateMeta()
	return nil
}

func (inode *Inode) installReadThroughHash(identity readThroughIdentity, hash string) {
	inode.mu.Lock()
	defer inode.mu.Unlock()
	if !inode.matchesReadThroughIdentityLocked(identity) {
		return
	}
	if inode.userMetadata == nil {
		inode.userMetadata = make(map[string][]byte)
	}
	inode.userMetadata[inode.fs.flags.HashAttr] = []byte(hash)
	inode.hashMetadataChecked = true
}
