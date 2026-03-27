# DiskCache

[![CI](https://github.com/juliehwang/diskcache/actions/workflows/ci.yml/badge.svg)](https://github.com/juliehwang/diskcache/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/juliehwang/diskcache)](https://goreportcard.com/report/github.com/juliehwang/diskcache)
[![GoDoc](https://pkg.go.dev/badge/github.com/juliehwang/diskcache)](https://pkg.go.dev/github.com/juliehwang/diskcache)
[![Go Version](https://img.shields.io/github/go-mod/go-version/juliehwang/diskcache)](go.mod)
[![License](https://img.shields.io/badge/license-BSD--2--Clause-blue.svg)](LICENSE)

[中文文档](README_CN.md)

## Why DiskCache?

Most Go caching libraries (groupcache, bigcache, freecache) are **in-memory only** — data is lost on restart and total capacity is limited by available RAM.

DiskCache fills a different niche: **persistent, disk-backed caching** with the ergonomics of an in-memory cache.

| | DiskCache | bigcache | freecache | groupcache |
|---|---|---|---|---|
| **Storage** | Disk | Memory | Memory | Memory |
| **Survives restart** | ✅ | ❌ | ❌ | ❌ |
| **Capacity** | Limited by disk | Limited by RAM | Limited by RAM | Limited by RAM |
| **Concurrency** | Sharded RWMutex | Sharded Mutex | Sharded Mutex | Mutex |
| **Eviction** | LRU + TTL | FIFO + TTL | LRU + TTL | LRU |
| **Use case** | Large files, warm restart | Hot data, low latency | Hot data, zero GC | Distributed cache |

**Choose DiskCache when you need:**
- Cache data that **survives process restarts** without external dependencies (Redis, Memcached)
- Caching **large objects** (images, ML models, compiled templates) that don't fit in RAM
- A **simple, embedded** solution with zero infrastructure overhead

## Features

- **Sharded locks** — 16 shards (FNV-1a hash) reduce write contention
- **O(1) LRU eviction** — doubly linked list + hash map per shard
- **TTL expiration** — background cleanup + lazy deletion on access
- **Async disk I/O** — evicted files deleted via buffered channel, outside shard locks
- **Restart recovery** — in-memory index rebuilt from disk on startup
- **Zero dependencies** — only the Go standard library (test dependencies excluded)

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                     DiskCache                       │
│                                                     │
│  ┌──────────┐ ┌──────────┐       ┌──────────┐      │
│  │ Shard 0  │ │ Shard 1  │  ...  │ Shard 15 │      │
│  │ RWMutex  │ │ RWMutex  │       │ RWMutex  │      │
│  │ LRU List │ │ LRU List │       │ LRU List │      │
│  │ Index Map│ │ Index Map│       │ Index Map│      │
│  └──────────┘ └──────────┘       └──────────┘      │
│                                                     │
│  ┌─────────────────┐  ┌──────────────────────────┐  │
│  │  cleanupLoop()  │  │  evictWorker()           │  │
│  │  periodic TTL   │  │  async file deletion     │  │
│  │  scan & expire  │  │  via buffered channel    │  │
│  └─────────────────┘  └──────────────────────────┘  │
└─────────────────────────────────────────────────────┘
						 │
					┌────┴────┐
					│  Disk   │
					│ (flat   │
					│  files) │
					└─────────┘
```

Key design decisions:
- **Disk I/O outside locks** — `Set` writes to disk before acquiring the shard lock; eviction deletes files after releasing it. Lock hold time is pure in-memory work.
- **Two-phase cleanup** — expired keys collected under read lock, removed under write lock. No blocking of concurrent reads during scan.
- **Async eviction** — evicted file paths are sent to a buffered channel (cap 512). Falls back to synchronous deletion if the channel is full.

> For a deeper dive, see [docs/design/ARCHITECTURE.md](docs/design/ARCHITECTURE.md).

## Installation

```bash
go get github.com/juliehwang/diskcache
```

Requires **Go 1.21+**. No CGO, no external dependencies.

## Quick Start

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/juliehwang/diskcache"
)

func main() {
	dc, err := diskcache.New(&diskcache.Config{
		DiskCacheDir:             "./cache",
		DiskCacheMaxSize:         100 * 1024 * 1024, // 100 MB
		DiskCacheTTL:             24 * time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer dc.Close()

	_ = dc.Set("greeting", []byte("hello, diskcache"))
	data, _ := dc.Get("greeting")
	fmt.Println(string(data)) // "hello, diskcache"
}
```

## API

| Method | Description |
|--------|-------------|
| `New(cfg) (*DiskCache, error)` | Create a cache instance |
| `Set(key, data) error` | Write a cache entry |
| `Get(key) ([]byte, error)` | Read a cache entry (returns `os.ErrNotExist` on miss) |
| `Delete(key) error` | Delete a cache entry |
| `Size() int64` | Total cache size in bytes |
| `Len() int` | Number of cached entries |
| `Close()` | Stop background goroutines and drain eviction queue |

## Config

| Field | Type | Description |
|-------|------|-------------|
| `DiskCacheDir` | `string` | Cache directory path |
| `DiskCacheMaxSize` | `int64` | Max total size in bytes |
| `DiskCacheTTL` | `time.Duration` | Entry time-to-live |
| `DiskCacheCleanupInterval` | `time.Duration` | Background cleanup interval |

## Benchmarks

Tested on Linux (Intel Xeon Platinum 8255C @ 2.50GHz, 16 cores), 1 KB values:

```
goos: linux
goarch: amd64
cpu: Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz

BenchmarkDiskCache_Set-16    59736    20052 ns/op    526 B/op    8 allocs/op
BenchmarkDiskCache_Get-16   121865     9627 ns/op   1541 B/op    6 allocs/op
```

| Operation | Throughput | Latency | Allocs |
|-----------|-----------|---------|--------|
| **Set** (1 KB) | ~50,000 ops/s | ~20 µs | 8 allocs/op |
| **Get** (1 KB) | ~104,000 ops/s | ~9.6 µs | 6 allocs/op |

> **Note:** Latency is dominated by disk I/O. On NVMe SSDs, expect significantly better numbers. In-memory index operations alone take < 200 ns.

Run benchmarks yourself:

```bash
cd test/benchmarks && go test -bench=. -benchmem
```

## Use Cases

| Scenario | Example |
|----------|---------|
| **Web static assets** | Cache resized images, compiled CSS/JS bundles |
| **ML model serving** | Cache downloaded model weights locally |
| **Data pipelines** | Cache intermediate processing results |
| **API response caching** | Persist upstream API responses across restarts |
| **File download proxy** | Local disk cache for remote file downloads |

## Project Structure

```
diskcache/
├── diskcache.go                    # Public facade (New / Config)
├── pkg/disk_cache/
│   ├── disk_cache.go               # Core implementation
│   └── disk_cache_test.go          # Unit tests
├── test/benchmarks/
│   └── disk_cache_bench_test.go    # Benchmarks
├── examples/
│   └── basic_usage.go              # Usage example
└── docs/design/
	└── ARCHITECTURE.md             # Design documentation
```

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Run quality checks before committing:
   ```bash
   make check   # runs gofmt + go vet + race tests
   ```
4. Commit with a clear message (`git commit -m 'feat: add amazing feature'`)
5. Push and open a Pull Request

## License

BSD 2-Clause License — see [LICENSE](LICENSE).