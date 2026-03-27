package benchmarks

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/juliehwang/diskcache"
)

func BenchmarkDiskCache_Set(b *testing.B) {
	dir := b.TempDir()
	cfg := &diskcache.Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         100 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	dc, err := diskcache.NewDiskCache(cfg)
	if err != nil {
		b.Fatal("create disk cache:", err)
	}
	defer dc.Close()

	data := bytes.Repeat([]byte("a"), 1024)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := dc.Set(fmt.Sprintf("key_%d", i), data); err != nil {
			b.Fatal("set:", err)
		}
	}
}

func BenchmarkDiskCache_Get(b *testing.B) {
	dir := b.TempDir()
	cfg := &diskcache.Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         100 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	dc, err := diskcache.NewDiskCache(cfg)
	if err != nil {
		b.Fatal("create disk cache:", err)
	}
	defer dc.Close()

	data := bytes.Repeat([]byte("a"), 1024)
	for i := 0; i < 1000; i++ {
		if err := dc.Set(fmt.Sprintf("key_%d", i), data); err != nil {
			b.Fatal("set:", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if _, err := dc.Get(fmt.Sprintf("key_%d", i%1000)); err != nil {
			b.Fatal("get:", err)
		}
	}
}
