// Package diskcache 提供高性能的磁盘缓存实现，支持LRU淘汰策略和TTL过期机制
// 采用分片锁 + 双向链表 + map 实现 O(1) LRU 淘汰，磁盘删除异步化
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
	shardCount    = 16   // 分片数，必须为 2 的幂，方便位运算取模
	evictChanSize = 512  // 异步磁盘删除队列缓冲区大小
	filePermMode  = 0644 // 缓存文件权限
	dirPermMode   = 0755 // 缓存目录权限
	fnvHashBytes  = 8    // FNV-1a 哈希取 key 前 N 字节
)

// cacheShard 表示单个分片，包含独立的 LRU 链表 + map + 读写锁。
type cacheShard struct {
	mu          sync.RWMutex
	index       map[string]*list.Element
	lruList     *list.List
	currentSize int64
}

// DiskCache 提供基于磁盘的缓存实现。
// 采用分片锁 + 双向链表 + map 实现 O(1) LRU 淘汰，磁盘删除异步化。
//  1. 分片锁（shardCount=16）：降低并发写锁竞争。
//  2. Set 写磁盘在锁外执行：消除写磁盘时阻塞读写路径的问题。
//  3. Get 读到过期数据时异步触发删除：惰性清理，减少过期数据占用磁盘。
type DiskCache struct {
	baseDir         string                  // 缓存根目录
	maxSize         int64                   // 最大缓存总大小（字节）
	ttl             time.Duration           // 缓存条目生存时间，用新建时间对比
	cleanupInterval time.Duration           // 后台清理间隔
	shards          [shardCount]*cacheShard // 分片数组
	totalMu         sync.Mutex              // 串行化跨分片淘汰决策，避免多个 Set 并发时重复淘汰
	evictChan       chan string             // 异步磁盘删除队列
	stopChan        chan struct{}           // 停止信号
	wg              sync.WaitGroup          // 等待后台 goroutine 退出
	closeOnce       sync.Once               // 保证 Close 只执行一次
}

// cacheEntry 表示磁盘缓存条目的元数据。
type cacheEntry struct {
	Key        string    // 缓存键
	FilePath   string    // 文件路径
	Size       int64     // 文件大小
	CreatedAt  time.Time // 创建时间
	AccessedAt time.Time // 访问时间（由链表位置隐式表达，保留用于 TTL 清理）
}

// Config 定义磁盘缓存的配置参数。
type Config struct {
	DiskCacheDir             string        // 缓存目录
	DiskCacheMaxSize         int64         // 最大缓存大小
	DiskCacheTTL             time.Duration // 缓存生存时间
	DiskCacheCleanupInterval time.Duration // 清理间隔
}

// NewDiskCache 创建并返回一个新的磁盘缓存实例。
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
		// 缓冲区大小按需调整，避免后台删除跟不上时阻塞写路径
		evictChan: make(chan string, evictChanSize),
		stopChan:  make(chan struct{}),
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
	go dc.cleanupLoop() // 后台定时清理过期数据，写锁内只有纯内存操作，不影响读写性能
	go dc.evictWorker() // 后台异步删除磁盘文件，不持锁不影响读写性能
	return dc, nil
}

// getShard 根据 key 获取对应分片（FNV-1a 哈希取模，O(1)）。
// key 格式为 "{md5hex}_{blockIndex}"，md5hex 首字节只有 16 种可能值（'0'-'9','a'-'f'），
// 直接取首字节低 4 位会导致分片 10~15 永远为空，分布严重不均。
// 改用 FNV-1a 对 key 前 8 字节做哈希，所有 16 个分片均匀分布。
func (dc *DiskCache) getShard(key string) *cacheShard {
	if len(key) == 0 {
		return dc.shards[0]
	}
	// FNV-1a 32位哈希：对 key 前 8 字节（或全部字节，取较小值）做哈希
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

// currentTotalSize 遍历所有分片，返回精确的当前缓存总大小。
func (dc *DiskCache) currentTotalSize() int64 {
	var total int64
	for _, s := range dc.shards {
		s.mu.RLock()
		total += s.currentSize
		s.mu.RUnlock()
	}
	return total
}

// Get 获取缓存数据，缓存未命中或已过期返回 error。
func (dc *DiskCache) Get(key string) ([]byte, error) {
	shard := dc.getShard(key)
	// 在读锁内取出 entry 的不可变字段副本，避免锁外访问 elem.Value 的竞态
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
	// 在读锁内复制不可变字段，锁外安全使用
	createdAt := entry.CreatedAt
	filePath := entry.FilePath
	shard.mu.RUnlock()

	// 检查是否过期
	if time.Since(createdAt) > dc.ttl {
		// 过期时异步触发删除（惰性清理），不阻塞读路径
		go func() { _ = dc.Delete(key) }()
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		_ = dc.Delete(key)
		return nil, err
	}
	// 命中：将节点移到链表尾部（O(1)），更新访问时间
	// 必须重新从 index 获取 elem（防止读文件期间并发 Set 覆盖写替换了 elem，
	// 对已从链表中 Remove 的旧 elem 调用 MoveToBack 会导致链表损坏）
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

// Set 写入缓存数据，空间不足时自动 LRU 淘汰（O(1) 淘汰，磁盘删除异步）。
// 写磁盘在锁外执行，消除写磁盘时阻塞读写路径的问题
func (dc *DiskCache) Set(key string, data []byte) error {
	dataSize := int64(len(data))
	// 先在全局锁下做 LRU 淘汰决策（跨分片），释放后再写磁盘
	dc.evictIfNeeded(dataSize)
	filePath := dc.getFilePath(key)
	// 写磁盘在锁外执行，不持有任何分片锁
	if err := os.WriteFile(filePath, data, filePermMode); err != nil {
		return fmt.Errorf("write cache file %s: %w", filePath, err)
	}
	// 写磁盘成功后，加锁更新内存索引
	shard := dc.getShard(key)
	now := time.Now()
	shard.mu.Lock()
	// 若 key 已存在，先从链表中移除旧节点
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
	// 新节点追加到链表尾部（最新）
	elem := shard.lruList.PushBack(entry)
	shard.index[key] = elem
	shard.currentSize += dataSize
	shard.mu.Unlock()
	return nil
}

// Delete 删除指定缓存条目。
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
	// 磁盘删除在锁外执行
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Close 关闭磁盘缓存，停止后台清理并等待后台 goroutine 退出（可安全重复调用）。
func (dc *DiskCache) Close() {
	dc.closeOnce.Do(func() {
		close(dc.stopChan)
		dc.wg.Wait()
	})
}

// Size 返回当前缓存总大小（字节）。
func (dc *DiskCache) Size() int64 {
	return dc.currentTotalSize()
}

// Len 返回缓存条目数。
func (dc *DiskCache) Len() int {
	var total int
	for _, s := range dc.shards {
		s.mu.RLock()
		total += len(s.index)
		s.mu.RUnlock()
	}
	return total
}

// findOldestEntry 在所有分片中找到链表头部最旧的节点（近似全局 LRU）。
// 链表头部（Front）即最久未访问节点（MoveToBack 维护），直接比较 AccessedAt 即可
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

// evictOne 从指定分片中摘除最旧节点，返回释放的字节数（0 表示节点已被并发删除）。
// 二次确认节点仍在（防止并发删除导致重复操作），重新从 elem.Value 取 entry 避免 TOCTOU
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
	// 非阻塞发送到异步删除队列；若队列满则降级为同步删除
	select {
	case dc.evictChan <- liveEntry.FilePath:
	default:
		os.Remove(liveEntry.FilePath)
	}
	return liveEntry.Size
}

// evictIfNeeded 按 LRU 策略跨分片淘汰旧数据，直到有足够空间存放 newSize 字节。
// 使用全局 totalMu 串行化淘汰决策，避免多个 Set 并发时重复淘汰
// 淘汰只操作内存，磁盘删除通过 evictChan 异步完成
func (dc *DiskCache) evictIfNeeded(newSize int64) {
	dc.totalMu.Lock()
	defer dc.totalMu.Unlock()
	// 循环开始前计算一次总大小，淘汰后增量更新，避免每次循环都遍历 16 个分片
	total := dc.currentTotalSize()
	for total+newSize > dc.maxSize {
		oldestShard, oldestEntry := dc.findOldestEntry()
		if oldestShard == nil {
			break // 所有分片均为空，无法继续淘汰
		}
		// 增量更新 total，避免下次循环重新遍历所有分片
		total -= dc.evictOne(oldestShard, oldestEntry)
	}
}

// evictWorker 后台 goroutine，异步执行磁盘文件删除。
func (dc *DiskCache) evictWorker() {
	defer dc.wg.Done()
	for {
		select {
		case filePath := <-dc.evictChan:
			os.Remove(filePath)
		case <-dc.stopChan:
			// 排空队列后退出，记录排空数量方便排查积压情况
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

// walkCacheDir 遍历缓存目录，将有效文件收集到 entries，过期文件收集到 expiredPaths。
// 只遍历根目录一层（跳过子目录），以文件修改时间作为 CreatedAt
func (dc *DiskCache) walkCacheDir(now time.Time) (entries []*cacheEntry, expiredPaths []string, err error) {
	err = filepath.Walk(dc.baseDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// 跳过单个文件错误（如并发删除），避免中断整个 Walk 导致孤儿文件
			log.Printf("loadIndex: skip file %s: %v", path, walkErr)
			return nil
		}
		if info.IsDir() {
			// 只遍历根目录一层，Set 写入时所有文件平铺在 baseDir，不创建子目录
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

// queueExpiredForDeletion 将过期文件路径异步投递到 evictChan，不阻塞启动流程。
// 复用 evictWorker 队列，避免额外 goroutine 与 evictWorker 并发竞争磁盘 I/O
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
				// 队列满时降级为同步删除兜底
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					log.Printf("loadIndex: failed to delete expired file %s: %v", p, err)
				}
			}
		}
	}()
}

// loadIndex 启动时扫描缓存目录，重建内存索引。
// 按文件修改时间升序排序后插入链表，使 LRU 顺序与文件新旧顺序一致
// 加载时过滤已过期文件：避免重启后过期数据占用 currentSize，导致 evictIfNeeded 误判
func (dc *DiskCache) loadIndex() error {
	entries, expiredPaths, err := dc.walkCacheDir(time.Now())
	// 异步删除已过期文件，不阻塞启动流程
	dc.queueExpiredForDeletion(expiredPaths)
	if err != nil {
		return err
	}
	// 按修改时间升序排序：最旧的在链表头部（优先被 LRU 淘汰），最新的在尾部
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	// loadIndex 在单线程初始化阶段调用，无需加锁
	for _, entry := range entries {
		shard := dc.getShard(entry.Key)
		elem := shard.lruList.PushBack(entry)
		shard.index[entry.Key] = elem
		shard.currentSize += entry.Size
	}
	return nil
}

// cleanupLoop 后台定时清理过期缓存。
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

// cleanup 清理所有过期条目。
// 两阶段处理：读锁收集过期 key → 写锁删除内存结构 → 锁外异步删除磁盘文件
// 相比直接持写锁遍历整个链表，读锁阶段不阻塞并发的 Get/Set，
// 写锁阶段只做纯内存操作（Remove/delete），持锁时间极短
func (dc *DiskCache) cleanup() {
	now := time.Now()
	for _, shard := range dc.shards {
		// 第一阶段：读锁收集过期 key（只记录 key，不持有 elem 引用）
		// 注意：链表初始按 CreatedAt 升序排列，但 Get 命中时 MoveToBack 会打乱顺序，
		// 因此不能 break 提前终止，必须遍历整个链表以确保不遗漏过期节点
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
		// 第二阶段：写锁删除内存结构
		// 通过 key 重新从 index 获取 elem（防止读锁释放后并发 Set 替换了 elem，
		// 对已 Remove 的旧 elem 操作会导致链表损坏）
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
			// 二次确认仍然过期（防止并发 Set 覆盖写更新了 CreatedAt）
			if now.Sub(entry.CreatedAt) <= dc.ttl {
				continue
			}
			shard.lruList.Remove(elem)
			delete(shard.index, key)
			shard.currentSize -= entry.Size
			toEvict = append(toEvict, entry.FilePath)
		}
		shard.mu.Unlock()
		// 锁外发送，不阻塞读写路径；队列满时降级为同步删除兜底
		for _, filePath := range toEvict {
			select {
			case dc.evictChan <- filePath:
			default:
				os.Remove(filePath)
			}
		}
	}
}

// getFilePath 根据 key 生成缓存文件路径。
func (dc *DiskCache) getFilePath(key string) string {
	return filepath.Join(dc.baseDir, key)
}

// BuildCacheKey 根据文件 URL 和块索引构建缓存键。
// 使用 MD5 对 URL 做哈希，避免文件名过长或含特殊字符
func BuildCacheKey(fileURL string, blockIndex uint64) string {
	hash := md5.Sum([]byte(fileURL))
	return fmt.Sprintf("%s_%d", hex.EncodeToString(hash[:]), blockIndex)
}
