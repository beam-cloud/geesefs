package core

import "testing"

func TestShouldUseMultipartCopyAvoidsMetadataSelfCopy(t *testing.T) {
	threshold := uint64(128 * 1024 * 1024)

	if shouldUseMultipartCopy(false, 1024*1024*1024, threshold, true) {
		t.Fatal("1GiB metadata self-copy must use CopyObject, not multipart copy")
	}
	if !shouldUseMultipartCopy(false, 1024*1024*1024, threshold, false) {
		t.Fatal("1GiB cross-object copy should still use multipart copy")
	}
	if !shouldUseMultipartCopy(false, maxSingleCopyObjectSize+1, threshold, true) {
		t.Fatal("metadata self-copy above S3 single-copy limit must use multipart copy")
	}
	if shouldUseMultipartCopy(true, 1024*1024*1024, threshold, false) {
		t.Fatal("GCS-compatible backend should not use S3 multipart copy")
	}
}
