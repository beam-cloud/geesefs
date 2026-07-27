package core

import (
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newMetadataDeadlineTestBackend(endpoint string, flags *cfg.FlagStorage, config *cfg.S3Config) *S3Backend {
	client := s3.New(session.Must(session.NewSession(aws.NewConfig().
		WithCredentials(credentials.NewStaticCredentials("key", "secret", "")).
		WithEndpoint(endpoint).
		WithRegion("us-east-1").
		WithS3ForcePathStyle(true).
		WithHTTPClient(&http.Client{Timeout: 2 * time.Second}).
		WithMaxRetries(0))))
	return &S3Backend{
		S3:     client,
		bucket: "bucket",
		flags:  flags,
		config: config,
	}
}

func TestGetBlobSendsIfMatch(t *testing.T) {
	wantETag := `"etag-v1"`
	requestIfMatch := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestIfMatch <- r.Header.Get("If-Match")
		w.Header().Set("ETag", wantETag)
		_, _ = io.WriteString(w, "data")
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

	resp, err := backend.GetBlob(&GetBlobInput{Key: "model.bin", Count: 4, IfMatch: &wantETag})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if got := <-requestIfMatch; got != wantETag {
		t.Fatalf("If-Match = %q, want %q", got, wantETag)
	}
}

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

func TestMetadataHTTPTimeoutIsBounded(t *testing.T) {
	flags := cfg.DefaultFlags()
	backend := &S3Backend{flags: flags}

	flags.MetadataHTTPTimeout = 0
	if got := backend.metadataHTTPTimeout(); got != cfg.DefaultMetadataHTTPTimeout {
		t.Fatalf("zero metadata timeout = %v, want safe default %v", got, cfg.DefaultMetadataHTTPTimeout)
	}

	flags.MetadataHTTPTimeout = 15 * time.Minute
	if got := backend.metadataHTTPTimeout(); got != cfg.MaxMetadataHTTPTimeout {
		t.Fatalf("large metadata timeout = %v, want hard cap %v", got, cfg.MaxMetadataHTTPTimeout)
	}
}

func TestHeadBlobUsesMetadataDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	flags := cfg.DefaultFlags()
	flags.MetadataHTTPTimeout = 40 * time.Millisecond
	backend := newMetadataDeadlineTestBackend(server.URL, flags, &cfg.S3Config{})

	started := time.Now()
	_, err := backend.HeadBlob(&HeadBlobInput{Key: "model.bin"})
	if err == nil {
		t.Fatal("expected metadata request deadline")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("metadata request took %v, want deadline well below HTTP client timeout", elapsed)
	}
}

func TestListObjectsUsesMetadataDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config *cfg.S3Config
	}{
		{name: "v1", config: &cfg.S3Config{}},
		{name: "v2", config: &cfg.S3Config{ListV2: true}},
		{name: "v1ext", config: &cfg.S3Config{ListV1Ext: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			defer server.Close()

			flags := cfg.DefaultFlags()
			flags.MetadataHTTPTimeout = 40 * time.Millisecond
			backend := newMetadataDeadlineTestBackend(server.URL, flags, tc.config)

			started := time.Now()
			_, _, err := backend.ListObjectsV2(&s3.ListObjectsV2Input{
				Bucket: aws.String("bucket"),
			})
			if err == nil {
				t.Fatal("expected metadata request deadline")
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("metadata request took %v, want deadline well below HTTP client timeout", elapsed)
			}
		})
	}
}

func TestBucketDetectionHasFiniteRetries(t *testing.T) {
	var requests atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Slow Down",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
	awsConfig := aws.NewConfig().
		WithCredentials(credentials.NewStaticCredentials("key", "secret", "")).
		WithEndpoint("https://storage.invalid").
		WithRegion("us-east-1").
		WithHTTPClient(httpClient).
		WithMaxRetries(0)
	client := s3.New(session.Must(session.NewSession(awsConfig)))
	flags := cfg.DefaultFlags()
	flags.MetadataHTTPTimeout = 2 * time.Second
	backend := &S3Backend{
		S3:        client,
		bucket:    "bucket",
		awsConfig: awsConfig,
		flags:     flags,
		config:    &cfg.S3Config{},
	}

	started := time.Now()
	err, _ := backend.detectBucketLocationByHEAD()
	if err == nil {
		t.Fatal("expected bucket detection error")
	}
	if got := requests.Load(); got != bucketDetectionAttempts {
		t.Fatalf("bucket detection requests = %d, want %d", got, bucketDetectionAttempts)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("bucket detection took %v despite finite retry policy", elapsed)
	}
}
