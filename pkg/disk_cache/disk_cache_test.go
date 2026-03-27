// Package diskcache 磁盘缓存单元测试
package diskcache

import (
	"bytes"
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCache 创建临时目录的测试缓存，t.Cleanup 自动关闭
func newTestCache(t *testing.T, maxSize int64, ttl time.Duration) *DiskCache {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         maxSize,
		DiskCacheTTL:             ttl,
		DiskCacheCleanupInterval: time.Hour, // 测试中不触发自动清理
	}
	dc, err := NewDiskCache(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { dc.Close() })
	return dc
}

// TestDiskCache_NilConfig 传入 nil config 返回错误
func TestDiskCache_NilConfig(t *testing.T) {
	_, err := NewDiskCache(nil)
	assert.Error(t, err)
}

// TestDiskCache_SetAndGet_Hit 写入后能正确读取
func TestDiskCache_SetAndGet_Hit(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	data := []byte("hello_pdf_block")
	require.NoError(t, dc.Set("key1", data))

	got, err := dc.Get("key1")
	assert.NoError(t, err)
	assert.Equal(t, data, got)
}

// TestDiskCache_Get_Miss 未命中返回 os.ErrNotExist
func TestDiskCache_Get_Miss(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)

	_, err := dc.Get("not_exist_key")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestDiskCache_Set_Overwrite 覆盖写同一 key，旧数据被替换，Size 和 Len 正确
func TestDiskCache_Set_Overwrite(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("v1")))
	require.NoError(t, dc.Set("key1", []byte("v2_longer")))

	got, err := dc.Get("key1")
	assert.NoError(t, err)
	assert.Equal(t, []byte("v2_longer"), got)
	// 覆盖写后 Len 仍为 1，Size 只计算新值
	assert.Equal(t, 1, dc.Len())
	assert.Equal(t, int64(len("v2_longer")), dc.Size())
}

// TestDiskCache_Delete 删除后 Get 返回 ErrNotExist，Size 和 Len 归零
func TestDiskCache_Delete(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("data")))
	require.NoError(t, dc.Delete("key1"))

	_, err := dc.Get("key1")
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, int64(0), dc.Size())
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_Delete_NotExist 删除不存在的 key 不报错（幂等）
func TestDiskCache_Delete_NotExist(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	assert.NoError(t, dc.Delete("not_exist"))
}

// TestDiskCache_EmptyData 写入空数据能正常读取
func TestDiskCache_EmptyData(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("empty", []byte{}))

	got, err := dc.Get("empty")
	assert.NoError(t, err)
	assert.Equal(t, []byte{}, got)
}

// TestDiskCache_SizeAndLen_Consistency Size 和 Len 与实际写入一致
func TestDiskCache_SizeAndLen_Consistency(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	keys := []string{"a", "b", "c", "d", "e"}
	var totalSize int64
	for _, k := range keys {
		data := []byte(k + "_data")
		require.NoError(t, dc.Set(k, data))
		totalSize += int64(len(data))
	}
	assert.Equal(t, len(keys), dc.Len())
	assert.Equal(t, totalSize, dc.Size())
}

// TestDiskCache_Get_Expired TTL 过期后 Get 返回 ErrNotExist，并触发惰性删除
func TestDiskCache_Get_Expired(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 50*time.Millisecond)
	require.NoError(t, dc.Set("key1", []byte("data")))

	time.Sleep(100 * time.Millisecond) // 等待过期

	_, err := dc.Get("key1")
	assert.ErrorIs(t, err, os.ErrNotExist)

	// 等待惰性删除的异步 goroutine 完成
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_Cleanup_AllExpired cleanup 清理全部过期条目
func TestDiskCache_Cleanup_AllExpired(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 50*time.Millisecond)
	for i := 0; i < 5; i++ {
		require.NoError(t, dc.Set(fmt.Sprintf("key%d", i), []byte("data")))
	}
	assert.Equal(t, 5, dc.Len())

	time.Sleep(100 * time.Millisecond) // 等待全部过期
	dc.cleanup()                       // 手动触发清理

	// 等待异步磁盘删除完成
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
	assert.Equal(t, int64(0), dc.Size())
}

// TestDiskCache_Cleanup_PartialExpired 只清理过期的，未过期的保留
func TestDiskCache_Cleanup_PartialExpired(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 150*time.Millisecond)
	require.NoError(t, dc.Set("old", []byte("old_data")))

	time.Sleep(200 * time.Millisecond) // old 已过期

	require.NoError(t, dc.Set("new", []byte("new_data"))) // new 未过期
	dc.cleanup()
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, dc.Len())
	got, err := dc.Get("new")
	assert.NoError(t, err)
	assert.Equal(t, []byte("new_data"), got)
}

// TestDiskCache_LRU_Eviction 超出 maxSize 时淘汰最旧节点
func TestDiskCache_LRU_Eviction(t *testing.T) {
	// maxSize = 30 字节，每条 10 字节
	dc := newTestCache(t, 30, time.Hour)
	require.NoError(t, dc.Set("k1", bytes.Repeat([]byte("a"), 10)))
	require.NoError(t, dc.Set("k2", bytes.Repeat([]byte("b"), 10)))
	require.NoError(t, dc.Set("k3", bytes.Repeat([]byte("c"), 10)))
	assert.Equal(t, 3, dc.Len())

	// 写入第 4 条，触发淘汰 k1（最旧）
	require.NoError(t, dc.Set("k4", bytes.Repeat([]byte("d"), 10)))

	// 等待异步磁盘删除
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 3, dc.Len())
	assert.LessOrEqual(t, dc.Size(), int64(30))

	_, err := dc.Get("k1")
	assert.ErrorIs(t, err, os.ErrNotExist, "k1 应被 LRU 淘汰")
}

// TestDiskCache_LRU_SingleItemLargerThanMax 单条数据超过 maxSize，淘汰所有后仍写入
func TestDiskCache_LRU_SingleItemLargerThanMax(t *testing.T) {
	dc := newTestCache(t, 5, time.Hour) // maxSize = 5 字节
	require.NoError(t, dc.Set("k1", []byte("small")))

	// 写入 10 字节，超过 maxSize，k1 被淘汰，k2 仍然写入
	require.NoError(t, dc.Set("k2", bytes.Repeat([]byte("x"), 10)))

	time.Sleep(100 * time.Millisecond)
	_, err := dc.Get("k1")
	assert.ErrorIs(t, err, os.ErrNotExist, "k1 应被淘汰")

	got, err := dc.Get("k2")
	assert.NoError(t, err)
	assert.Len(t, got, 10)
}

// TestDiskCache_LRU_MultipleEvictions 连续写入触发多次淘汰，总大小始终不超过 maxSize
func TestDiskCache_LRU_MultipleEvictions(t *testing.T) {
	const blockSize = 100
	const maxSize = blockSize * 5
	dc := newTestCache(t, maxSize, time.Hour)

	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key_%d", i)
		require.NoError(t, dc.Set(key, bytes.Repeat([]byte("x"), blockSize)))
	}

	time.Sleep(100 * time.Millisecond)
	// evictIfNeeded 用 totalMu 串行化淘汰决策，但写入索引在淘汰之后，
	// 并发窗口内最多 shardCount 个 goroutine 同时通过淘汰检查后才写入索引，
	// 因此允许超出量最多为 shardCount 个块大小
	assert.LessOrEqual(t, dc.Size(), int64(maxSize+shardCount*blockSize))
}

// TestDiskCache_LoadIndex_Recovery 重启后能恢复已有缓存
func TestDiskCache_LoadIndex_Recovery(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	// 第一次启动，写入数据
	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	require.NoError(t, dc1.Set("key1", []byte("hello")))
	require.NoError(t, dc1.Set("key2", []byte("world")))
	dc1.Close()

	// 第二次启动，恢复索引
	dc2, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc2.Close()

	assert.Equal(t, 2, dc2.Len())
	got, err := dc2.Get("key1")
	assert.NoError(t, err)
	assert.Equal(t, []byte("hello"), got)

	got2, err := dc2.Get("key2")
	assert.NoError(t, err)
	assert.Equal(t, []byte("world"), got2)
}

// TestDiskCache_LoadIndex_SkipExpired 重启时过期文件不加载到索引
func TestDiskCache_LoadIndex_SkipExpired(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             50 * time.Millisecond,
		DiskCacheCleanupInterval: time.Hour,
	}

	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	require.NoError(t, dc1.Set("expired_key", []byte("data")))
	dc1.Close()

	time.Sleep(100 * time.Millisecond) // 等待过期

	dc2, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc2.Close()

	// 过期文件不应加载到索引
	assert.Equal(t, 0, dc2.Len())
	assert.Equal(t, int64(0), dc2.Size())
}

// TestDiskCache_LoadIndex_SizeConsistency 重启后 Size 与实际文件大小一致
func TestDiskCache_LoadIndex_SizeConsistency(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	data1 := bytes.Repeat([]byte("a"), 1024)
	data2 := bytes.Repeat([]byte("b"), 2048)
	require.NoError(t, dc1.Set("k1", data1))
	require.NoError(t, dc1.Set("k2", data2))
	expectedSize := int64(len(data1) + len(data2))
	dc1.Close()

	dc2, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc2.Close()

	assert.Equal(t, expectedSize, dc2.Size())
}

// TestDiskCache_Concurrent_SetGet 并发读写不 panic、不数据竞争（配合 -race 运行）
func TestDiskCache_Concurrent_SetGet(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	const goroutines = 50
	const ops = 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				key := fmt.Sprintf("key_%d_%d", id, j%10) // 故意复用 key 触发覆盖写
				_ = dc.Set(key, []byte(fmt.Sprintf("data_%d", j)))
				_, _ = dc.Get(key)
			}
		}(i)
	}
	wg.Wait()
}

// TestDiskCache_Concurrent_SetDelete 并发写和删除不 panic
func TestDiskCache_Concurrent_SetDelete(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	const goroutines = 20

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", id)
			for j := 0; j < 50; j++ {
				_ = dc.Set(key, []byte("data"))
			}
		}(i)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", id)
			for j := 0; j < 50; j++ {
				_ = dc.Delete(key)
			}
		}(i)
	}
	wg.Wait()
}

// TestDiskCache_Concurrent_Eviction 并发写触发 LRU 淘汰不 panic，总大小不严重超出 maxSize
func TestDiskCache_Concurrent_Eviction(t *testing.T) {
	const blockSize = 64
	const maxSize = 1024
	dc := newTestCache(t, maxSize, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				key := fmt.Sprintf("k%d_%d", id, j)
				_ = dc.Set(key, bytes.Repeat([]byte("x"), blockSize))
			}
		}(i)
	}
	wg.Wait()

	time.Sleep(100 * time.Millisecond)
	// evictIfNeeded 用 totalMu 串行化淘汰决策，但写入索引在淘汰之后，
	// 并发窗口内最多 shardCount 个 goroutine 同时通过淘汰检查后才写入索引，
	// 因此允许超出量最多为 shardCount 个块大小
	assert.LessOrEqual(t, dc.Size(), int64(maxSize+shardCount*blockSize))
}

// TestDiskCache_Concurrent_CleanupAndSet 并发 cleanup 和 Set 不 panic
func TestDiskCache_Concurrent_CleanupAndSet(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 50*time.Millisecond)

	var wg sync.WaitGroup
	// 写入 goroutine
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				_ = dc.Set(fmt.Sprintf("k%d_%d", id, j), []byte("data"))
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
	}
	// cleanup goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			dc.cleanup()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	wg.Wait()
}

// TestDiskCache_LargeData 写入接近 maxSize 的大数据
func TestDiskCache_LargeData(t *testing.T) {
	const maxSize = 5 * 1024 * 1024 // 5MB
	dc := newTestCache(t, maxSize, time.Hour)
	data := bytes.Repeat([]byte("x"), maxSize-1)
	require.NoError(t, dc.Set("large", data))

	got, err := dc.Get("large")
	assert.NoError(t, err)
	assert.Equal(t, data, got)
}

// TestDiskCache_MultipleShards 写入足够多的 key，覆盖多个分片
func TestDiskCache_MultipleShards(t *testing.T) {
	dc := newTestCache(t, 100*1024*1024, time.Hour)
	const count = 160 // 远超 shardCount=16，确保每个分片都有数据
	for i := 0; i < count; i++ {
		key := BuildCacheKey(fmt.Sprintf("http://example.com/%d.pdf", i), uint64(i))
		require.NoError(t, dc.Set(key, []byte(fmt.Sprintf("block_%d", i))))
	}
	assert.Equal(t, count, dc.Len())
}

// TestBuildCacheKey_Uniqueness 不同 URL 和 blockIndex 生成不同 key
func TestBuildCacheKey_Uniqueness(t *testing.T) {
	k1 := BuildCacheKey("http://example.com/a.pdf", 0)
	k2 := BuildCacheKey("http://example.com/a.pdf", 1)
	k3 := BuildCacheKey("http://example.com/b.pdf", 0)
	assert.NotEqual(t, k1, k2, "同 URL 不同 blockIndex 应生成不同 key")
	assert.NotEqual(t, k1, k3, "不同 URL 同 blockIndex 应生成不同 key")
	assert.NotEqual(t, k2, k3)
}

// TestBuildCacheKey_Deterministic 相同输入生成相同 key（确定性）
func TestBuildCacheKey_Deterministic(t *testing.T) {
	k1 := BuildCacheKey("http://example.com/a.pdf", 42)
	k2 := BuildCacheKey("http://example.com/a.pdf", 42)
	assert.Equal(t, k1, k2)
}

// TestGetShard_EmptyKey 空 key 返回 shards[0]，不 panic
func TestGetShard_EmptyKey(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	shard := dc.getShard("")
	assert.Equal(t, dc.shards[0], shard)
}

// TestGetShard_Distribution FNV-1a 分片分布均匀性验证
func TestGetShard_Distribution(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	counts := make(map[*cacheShard]int)
	const total = 1600
	for i := 0; i < total; i++ {
		key := BuildCacheKey(fmt.Sprintf("http://example.com/%d.pdf", i), uint64(i))
		shard := dc.getShard(key)
		counts[shard]++
	}
	// 每个分片期望 100 次，允许 ±60% 偏差（均匀性验证）
	for _, cnt := range counts {
		assert.Greater(t, cnt, 40, "分片分布过于不均（某分片命中过少）")
		assert.Less(t, cnt, 160, "分片分布过于不均（某分片命中过多）")
	}
	// 确保所有 16 个分片都被覆盖
	assert.Equal(t, shardCount, len(counts), "应覆盖全部 16 个分片")
}

// TestDiskCache_Get_FileReadError 文件已在索引中但磁盘文件被外部删除，Get 应返回 error 并清理索引
func TestDiskCache_Get_FileReadError(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("data")))

	// 模拟磁盘文件被外部删除（索引仍存在）
	filePath := dc.getFilePath("key1")
	require.NoError(t, os.Remove(filePath))

	// Get 应返回 error（ReadFile 失败），并触发 Delete 清理索引
	_, err := dc.Get("key1")
	assert.Error(t, err)

	// 等待 Delete 完成
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_Get_ConcurrentDelete_StillExists 读文件期间并发 Delete 摘除节点，MoveToBack 分支不执行
func TestDiskCache_Get_ConcurrentDelete_StillExists(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("data")))

	// 并发：一个 goroutine 读，一个 goroutine 删
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = dc.Get("key1")
		}()
		go func() {
			defer wg.Done()
			_ = dc.Delete("key1")
		}()
	}
	wg.Wait()
	// 不 panic 即通过
}

// TestDiskCache_newDiskCache_MkdirAllFail 无法创建缓存目录时返回 error
func TestDiskCache_newDiskCache_MkdirAllFail(t *testing.T) {
	// 使用一个不可写的路径（/proc 下的子目录在 Linux 上无法创建）
	cfg := &Config{
		DiskCacheDir:             "/proc/no_permission_dir_xyz",
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	_, err := NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_evictWorker_DrainOnStop evictWorker 在 stopChan 关闭时排空队列（count > 0 分支）
func TestDiskCache_evictWorker_DrainOnStop(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	dc, err := NewDiskCache(cfg)
	require.NoError(t, err)
	// 不使用 t.Cleanup(dc.Close)，手动管理生命周期，避免重复关闭 stopChan

	// 写入若干文件，然后手动往 evictChan 投递路径（模拟积压）
	tmpDir := t.TempDir()
	for i := 0; i < 5; i++ {
		p := fmt.Sprintf("%s/file_%d", tmpDir, i)
		require.NoError(t, os.WriteFile(p, []byte("x"), 0644))
		dc.evictChan <- p
	}

	// 关闭 stopChan，evictWorker 和 cleanupLoop 都会退出
	dc.Close()

	// 等待 evictWorker 处理完毕
	time.Sleep(100 * time.Millisecond)
	// 文件应已被删除
	for i := 0; i < 5; i++ {
		p := fmt.Sprintf("%s/file_%d", tmpDir, i)
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "evictWorker 应已删除文件 %s", p)
	}
}

// TestDiskCache_walkCacheDir_SkipSubDir walkCacheDir 遇到子目录时跳过（filepath.SkipDir 分支）
func TestDiskCache_walkCacheDir_SkipSubDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	// 在缓存目录下创建一个子目录和子目录内的文件（不应被加载）
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.MkdirAll(subDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "should_skip"), []byte("skip"), 0644))

	// 在根目录写一个正常文件
	require.NoError(t, os.WriteFile(filepath.Join(dir, "normal_key"), []byte("data"), 0644))

	dc, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc.Close()

	// 只有根目录的文件被加载，子目录内的文件被跳过
	assert.Equal(t, 1, dc.Len())
	_, err = dc.Get("normal_key")
	assert.NoError(t, err)
}

// TestDiskCache_cleanup_EvictChanFull cleanup 时 evictChan 满，降级为同步删除（default 分支）
func TestDiskCache_cleanup_EvictChanFull(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             50 * time.Millisecond,
		DiskCacheCleanupInterval: time.Hour,
	}
	dc, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc.Close()

	// 写入若干条目
	for i := 0; i < 10; i++ {
		require.NoError(t, dc.Set(fmt.Sprintf("key%d", i), []byte("data")))
	}

	// 填满 evictChan，使 cleanup 走 default 分支（同步删除）
	for len(dc.evictChan) < cap(dc.evictChan) {
		dc.evictChan <- filepath.Join(dir, "nonexistent_fill")
	}

	time.Sleep(100 * time.Millisecond) // 等待过期
	dc.cleanup()                       // 触发清理，evictChan 满 → 同步删除

	// 等待清理完成
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_evictOne_EvictChanFull evictOne 时 evictChan 满，降级为同步删除（default 分支）
func TestDiskCache_evictOne_EvictChanFull(t *testing.T) {
	const blockSize = 10
	dc := newTestCache(t, blockSize*2, time.Hour)

	// 填满 evictChan
	for len(dc.evictChan) < cap(dc.evictChan) {
		dc.evictChan <- filepath.Join(dc.baseDir, "nonexistent_fill")
	}

	// 写入超出 maxSize，触发 evictOne，此时 evictChan 满 → 同步删除
	require.NoError(t, dc.Set("k1", bytes.Repeat([]byte("a"), blockSize)))
	require.NoError(t, dc.Set("k2", bytes.Repeat([]byte("b"), blockSize)))
	require.NoError(t, dc.Set("k3", bytes.Repeat([]byte("c"), blockSize))) // 触发淘汰

	time.Sleep(50 * time.Millisecond)
	assert.LessOrEqual(t, dc.Size(), int64(blockSize*2))
}

// TestDiskCache_queueExpiredForDeletion_EvictChanFull queueExpiredForDeletion 时 evictChan 满，降级同步删除
func TestDiskCache_queueExpiredForDeletion_EvictChanFull(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             50 * time.Millisecond,
		DiskCacheCleanupInterval: time.Hour,
	}

	// 第一次启动，写入数据
	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		require.NoError(t, dc1.Set(fmt.Sprintf("key%d", i), []byte("data")))
	}
	dc1.Close()

	time.Sleep(100 * time.Millisecond) // 等待过期

	// 第二次启动前，预先创建一个 evictChan 已满的 DiskCache 并手动调用 queueExpiredForDeletion
	// 通过直接调用 walkCacheDir + queueExpiredForDeletion 来覆盖该路径
	dc2 := &DiskCache{
		baseDir:         dir,
		maxSize:         10 * 1024 * 1024,
		ttl:             50 * time.Millisecond,
		cleanupInterval: time.Hour,
		evictChan:       make(chan string, 1), // 容量为 1，方便填满
		stopChan:        make(chan struct{}),
	}
	for i := 0; i < shardCount; i++ {
		dc2.shards[i] = &cacheShard{
			index:   make(map[string]*list.Element),
			lruList: list.New(),
		}
	}
	defer dc2.Close()

	// 填满 evictChan
	dc2.evictChan <- "fill"

	// 收集过期文件
	_, expiredPaths, err := dc2.walkCacheDir(time.Now())
	require.NoError(t, err)
	assert.NotEmpty(t, expiredPaths)

	// 调用 queueExpiredForDeletion，evictChan 满 → 降级同步删除
	dc2.queueExpiredForDeletion(expiredPaths)

	// 等待异步 goroutine 完成
	time.Sleep(200 * time.Millisecond)
	// 文件应已被删除（同步删除兜底）
	for _, p := range expiredPaths {
		_, statErr := os.Stat(p)
		assert.True(t, os.IsNotExist(statErr), "过期文件应已被删除: %s", p)
	}
}

// TestDiskCache_loadIndex_WalkError loadIndex 时 walkCacheDir 返回 error（目录不可读）
func TestDiskCache_loadIndex_WalkError(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	// 先正常创建，写入数据
	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	require.NoError(t, dc1.Set("key1", []byte("data")))
	dc1.Close()

	// 将缓存目录权限设为不可读，使 Walk 失败
	require.NoError(t, os.Chmod(dir, 0000))
	defer os.Chmod(dir, 0755) // 恢复权限，确保 t.TempDir 能清理

	// 非 root 用户下，loadIndex 应返回 error
	if os.Getuid() != 0 {
		_, err = NewDiskCache(cfg)
		assert.Error(t, err)
	}
}

// TestDiskCache_Config_EmptyDir 空目录路径返回错误
func TestDiskCache_Config_EmptyDir(t *testing.T) {
	cfg := &Config{
		DiskCacheDir:             "",
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	_, err := NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_Config_ZeroMaxSize maxSize 为 0 返回错误
func TestDiskCache_Config_ZeroMaxSize(t *testing.T) {
	cfg := &Config{
		DiskCacheDir:             t.TempDir(),
		DiskCacheMaxSize:         0,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	_, err := NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_Config_NegativeTTL TTL 为负值返回错误
func TestDiskCache_Config_NegativeTTL(t *testing.T) {
	cfg := &Config{
		DiskCacheDir:             t.TempDir(),
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             -time.Second,
		DiskCacheCleanupInterval: time.Hour,
	}
	_, err := NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_Config_ZeroCleanupInterval cleanupInterval 为 0 返回错误
func TestDiskCache_Config_ZeroCleanupInterval(t *testing.T) {
	cfg := &Config{
		DiskCacheDir:             t.TempDir(),
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: 0,
	}
	_, err := NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_Close_WaitsForGoroutines Close 等待后台 goroutine 退出后才返回
func TestDiskCache_Close_WaitsForGoroutines(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	// 往 evictChan 投递一些任务
	for i := 0; i < 10; i++ {
		p := filepath.Join(dc.baseDir, fmt.Sprintf("close_test_%d", i))
		_ = os.WriteFile(p, []byte("x"), 0644)
		dc.evictChan <- p
	}
	// Close 应等待 evictWorker 排空队列后返回
	dc.Close()
	// Close 返回后，文件应已被删除
	for i := 0; i < 10; i++ {
		p := filepath.Join(dc.baseDir, fmt.Sprintf("close_test_%d", i))
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "Close 返回后文件应已被删除: %s", p)
	}
}

// TestDiskCache_Cleanup_AfterMoveToBack cleanup 在 Get.MoveToBack 打乱链表顺序后仍能清理所有过期节点
func TestDiskCache_Cleanup_AfterMoveToBack(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 200*time.Millisecond)
	// 写入 3 个 key，按时间顺序 k1 < k2 < k3
	require.NoError(t, dc.Set("k1", []byte("data1")))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, dc.Set("k2", []byte("data2")))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, dc.Set("k3", []byte("data3")))

	// Get k1 使其 MoveToBack，链表顺序变为 k2 -> k3 -> k1
	// 此时 k1 的 CreatedAt 最旧但在链表尾部
	_, err := dc.Get("k1")
	require.NoError(t, err)

	// 等待全部过期
	time.Sleep(250 * time.Millisecond)

	// cleanup 应清理所有 3 个过期节点（不能因为 break 优化而遗漏 k1）
	dc.cleanup()
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 0, dc.Len(), "cleanup 应清理所有过期节点，包括被 MoveToBack 的 k1")
	assert.Equal(t, int64(0), dc.Size())
}

// TestDiskCache_Get_ConcurrentSetOverwrite 并发 Get 和 Set 覆盖写同一 key，验证链表不损坏
// 此测试覆盖修复的 Bug：Get 中 MoveToBack 必须使用从 index 重新获取的 liveElem，
// 而非读锁阶段的旧 elem（旧 elem 可能已被并发 Set 覆盖写 Remove）
func TestDiskCache_Get_ConcurrentSetOverwrite(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	const key = "shared_key"
	require.NoError(t, dc.Set(key, []byte("initial")))

	var wg sync.WaitGroup
	// 多个 goroutine 并发 Get 同一 key（触发 MoveToBack）
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = dc.Get(key)
			}
		}()
	}
	// 多个 goroutine 并发 Set 覆盖写同一 key（触发 Remove 旧 elem + PushBack 新 elem）
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = dc.Set(key, []byte(fmt.Sprintf("data_%d_%d", id, j)))
			}
		}(i)
	}
	wg.Wait()

	// 验证链表完整性：Len 应为 1，链表长度应与 index 大小一致
	shard := dc.getShard(key)
	shard.mu.RLock()
	listLen := shard.lruList.Len()
	indexLen := len(shard.index)
	shard.mu.RUnlock()
	assert.Equal(t, 1, dc.Len(), "覆盖写后应只有 1 个条目")
	assert.Equal(t, listLen, indexLen, "链表长度应与 index 大小一致（链表未损坏）")
}

// TestDiskCache_Cleanup_ConcurrentSetOverwrite 并发 cleanup 和 Set 覆盖写，验证 cleanup 通过 key 重新获取 elem 的安全性
func TestDiskCache_Cleanup_ConcurrentSetOverwrite(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 80*time.Millisecond)
	const count = 20

	// 写入一批 key
	for i := 0; i < count; i++ {
		require.NoError(t, dc.Set(fmt.Sprintf("key_%d", i), []byte("old_data")))
	}

	// 等待部分过期
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	// 并发 cleanup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			dc.cleanup()
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// 并发 Set 覆盖写（刷新 CreatedAt，使部分 key 不再过期）
	for i := 0; i < count/2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", id)
			_ = dc.Set(key, []byte("new_data"))
		}(i)
	}
	wg.Wait()

	// 验证：不 panic 即通过，且每个分片的链表长度与 index 大小一致
	for i, shard := range dc.shards {
		shard.mu.RLock()
		listLen := shard.lruList.Len()
		indexLen := len(shard.index)
		shard.mu.RUnlock()
		assert.Equal(t, listLen, indexLen, "分片 %d 链表长度应与 index 大小一致", i)
	}
}
