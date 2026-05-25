package core

import (
	"crypto/md5"
	"fmt"
	"testing"
)

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

func TestExpectedMultipartETag(t *testing.T) {
	part1 := md5.Sum([]byte("part-one"))
	part2 := md5.Sum([]byte("part-two"))
	part1ETag := fmt.Sprintf("%x", part1)
	part2ETag := fmt.Sprintf("\"%x\"", part2)

	got := expectedMultipartETag([]*string{&part1ETag, &part2ETag}, 2)
	if got == nil {
		t.Fatal("expected multipart etag")
	}

	combined := append(part1[:], part2[:]...)
	wantSum := md5.Sum(combined)
	want := fmt.Sprintf("%x-2", wantSum)
	if *got != want {
		t.Fatalf("expected %s, got %s", want, *got)
	}
}

func TestExpectedMultipartETagRejectsOpaquePartETag(t *testing.T) {
	opaque := "opaque-etag"
	if got := expectedMultipartETag([]*string{&opaque}, 1); got != nil {
		t.Fatalf("expected opaque multipart etag to be rejected, got %s", *got)
	}
}
