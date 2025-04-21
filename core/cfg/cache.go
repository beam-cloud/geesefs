package cfg

type ContentCache interface {
	GetContent(hash string, offset int64, length int64) ([]byte, error)
	StoreContent(chunks chan []byte, hash string) (string, error)
	StoreContentFromSource(bucketName string, key string) (string, error)
	StoreContentFromSourceWithLock(bucketName string, key string) (string, error)
}
