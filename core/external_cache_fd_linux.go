//go:build linux

package core

import "github.com/yandex-cloud/geesefs/core/cfg"

func supportsExternalCacheFDReads(client cfg.ContentCache) bool {
	pageCache, ok := client.(cfg.ContentCacheClientLocalPageFileViews)
	return ok && pageCache != nil
}

func (fs *Goofys) externalCacheFDReadsEnabled() bool {
	return fs != nil && fs.externalCacheFDReads && fs.flags != nil &&
		supportsExternalCacheFDReads(fs.flags.ExternalCacheClient)
}
