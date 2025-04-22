package cfg

type ContentCache interface {
	GetContent(hash string, offset int64, length int64) ([]byte, error)
	StoreContent(chunks chan []byte, hash string) (string, error)
	StoreContentFromSource(source struct {
		Path        string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}) (string, error)
	StoreContentFromSourceWithLock(source struct {
		Path        string
		BucketName  string
		Region      string
		EndpointURL string
		AccessKey   string
		SecretKey   string
	}) (string, error)
}
