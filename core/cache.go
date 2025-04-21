package core

type ContentCache interface {
	GetContent(hash string, offset int64, length int64) ([]byte, error)
	StoreContent(chunks chan []byte, hash string) (string, error)
	StoreContentFromSource(bucketName string, key string, hash string) (string, error)
}
