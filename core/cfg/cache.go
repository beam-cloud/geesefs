package cfg

import "context"

type ContentCache interface {
	GetContent(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]byte, error)
	GetContentStream(hash string, offset int64, length int64, opts struct {
		RoutingKey string
	}) (chan []byte, error)
	StoreContent(chunks chan []byte, hash string, opts struct{ RoutingKey string }) (string, error)
	StoreContentFromS3(source struct {
		Path        string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error)
}

type ContentCacheReadInto interface {
	ReadContentInto(ctx context.Context, hash string, offset int64, dst []byte, opts struct{ RoutingKey string }) (int64, error)
}

type ContentCacheStoreLocalPath interface {
	StoreContentFromLocalPath(source struct {
		Path      string
		CachePath string
	}, opts struct {
		RoutingKey string
		Lock       bool
	}) (string, error)
}

// ContentCacheObjectIdentity identifies one immutable view of an object. ETag
// and Size are part of the identity so a stale hash can never be reused after
// the object is replaced in place.
type ContentCacheObjectIdentity struct {
	Endpoint string
	Bucket   string
	Path     string
	ETag     string
	Size     uint64
}

// ContentCacheObjectHashRegistry is an optional durable path-to-content-hash
// index owned by the external cache. S3 cannot update object metadata without
// copying the object, and several S3-compatible backends turn a metadata-only
// self-copy into a full object transfer. Keeping this tiny identity record in
// the cache coordinator lets lazy read-through survive remounts without
// rewriting or temporarily hiding the source object.
type ContentCacheObjectHashRegistry interface {
	LookupObjectContentHash(ctx context.Context, identity ContentCacheObjectIdentity) (hash string, found bool, err error)
	StoreObjectContentHash(ctx context.Context, identity ContentCacheObjectIdentity, hash string) error
}

type ClientLocalPageFileView struct {
	Path   string
	Offset int64
	Length int
}

type ContentCacheClientLocalPageFileViews interface {
	ClientLocalPageFileViews(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]ClientLocalPageFileView, error)
}
