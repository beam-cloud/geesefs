package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/yandex-cloud/geesefs/core/cfg"
)

const (
	defaultExternalCacheReadTimeout = 30 * time.Second
)

type externalCacheCallResult[T any] struct {
	value T
	err   error
}

func (fs *Goofys) externalCacheReadTimeout() time.Duration {
	if fs != nil && fs.flags != nil && fs.flags.ExternalCacheReadTimeout > 0 {
		return fs.flags.ExternalCacheReadTimeout
	}
	return defaultExternalCacheReadTimeout
}

func (fs *Goofys) recordExternalCacheTimeout(op, hash string, timeout time.Duration) {
	if fs == nil {
		return
	}

	atomic.AddInt64(&fs.stats.externalCacheTimeouts, 1)
	log.Warnf("geesefs external cache read timed out: op=%s hash=%q timeout=%s", op, hash, timeout)
}

func externalCacheCall[T any](fs *Goofys, op, hash string, call func(context.Context) (T, error)) (T, error) {
	var zero T

	timeout := fs.externalCacheReadTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ch := make(chan externalCacheCallResult[T], 1)
	go func() {
		value, err := call(ctx)
		ch <- externalCacheCallResult[T]{value: value, err: err}
	}()

	select {
	case result := <-ch:
		if errors.Is(result.err, context.DeadlineExceeded) {
			fs.recordExternalCacheTimeout(op, hash, timeout)
			return zero, fmt.Errorf("%w: %s exceeded %s: %w", errExternalCacheTimeout, op, timeout, result.err)
		}
		return result.value, result.err
	case <-ctx.Done():
		fs.recordExternalCacheTimeout(op, hash, timeout)
		return zero, fmt.Errorf("%w: %s exceeded %s", errExternalCacheTimeout, op, timeout)
	}
}

func (fs *Goofys) externalCacheGetContent(cache cfg.ContentCache, hash string, offset int64, length int64) ([]byte, error) {
	return externalCacheCall(fs, "GetContent", hash, func(ctx context.Context) ([]byte, error) {
		return cache.GetContent(hash, offset, length, struct{ RoutingKey string }{RoutingKey: hash})
	})
}

func (fs *Goofys) externalCacheGetContentStream(cache cfg.ContentCache, hash string, offset int64, length int64) (chan []byte, error) {
	return externalCacheCall(fs, "GetContentStream", hash, func(ctx context.Context) (chan []byte, error) {
		return cache.GetContentStream(hash, offset, length, struct {
			RoutingKey string
		}{RoutingKey: hash})
	})
}

func (fs *Goofys) externalCacheReadContentInto(cache cfg.ContentCacheReadInto, hash string, offset int64, dst []byte) (int64, error) {
	return externalCacheCall(fs, "ReadContentInto", hash, func(ctx context.Context) (int64, error) {
		return cache.ReadContentInto(ctx, hash, offset, dst, struct{ RoutingKey string }{RoutingKey: hash})
	})
}

func (fs *Goofys) externalCacheClientLocalPageFileViews(cache cfg.ContentCacheClientLocalPageFileViews, hash string, offset int64, length int64) ([]cfg.ClientLocalPageFileView, error) {
	return externalCacheCall(fs, "ClientLocalPageFileViews", hash, func(ctx context.Context) ([]cfg.ClientLocalPageFileView, error) {
		return cache.ClientLocalPageFileViews(hash, offset, length, struct{ RoutingKey string }{RoutingKey: hash})
	})
}

func (fs *Goofys) externalCacheStreamReceiveTimeout(op, hash string, timeout time.Duration) error {
	fs.recordExternalCacheTimeout(op, hash, timeout)
	return fmt.Errorf("%w: %s exceeded %s", errExternalCacheTimeout, op, timeout)
}
