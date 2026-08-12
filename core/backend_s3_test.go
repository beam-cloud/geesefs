package core

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/sirupsen/logrus"
	"github.com/yandex-cloud/geesefs/core/cfg"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func captureS3Log(t *testing.T) *bytes.Buffer {
	t.Helper()
	output := &bytes.Buffer{}
	originalOutput := s3Log.Out
	originalLevel := s3Log.Level
	s3Log.SetOutput(output)
	s3Log.SetLevel(logrus.WarnLevel)
	t.Cleanup(func() {
		s3Log.SetOutput(originalOutput)
		s3Log.SetLevel(originalLevel)
	})
	return output
}

func newListBlobsTestBackend(t *testing.T, endpoint string, retries int, timeout time.Duration) *S3Backend {
	t.Helper()
	flags := cfg.DefaultFlags()
	flags.Endpoint = endpoint
	flags.MetadataHTTPTimeout = timeout
	backend, err := NewS3("bucket", flags, (&cfg.S3Config{
		AccessKey:             "key",
		SecretKey:             "secret",
		ListV2:                true,
		MetadataSDKMaxRetries: retries,
	}).Init())
	if err != nil {
		t.Fatal(err)
	}
	return backend
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
	if got := backend.S3.MaxRetries(); got != 0 {
		t.Fatalf("client retries = %d, want metadata override to remain request-local", got)
	}
}

func TestListBlobsRetriesRequestTimeout(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusRequestTimeout)
			_, _ = fmt.Fprint(w, `<Error><Code>RequestTimeout</Code><Message>retry me</Message></Error>`)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<ListBucketResult><Name>bucket</Name><KeyCount>0</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()

	backend := newListBlobsTestBackend(t, server.URL, cfg.DefaultMetadataSDKMaxRetries, 0)

	if _, err := backend.ListBlobs(&ListBlobsInput{}); err != nil {
		t.Fatalf("list failed after transient request timeout: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestListBlobsExhaustedRequestTimeoutMapsToEAGAIN(t *testing.T) {
	var requests atomic.Int32
	logs := captureS3Log(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("X-Amz-Request-Id", "request-408")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = fmt.Fprint(w, `<Error><Code>RequestTimeout</Code><Message>still unavailable</Message></Error>`)
	}))
	defer server.Close()

	backend := newListBlobsTestBackend(t, server.URL, cfg.DefaultMetadataSDKMaxRetries, 0)

	_, err := backend.ListBlobs(&ListBlobsInput{})
	if err == nil {
		t.Fatal("expected request timeout")
	}
	mapped := mapAwsError(err)
	if !errors.Is(mapped, syscall.EAGAIN) {
		t.Fatalf("mapAwsError(%v) = %v, want %v", err, mapped, syscall.EAGAIN)
	}
	if remapped := mapAwsError(mapped); remapped != syscall.EAGAIN {
		t.Fatalf("remapped error = %v, want %v", remapped, syscall.EAGAIN)
	}
	if got := requests.Load(); got != int32(cfg.DefaultMetadataSDKMaxRetries+1) {
		t.Fatalf("requests = %d, want %d", got, cfg.DefaultMetadataSDKMaxRetries+1)
	}
	logText := logs.String()
	if strings.Count(logText, "transient backend request exhausted") != 1 ||
		!strings.Contains(logText, "http=408") ||
		!strings.Contains(logText, "s3=RequestTimeout") ||
		!strings.Contains(logText, "request=request-408") {
		t.Fatalf("unexpected exhausted request log: %q", logText)
	}
}

func TestListBlobsRetriesHonorMetadataDeadline(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = fmt.Fprint(w, `<Error><Code>RequestTimeout</Code><Message>still unavailable</Message></Error>`)
	}))
	defer server.Close()

	const metadataTimeout = 75 * time.Millisecond
	backend := newListBlobsTestBackend(t, server.URL, cfg.DefaultMetadataSDKMaxRetries, metadataTimeout)

	started := time.Now()
	_, err := backend.ListBlobs(&ListBlobsInput{})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected request timeout")
	}
	if mapped := mapAwsError(err); !errors.Is(mapped, syscall.EAGAIN) {
		t.Fatalf("mapAwsError(%v) = %v, want %v", err, mapped, syscall.EAGAIN)
	}
	if got := requests.Load(); got >= int32(cfg.DefaultMetadataSDKMaxRetries+1) {
		t.Fatalf("requests = %d, want metadata deadline to stop retries before %d", got, cfg.DefaultMetadataSDKMaxRetries+1)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("metadata retries took %v, want total deadline near %v", elapsed, metadataTimeout)
	}
}

func TestListBlobsHealthyDoesNotRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<ListBucketResult><Name>bucket</Name><KeyCount>0</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()

	backend := newListBlobsTestBackend(t, server.URL, cfg.DefaultMetadataSDKMaxRetries, 0)

	if _, err := backend.ListBlobs(&ListBlobsInput{}); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestListBlobsMetadataRetriesRequireOptIn(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = fmt.Fprint(w, `<Error><Code>RequestTimeout</Code><Message>do not retry</Message></Error>`)
	}))
	defer server.Close()

	backend := newListBlobsTestBackend(t, server.URL, 0, 0)

	_, err := backend.ListBlobs(&ListBlobsInput{})
	if mapped := mapAwsError(err); !errors.Is(mapped, syscall.EAGAIN) {
		t.Fatalf("mapAwsError(%v) = %v, want %v", err, mapped, syscall.EAGAIN)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 without metadata retry opt-in", got)
	}
}

func TestRetryableBackendErrorsMapToEAGAIN(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{
			name: "request timeout code",
			err:  awserr.New("RequestTimeout", "request timed out", nil),
		},
		{
			name: "request transport timeout",
			err: awserr.New("RequestError", "request failed", &net.DNSError{
				IsTimeout: true,
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mapAwsError(test.err); !errors.Is(got, syscall.EAGAIN) {
				t.Fatalf("mapAwsError(%v) = %v, want %v", test.err, got, syscall.EAGAIN)
			}
		})
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
