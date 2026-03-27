// Package diskcache provides a disk cache with LRU eviction and TTL expiration.
package diskcache

import (
	"container/list"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	shardCount    = 16 // must be power of 2
	evictChanSize = 512
	filePermMode  = 0644
	dirPermMode   = 0755
	fnvHashBytes  = 8
)

// cacheShard is a single shard with its own LRU list, index map, and RWMutex.
type cacheShard struct {
	mu          sync.RWMutex
	index       map[string]*list.Element
	lruList     *list.List
	currentSize int64
}

// DiskCache is a sharded disk cache with O(1) LRU eviction and async disk deletion.
//
// Design choices:
//   - 16 shards reduce write lock contention.
//   - Disk I/O happens outside shard locks.
//   - Expired entries are lazily deleted on access.
type DiskCache struct {
	baseDir         string
	maxSize         int64
	ttl             time.Duration
	cleanupInterval time.Duration
	shards          [shardCount]*cacheShard
	totalMu         sync.Mutex  // serializes cross-shard eviction
	evictChan       chan string // async disk deletion queue
	stopChan        chan struct{}
	wg              sync.WaitGroup
	closeOnce       sync.Once
}

// cacheEntry holds metadata for a single cached file.
type cacheEntry struct {
	Key        string
	FilePath   string
	Size       int64
	CreatedAt  time.Time
	AccessedAt time.Time
}

// Config holds disk cache configuration.
type Config struct {
	DiskCacheDir             string
	DiskCacheMaxSize         int64
	DiskCacheTTL             time.Duration
	DiskCacheCleanupInterval time.Duration
}

// NewDiskCache creates a new DiskCache. It scans the cache directory to rebuild
// the in-memory index and starts background cleanup and eviction goroutines.
func NewDiskCache(cfg *Config) (*DiskCache, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if cfg.DiskCacheDir == "" {
		return nil, fmt.Errorf("cache directory is empty")
	}
	if cfg.DiskCacheMaxSize <= 0 {
		return nil, fmt.Errorf("max size must be positive, got %d", cfg.DiskCacheMaxSize)
	}
	if cfg.DiskCacheTTL <= 0 {
		return nil, fmt.Errorf("ttl must be positive, got %v", cfg.DiskCacheTTL)
	}
	if cfg.DiskCacheCleanupInterval <= 0 {
		return nil, fmt.Errorf("cleanup interval must be positive, got %v", cfg.DiskCacheCleanupInterval)
	}
	if err := os.MkdirAll(cfg.DiskCacheDir, dirPermMode); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	dc := &DiskCache{
		baseDir:         cfg.DiskCacheDir,
		maxSize:         cfg.DiskCacheMaxSize,
		ttl:             cfg.DiskCacheTTL,
		cleanupInterval: cfg.DiskCacheCleanupInterval,
		evictChan:       make(chan string, evictChanSize),
		stopChan:        make(chan struct{}),
	}
	for i := 0; i < shardCount; i++ {
		dc.shards[i] = &cacheShard{
			index:   make(map[string]*list.Element),
			lruList: list.New(),
		}
	}
	if err := dc.loadIndex(); err != nil {
		return nil, fmt.Errorf("load disk cache index: %w", err)
	}
	dc.wg.Add(2)
	go dc.cleanupLoop()
	go dc.evictWorker()
	return dc, nil
}

// getShard returns the shard for key using FNV-1a on the first 8 bytes.
// Direct low-nibble of hex chars would leave shards 10-15 empty; FNV-1a avoids this.
func (dc *DiskCache) getShard(key string) *cacheShard {
	if len(key) == 0 {
		return dc.shards[0]
	}
	const (
		fnvOffset uint32 = 2166136261
		fnvPrime  uint32 = 16777619
	)
	h := fnvOffset
	n := len(key)
	if n > fnvHashBytes {
		n = fnvHashBytes
	}
	for i := 0; i < n; i++ {
		h ^= uint32(key[i])
		h *= fnvPrime
	}
	return dc.shards[h&(shardCount-1)]
}

// currentTotalSize returns the sum of all shard sizes.
func (dc *DiskCache) currentTotalSize() int64 {
	var total int64
	for _, s := range dc.shards {
		s.mu.RLock()
		total += s.currentSize
		s.mu.RUnlock()
	}
	return total
}

// Get retrieves cached data by key. Returns os.ErrNotExist on miss or expiration.
func (dc *DiskCache) Get(key string) ([]byte, error) {
	shard := dc.getShard(key)
	shard.mu.RLock()
	elem, ok := shard.index[key]
	if !ok {
		shard.mu.RUnlock()
		return nil, os.ErrNotExist
	}
	entry, ok := elem.Value.(*cacheEntry)
	if !ok || entry == nil {
		shard.mu.RUnlock()
		return nil, fmt.Errorf("invalid cache entry for key: %s", key)
	}
	createdAt := entry.CreatedAt
	filePath := entry.FilePath
	shard.mu.RUnlock()

	if time.Since(createdAt) > dc.ttl {
		go func() { _ = dc.Delete(key) }() // lazy cleanup
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		_ = dc.Delete(key)
		return nil, err
	}
	// Re-fetch elem from index; a concurrent Set may have replaced it during the file read.
	shard.mu.Lock()
	if liveElem, stillExists := shard.index[key]; stillExists {
		shard.lruList.MoveToBack(liveElem)
		if liveEntry, ok := liveElem.Value.(*cacheEntry); ok {
			liveEntry.AccessedAt = time.Now()
		}
	}
	shard.mu.Unlock()
	return data, nil
}

// Set writes data to disk and updates the in-memory index. Evicts LRU entries if needed.
func (dc *DiskCache) Set(key string, data []byte) error {
	dataSize := int64(len(data))
	dc.evictIfNeeded(dataSize)
	filePath := dc.getFilePath(key)
	if err := os.WriteFile(filePath, data, filePermMode); err != nil {
		return fmt.Errorf("write cache file %s: %w", filePath, err)
	}
	shard := dc.getShard(key)
	now := time.Now()
	shard.mu.Lock()
	if elem, exists := shard.index[key]; exists {
		if old, ok := elem.Value.(*cacheEntry); ok {
			shard.currentSize -= old.Size
		}
		shard.lruList.Remove(elem)
	}
	entry := &cacheEntry{
		Key:        key,
		FilePath:   filePath,
		Size:       dataSize,
		CreatedAt:  now,
		AccessedAt: now,
	}
	elem := shard.lruList.PushBack(entry)
	shard.index[key] = elem
	shard.currentSize += dataSize
	shard.mu.Unlock()
	return nil
}

// Delete removes the specified cache entry.
func (dc *DiskCache) Delete(key string) error {
	shard := dc.getShard(key)
	shard.mu.Lock()
	elem, ok := shard.index[key]
	if !ok {
		shard.mu.Unlock()
		return nil
	}
	entry, ok := elem.Value.(*cacheEntry)
	if !ok {
		shard.lruList.Remove(elem)
		delete(shard.index, key)
		shard.mu.Unlock()
		return fmt.Errorf("invalid cache entry for key %s", key)
	}
	filePath := entry.FilePath
	shard.lruList.Remove(elem)
	delete(shard.index, key)
	shard.currentSize -= entry.Size
	shard.mu.Unlock()
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Close stops background goroutines and waits for them to exit. Safe to call multiple times.
func (dc *DiskCache) Close() {
	dc.closeOnce.Do(func() {
		close(dc.stopChan)
		dc.wg.Wait()
	})
}

// Size returns the total cache size in bytes.
func (dc *DiskCache) Size() int64 {
	return dc.currentTotalSize()
}

// Len returns the number of cached entries.
func (dc *DiskCache) Len() int {
	var total int
	for _, s := range dc.shards {
		s.mu.RLock()
		total += len(s.index)
		s.mu.RUnlock()
	}
	return total
}

// findOldestEntry returns the least recently accessed entry across all shards.
func (dc *DiskCache) findOldestEntry() (*cacheShard, *cacheEntry) {
	var (
		oldestShard *cacheShard
		oldestEntry *cacheEntry
	)
	for _, s := range dc.shards {
		s.mu.RLock()
		front := s.lruList.Front()
		if front != nil {
			if e, ok := front.Value.(*cacheEntry); ok {
				if oldestShard == nil || e.AccessedAt.Before(oldestEntry.AccessedAt) {
					oldestShard = s
					oldestEntry = e
				}
			}
		}
		s.mu.RUnlock()
	}
	return oldestShard, oldestEntry
}

// evictOne removes entry from shard and returns freed bytes (0 if already gone).
func (dc *DiskCache) evictOne(shard *cacheShard, entry *cacheEntry) int64 {
	shard.mu.Lock()
	defer shard.mu.Unlock()
	elem, stillExists := shard.index[entry.Key]
	if !stillExists {
		return 0
	}
	liveEntry, ok := elem.Value.(*cacheEntry)
	if !ok {
		return 0
	}
	shard.lruList.Remove(elem)
	delete(shard.index, liveEntry.Key)
	shard.currentSize -= liveEntry.Size
	select {
	case dc.evictChan <- liveEntry.FilePath:
	default:
		os.Remove(liveEntry.FilePath) // queue full, delete synchronously
	}
	return liveEntry.Size
}

// evictIfNeeded evicts LRU entries across shards until there is room for newSize bytes.
// Uses totalMu to serialize eviction decisions across concurrent Set calls.
func (dc *DiskCache) evictIfNeeded(newSize int64) {
	dc.totalMu.Lock()
	defer dc.totalMu.Unlock()
	total := dc.currentTotalSize()
	for total+newSize > dc.maxSize {
		oldestShard, oldestEntry := dc.findOldestEntry()
		if oldestShard == nil {
			break
		}
		total -= dc.evictOne(oldestShard, oldestEntry)
	}
}

// evictWorker asynchronously deletes evicted files from disk.
func (dc *DiskCache) evictWorker() {
	defer dc.wg.Done()
	for {
		select {
		case filePath := <-dc.evictChan:
			os.Remove(filePath)
		case <-dc.stopChan:
			count := 0
			for {
				select {
				case filePath := <-dc.evictChan:
					os.Remove(filePath)
					count++
				default:
					if count > 0 {
						log.Printf("evictWorker: drained %d pending evictions on stop", count)
					}
					return
				}
			}
		}
	}
}

// walkCacheDir scans baseDir (one level deep), splitting files into valid entries
// and expired paths based on TTL.
func (dc *DiskCache) walkCacheDir(now time.Time) (entries []*cacheEntry, expiredPaths []string, err error) {
	err = filepath.Walk(dc.baseDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			log.Printf("loadIndex: skip file %s: %v", path, walkErr)
			return nil
		}
		if info.IsDir() {
			if path == dc.baseDir {
				return nil
			}
			return filepath.SkipDir
		}
		if now.Sub(info.ModTime()) > dc.ttl {
			expiredPaths = append(expiredPaths, path)
			return nil
		}
		key := filepath.Base(path)
		entries = append(entries, &cacheEntry{
			Key:        key,
			FilePath:   path,
			Size:       info.Size(),
			CreatedAt:  info.ModTime(),
			AccessedAt: info.ModTime(),
		})
		return nil
	})
	return entries, expiredPaths, err
}

// queueExpiredForDeletion sends expired paths to evictChan asynchronously.
func (dc *DiskCache) queueExpiredForDeletion(expiredPaths []string) {
	if len(expiredPaths) == 0 {
		return
	}
	log.Printf("loadIndex: found %d expired files, queuing for deletion", len(expiredPaths))
	go func() {
		for _, p := range expiredPaths {
			select {
			case dc.evictChan <- p:
			default:
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					log.Printf("loadIndex: failed to delete expired file %s: %v", p, err)
				}
			}
		}
	}()
}

// loadIndex rebuilds the in-memory index from disk on startup.
// Entries are sorted by modification time so the LRU list reflects file age.
// Expired files are excluded and queued for async deletion.
func (dc *DiskCache) loadIndex() error {
	entries, expiredPaths, err := dc.walkCacheDir(time.Now())
	dc.queueExpiredForDeletion(expiredPaths)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	for _, entry := range entries {
		shard := dc.getShard(entry.Key)
		elem := shard.lruList.PushBack(entry)
		shard.index[entry.Key] = elem
		shard.currentSize += entry.Size
	}
	return nil
}

// cleanupLoop periodically removes expired entries.
func (dc *DiskCache) cleanupLoop() {
	defer dc.wg.Done()
	ticker := time.NewTicker(dc.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			dc.cleanup()
		case <-dc.stopChan:
			return
		}
	}
}

// cleanup removes all expired entries using a two-phase approach:
// read lock to collect expired keys, then write lock to remove them.
func (dc *DiskCache) cleanup() {
	now := time.Now()
	for _, shard := range dc.shards {
		// Phase 1: collect expired keys under read lock.
		// MoveToBack disrupts CreatedAt order, so we must scan the full list.
		var expiredKeys []string
		shard.mu.RLock()
		for e := shard.lruList.Front(); e != nil; e = e.Next() {
			entry, ok := e.Value.(*cacheEntry)
			if !ok {
				continue
			}
			if now.Sub(entry.CreatedAt) > dc.ttl {
				expiredKeys = append(expiredKeys, entry.Key)
			}
		}
		shard.mu.RUnlock()
		if len(expiredKeys) == 0 {
			continue
		}
		// Phase 2: remove under write lock. Re-fetch elem by key to avoid stale references.
		var toEvict []string
		shard.mu.Lock()
		for _, key := range expiredKeys {
			elem, exists := shard.index[key]
			if !exists {
				continue
			}
			entry, ok := elem.Value.(*cacheEntry)
			if !ok {
				shard.lruList.Remove(elem)
				delete(shard.index, key)
				continue
			}
			if now.Sub(entry.CreatedAt) <= dc.ttl { // may have been refreshed by concurrent Set
				continue
			}
			shard.lruList.Remove(elem)
			delete(shard.index, key)
			shard.currentSize -= entry.Size
			toEvict = append(toEvict, entry.FilePath)
		}
		shard.mu.Unlock()
		for _, filePath := range toEvict {
			select {
			case dc.evictChan <- filePath:
			default:
				os.Remove(filePath)
			}
		}
	}
}

// getFilePath returns the disk path for key.
func (dc *DiskCache) getFilePath(key string) string {
	return filepath.Join(dc.baseDir, key)
}

// BuildCacheKey builds a cache key from a file URL and block index using MD5.
func BuildCacheKey(fileURL string, blockIndex uint64) string {
	hash := md5.Sum([]byte(fileURL))
	return fmt.Sprintf("%s_%d", hex.EncodeToString(hash[:]), blockIndex)
}
