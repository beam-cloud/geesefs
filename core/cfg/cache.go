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
		CachePath   string
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

type ContentCacheMaterializeLocal interface {
	MaterializeLocal(ctx context.Context, hash string, size int64, opts struct{ RoutingKey string }) (bool, error)
}

type ContentCacheMaterializeS3Local interface {
	MaterializeS3Local(ctx context.Context, source struct {
		Path        string
		CachePath   string
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

type ClientLocalPageFileView struct {
	Path   string
	Offset int64
	Length int
}

type ContentCacheClientLocalPageFileViews interface {
	ClientLocalPageFileViews(hash string, offset int64, length int64, opts struct{ RoutingKey string }) ([]ClientLocalPageFileView, error)
}
