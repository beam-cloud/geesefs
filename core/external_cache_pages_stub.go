//go:build windows

package core

type externalPageMmapCache struct{}

func (fs *Goofys) closeExternalPageMmapCache() {}
