package core

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/yandex-cloud/geesefs/core/cfg"
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

func TestMultipartCopyPartRetriesTransientInternalError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `<Error><Code>InternalError</Code><Message>retry me</Message></Error>`)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<CopyPartResult><ETag>"part-etag"</ETag><LastModified>2026-07-16T00:00:00Z</LastModified></CopyPartResult>`)
	}))
	defer server.Close()

	client := s3.New(session.Must(session.NewSession(aws.NewConfig().
		WithCredentials(credentials.NewStaticCredentials("key", "secret", "")).
		WithEndpoint(server.URL).
		WithRegion("us-east-1").
		WithS3ForcePathStyle(true).
		WithMaxRetries(0))))
	backend := &S3Backend{
		S3:     client,
		bucket: "bucket",
		flags:  cfg.DefaultFlags(),
		config: &cfg.S3Config{},
	}

	etag, err := backend.mpuCopyPart("bucket/source", "destination", "upload-id", "bytes=0-241", 1, nil)
	if err != nil {
		t.Fatalf("multipart copy part failed after transient error: %v", err)
	}
	if etag == nil || *etag != `"part-etag"` {
		t.Fatalf("etag = %v, want part-etag", etag)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}
