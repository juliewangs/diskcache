# DiskCache Architecture Design

## Overview

DiskCache is a high-performance disk cache library that uses a sharded lock + LRU linked list design to provide a high-concurrency disk caching solution.

## Architecture

### Component Overview

```
DiskCache
├── shards[16]        // cacheShard array
│   ├── sync.RWMutex  // per-shard lock
│   ├── lruList       // doubly linked list (container/list)
│   ├── index         // map[string]*list.Element
│   └── currentSize   // shard byte count
├── evictChan         // buffered channel for async file deletion
├── totalMu           // serializes cross-shard eviction
├── cleanupLoop()     // background TTL cleanup goroutine
└── evictWorker()     // background file deletion goroutine
```

### Key Design Decisions

#### Sharded Locks
16 shards (FNV-1a hash) reduce write lock contention. Each shard has an independent
RWMutex, so reads on different shards never block each other.

#### Disk I/O Outside Locks
`Set` writes to disk **before** acquiring the shard lock. `Delete` and eviction
remove the in-memory entry under the lock, then delete the file outside it.
This keeps lock hold times minimal (pure in-memory operations only).

#### Two-Phase Cleanup
`cleanup()` collects expired keys under a **read lock** (non-blocking to Get/Set),
then removes them under a **write lock**. Disk deletion is queued to `evictChan`
and handled by `evictWorker`.

#### Async Eviction
Evicted file paths are sent to a buffered channel (`evictChan`, cap=512).
`evictWorker` deletes them in the background. If the channel is full, deletion
falls back to synchronous removal to avoid unbounded memory growth.

#### Startup Recovery
`loadIndex` scans the cache directory, filters out expired files, sorts by
modification time, and rebuilds the LRU list. Expired files are queued for
async deletion without blocking startup.

### Data Flow

```
Set(key, data)
  ├─ evictIfNeeded(dataSize)     // cross-shard LRU eviction under totalMu
  ├─ os.WriteFile(...)           // disk write, no lock held
  └─ shard.Lock → update index   // pure in-memory, fast

Get(key)
  ├─ shard.RLock → copy metadata // read lock, no disk I/O
  ├─ check TTL                   // expired → async Delete
  ├─ os.ReadFile(...)            // disk read, no lock held
  └─ shard.Lock → MoveToBack     // re-fetch elem from index to avoid stale refs
```

## Performance Optimizations

### Concurrency
- Sharded lock design to reduce lock contention
- Read-write separation allowing concurrent reads
- Asynchronous file deletion to avoid blocking the main path

### Memory
- Lazy loading: file contents are only read when needed
- Periodic cleanup of expired cache entries

### Disk I/O
- Disk writes performed outside locks
- Async eviction via buffered channel
- Cache-friendly flat file layout

## Feature Support

### LRU Eviction
- Access-time-based LRU eviction strategy
- Maximum cache size limit support
- Automatic eviction of least recently used entries

### TTL Expiration
- Configurable cache entry time-to-live
- Periodic background cleanup of expired entries
- Lazy deletion mechanism to reduce overhead

### Persistence
- Cache data persisted to disk
- Supports recovery after process restart
- Index rebuilt from disk on startup

## API Design

### Core Interface
```go
type DiskCache struct { ... }

func NewDiskCache(cfg *Config) (*DiskCache, error)
func (dc *DiskCache) Set(key string, data []byte) error
func (dc *DiskCache) Get(key string) ([]byte, error)
func (dc *DiskCache) Delete(key string) error
func (dc *DiskCache) Close()
```

### Configuration
```go
type Config struct {
    DiskCacheDir             string        // Cache directory
    DiskCacheMaxSize         int64         // Maximum cache size
    DiskCacheTTL             time.Duration // Cache time-to-live
    DiskCacheCleanupInterval time.Duration // Cleanup interval
}
```

## Use Cases

### Suitable For
- Web service static resource caching
- Intermediate result caching in data processing
- Machine learning model caching
- File download caching

### Not Suitable For
- Hot data caching requiring sub-millisecond response
- Frequently updated small data caching
- Caching scenarios requiring complex queries

## Reliability

### Data Safety
- Atomic operations ensure data consistency
- Graceful shutdown ensures data integrity

### Fault Recovery
- Automatic detection and cleanup of corrupted files
- Comprehensive error handling and logging