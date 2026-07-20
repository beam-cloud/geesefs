//go:build !linux

package core

import "github.com/yandex-cloud/geesefs/core/cfg"

func supportsExternalCacheFDReads(client cfg.ContentCache) bool {
	return false
}

func (fs *Goofys) externalCacheFDReadsEnabled() bool {
	return false
}
