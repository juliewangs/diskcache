// Package diskcache provides a disk cache with LRU eviction and TTL expiration.
//
// Quick start:
//
//	cfg := &diskcache.Config{
//		DiskCacheDir:             "./cache",
//		DiskCacheMaxSize:         100 * 1024 * 1024,
//		DiskCacheTTL:             24 * time.Hour,
//		DiskCacheCleanupInterval: time.Hour,
//	}
//	dc, err := diskcache.New(cfg)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer dc.Close()
package diskcache

import (
	impl "github.com/juliewangs/diskcache/pkg/disk_cache"
)

// Config is a type alias for pkg/disk_cache.Config.
type Config = impl.Config

// DiskCache is a type alias for pkg/disk_cache.DiskCache.
type DiskCache = impl.DiskCache

// New creates a new DiskCache instance.
func New(cfg *Config) (*DiskCache, error) {
	return impl.NewDiskCache(cfg)
}

// NewDiskCache creates a new DiskCache instance (same as New, for migration compatibility).
func NewDiskCache(cfg *Config) (*DiskCache, error) {
	return impl.NewDiskCache(cfg)
}

// BuildCacheKey builds a cache key from a file URL and block index.
func BuildCacheKey(fileURL string, blockIndex uint64) string {
	return impl.BuildCacheKey(fileURL, blockIndex)
}
