// Package diskcache 提供高性能的磁盘缓存实现，支持 LRU 淘汰策略和 TTL 过期机制。
//
// 本包是 pkg/disk_cache 的顶层入口，方便用户直接 import "github.com/juliehwang/diskcache" 使用。
//
// 快速开始:
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
	impl "github.com/juliehwang/diskcache/pkg/disk_cache"
)

// Config 定义磁盘缓存的配置参数（类型别名，等同于 pkg/disk_cache.Config）。
type Config = impl.Config

// DiskCache 是磁盘缓存实例（类型别名，等同于 pkg/disk_cache.DiskCache）。
type DiskCache = impl.DiskCache

// New 创建磁盘缓存实例（推荐使用）。
func New(cfg *Config) (*DiskCache, error) {
	return impl.NewDiskCache(cfg)
}

// NewDiskCache 创建磁盘缓存实例。
// 保留与 pkg/disk_cache.NewDiskCache 相同的函数名，方便迁移。
func NewDiskCache(cfg *Config) (*DiskCache, error) {
	return impl.NewDiskCache(cfg)
}

// BuildCacheKey 根据文件 URL 和块索引构建缓存键。
func BuildCacheKey(fileURL string, blockIndex uint64) string {
	return impl.BuildCacheKey(fileURL, blockIndex)
}
