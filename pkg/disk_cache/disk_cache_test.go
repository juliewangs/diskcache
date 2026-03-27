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

// newTestCache creates a test DiskCache with auto-cleanup.
func newTestCache(t *testing.T, maxSize int64, ttl time.Duration) *DiskCache {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         maxSize,
		DiskCacheTTL:             ttl,
		DiskCacheCleanupInterval: time.Hour, // Disable auto-cleanup in tests
	}
	dc, err := NewDiskCache(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { dc.Close() })
	return dc
}

// TestDiskCache_NilConfig tests nil config rejection.
func TestDiskCache_NilConfig(t *testing.T) {
	_, err := NewDiskCache(nil)
	assert.Error(t, err)
}

// TestDiskCache_SetAndGet_Hit tests basic set-then-get.
func TestDiskCache_SetAndGet_Hit(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	data := []byte("hello_pdf_block")
	require.NoError(t, dc.Set("key1", data))

	got, err := dc.Get("key1")
	assert.NoError(t, err)
	assert.Equal(t, data, got)
}

// TestDiskCache_Get_Miss tests cache miss returns ErrNotExist.
func TestDiskCache_Get_Miss(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)

	_, err := dc.Get("not_exist_key")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestDiskCache_Set_Overwrite tests overwriting the same key.
func TestDiskCache_Set_Overwrite(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("v1")))
	require.NoError(t, dc.Set("key1", []byte("v2_longer")))

	got, err := dc.Get("key1")
	assert.NoError(t, err)
	assert.Equal(t, []byte("v2_longer"), got)
	assert.Equal(t, 1, dc.Len())
	assert.Equal(t, int64(len("v2_longer")), dc.Size())
}

// TestDiskCache_Delete tests deletion clears entry and counters.
func TestDiskCache_Delete(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("data")))
	require.NoError(t, dc.Delete("key1"))

	_, err := dc.Get("key1")
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, int64(0), dc.Size())
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_Delete_NotExist tests deleting a non-existent key is idempotent.
func TestDiskCache_Delete_NotExist(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	assert.NoError(t, dc.Delete("not_exist"))
}

// TestDiskCache_EmptyData tests storing and retrieving empty data.
func TestDiskCache_EmptyData(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("empty", []byte{}))

	got, err := dc.Get("empty")
	assert.NoError(t, err)
	assert.Equal(t, []byte{}, got)
}

// TestDiskCache_SizeAndLen_Consistency tests Size/Len accuracy.
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

// TestDiskCache_Get_Expired tests TTL expiration triggers lazy deletion.
func TestDiskCache_Get_Expired(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 50*time.Millisecond)
	require.NoError(t, dc.Set("key1", []byte("data")))

	time.Sleep(100 * time.Millisecond)

	_, err := dc.Get("key1")
	assert.ErrorIs(t, err, os.ErrNotExist)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_Cleanup_AllExpired tests cleanup removes all expired entries.
func TestDiskCache_Cleanup_AllExpired(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 50*time.Millisecond)
	for i := 0; i < 5; i++ {
		require.NoError(t, dc.Set(fmt.Sprintf("key%d", i), []byte("data")))
	}
	assert.Equal(t, 5, dc.Len())

	time.Sleep(100 * time.Millisecond)
	dc.cleanup()

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
	assert.Equal(t, int64(0), dc.Size())
}

// TestDiskCache_Cleanup_PartialExpired tests only expired entries are removed.
func TestDiskCache_Cleanup_PartialExpired(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 150*time.Millisecond)
	require.NoError(t, dc.Set("old", []byte("old_data")))

	time.Sleep(200 * time.Millisecond)

	require.NoError(t, dc.Set("new", []byte("new_data")))
	dc.cleanup()
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, dc.Len())
	got, err := dc.Get("new")
	assert.NoError(t, err)
	assert.Equal(t, []byte("new_data"), got)
}

// TestDiskCache_LRU_Eviction tests LRU eviction when maxSize is exceeded.
func TestDiskCache_LRU_Eviction(t *testing.T) {
	// maxSize = 30 bytes, each entry is 10 bytes
	dc := newTestCache(t, 30, time.Hour)
	require.NoError(t, dc.Set("k1", bytes.Repeat([]byte("a"), 10)))
	require.NoError(t, dc.Set("k2", bytes.Repeat([]byte("b"), 10)))
	require.NoError(t, dc.Set("k3", bytes.Repeat([]byte("c"), 10)))
	assert.Equal(t, 3, dc.Len())

	require.NoError(t, dc.Set("k4", bytes.Repeat([]byte("d"), 10)))

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 3, dc.Len())
	assert.LessOrEqual(t, dc.Size(), int64(30))

	_, err := dc.Get("k1")
	assert.ErrorIs(t, err, os.ErrNotExist, "k1 should be evicted by LRU")
}

// TestDiskCache_LRU_SingleItemLargerThanMax tests writing data larger than maxSize.
func TestDiskCache_LRU_SingleItemLargerThanMax(t *testing.T) {
	dc := newTestCache(t, 5, time.Hour) // maxSize = 5 bytes
	require.NoError(t, dc.Set("k1", []byte("small")))

	require.NoError(t, dc.Set("k2", bytes.Repeat([]byte("x"), 10)))

	time.Sleep(100 * time.Millisecond)
	_, err := dc.Get("k1")
	assert.ErrorIs(t, err, os.ErrNotExist, "k1 should be evicted")

	got, err := dc.Get("k2")
	assert.NoError(t, err)
	assert.Len(t, got, 10)
}

// TestDiskCache_LRU_MultipleEvictions tests consecutive writes trigger multiple evictions.
func TestDiskCache_LRU_MultipleEvictions(t *testing.T) {
	const blockSize = 100
	const maxSize = blockSize * 5
	dc := newTestCache(t, maxSize, time.Hour)

	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key_%d", i)
		require.NoError(t, dc.Set(key, bytes.Repeat([]byte("x"), blockSize)))
	}

	time.Sleep(100 * time.Millisecond)
	assert.LessOrEqual(t, dc.Size(), int64(maxSize+shardCount*blockSize))
}

// TestDiskCache_LoadIndex_Recovery tests index recovery after restart.
func TestDiskCache_LoadIndex_Recovery(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	require.NoError(t, dc1.Set("key1", []byte("hello")))
	require.NoError(t, dc1.Set("key2", []byte("world")))
	dc1.Close()

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

// TestDiskCache_LoadIndex_SkipExpired tests expired files are skipped on restart.
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

	time.Sleep(100 * time.Millisecond)

	dc2, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc2.Close()

	assert.Equal(t, 0, dc2.Len())
	assert.Equal(t, int64(0), dc2.Size())
}

// TestDiskCache_LoadIndex_SizeConsistency tests Size consistency after restart.
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

// TestDiskCache_Concurrent_SetGet tests concurrent reads/writes for data races.
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
				key := fmt.Sprintf("key_%d_%d", id, j%10)
				_ = dc.Set(key, []byte(fmt.Sprintf("data_%d", j)))
				_, _ = dc.Get(key)
			}
		}(i)
	}
	wg.Wait()
}

// TestDiskCache_Concurrent_SetDelete tests concurrent Set and Delete.
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

// TestDiskCache_Concurrent_Eviction tests concurrent writes with LRU eviction.
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
	assert.LessOrEqual(t, dc.Size(), int64(maxSize+shardCount*blockSize))
}

// TestDiskCache_Concurrent_CleanupAndSet tests concurrent cleanup and Set.
func TestDiskCache_Concurrent_CleanupAndSet(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 50*time.Millisecond)

	var wg sync.WaitGroup
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

// TestDiskCache_LargeData tests writing data close to maxSize.
func TestDiskCache_LargeData(t *testing.T) {
	const maxSize = 5 * 1024 * 1024 // 5MB
	dc := newTestCache(t, maxSize, time.Hour)
	data := bytes.Repeat([]byte("x"), maxSize-1)
	require.NoError(t, dc.Set("large", data))

	got, err := dc.Get("large")
	assert.NoError(t, err)
	assert.Equal(t, data, got)
}

// TestDiskCache_MultipleShards tests keys distribute across all shards.
func TestDiskCache_MultipleShards(t *testing.T) {
	dc := newTestCache(t, 100*1024*1024, time.Hour)
	const count = 160
	for i := 0; i < count; i++ {
		key := BuildCacheKey(fmt.Sprintf("http://example.com/%d.pdf", i), uint64(i))
		require.NoError(t, dc.Set(key, []byte(fmt.Sprintf("block_%d", i))))
	}
	assert.Equal(t, count, dc.Len())
}

// TestBuildCacheKey_Uniqueness tests key uniqueness.
func TestBuildCacheKey_Uniqueness(t *testing.T) {
	k1 := BuildCacheKey("http://example.com/a.pdf", 0)
	k2 := BuildCacheKey("http://example.com/a.pdf", 1)
	k3 := BuildCacheKey("http://example.com/b.pdf", 0)
	assert.NotEqual(t, k1, k2, "same URL with different blockIndex should generate different keys")
	assert.NotEqual(t, k1, k3, "different URL with same blockIndex should generate different keys")
	assert.NotEqual(t, k2, k3)
}

// TestBuildCacheKey_Deterministic tests key determinism.
func TestBuildCacheKey_Deterministic(t *testing.T) {
	k1 := BuildCacheKey("http://example.com/a.pdf", 42)
	k2 := BuildCacheKey("http://example.com/a.pdf", 42)
	assert.Equal(t, k1, k2)
}

// TestGetShard_EmptyKey tests empty key maps to shards[0].
func TestGetShard_EmptyKey(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	shard := dc.getShard("")
	assert.Equal(t, dc.shards[0], shard)
}

// TestGetShard_Distribution tests FNV-1a shard distribution uniformity.
func TestGetShard_Distribution(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	counts := make(map[*cacheShard]int)
	const total = 1600
	for i := 0; i < total; i++ {
		key := BuildCacheKey(fmt.Sprintf("http://example.com/%d.pdf", i), uint64(i))
		shard := dc.getShard(key)
		counts[shard]++
	}
	for _, cnt := range counts {
		assert.Greater(t, cnt, 40, "shard hit too few")
		assert.Less(t, cnt, 160, "shard hit too many")
	}
	assert.Equal(t, shardCount, len(counts), "should cover all shards")
}

// TestDiskCache_Get_FileReadError tests Get when the disk file is externally deleted.
func TestDiskCache_Get_FileReadError(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("data")))

	filePath := dc.getFilePath("key1")
	require.NoError(t, os.Remove(filePath))

	_, err := dc.Get("key1")
	assert.Error(t, err)

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_Get_ConcurrentDelete_StillExists tests concurrent Get and Delete safety.
func TestDiskCache_Get_ConcurrentDelete_StillExists(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	require.NoError(t, dc.Set("key1", []byte("data")))

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
}

// TestDiskCache_newDiskCache_MkdirAllFail tests error on unwritable directory.
func TestDiskCache_newDiskCache_MkdirAllFail(t *testing.T) {
	cfg := &Config{
		DiskCacheDir:             "/proc/no_permission_dir_xyz",
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}
	_, err := NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_evictWorker_DrainOnStop tests evictWorker drains queue on stop.
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

	tmpDir := t.TempDir()
	for i := 0; i < 5; i++ {
		p := fmt.Sprintf("%s/file_%d", tmpDir, i)
		require.NoError(t, os.WriteFile(p, []byte("x"), 0644))
		dc.evictChan <- p
	}

	dc.Close()

	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 5; i++ {
		p := fmt.Sprintf("%s/file_%d", tmpDir, i)
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "evictWorker should have deleted file %s", p)
	}
}

// TestDiskCache_walkCacheDir_SkipSubDir tests subdirectories are skipped.
func TestDiskCache_walkCacheDir_SkipSubDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.MkdirAll(subDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "should_skip"), []byte("skip"), 0644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "normal_key"), []byte("data"), 0644))

	dc, err := NewDiskCache(cfg)
	require.NoError(t, err)
	defer dc.Close()

	assert.Equal(t, 1, dc.Len())
	_, err = dc.Get("normal_key")
	assert.NoError(t, err)
}

// TestDiskCache_cleanup_EvictChanFull tests cleanup fallback when evictChan is full.
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

	for i := 0; i < 10; i++ {
		require.NoError(t, dc.Set(fmt.Sprintf("key%d", i), []byte("data")))
	}

	for len(dc.evictChan) < cap(dc.evictChan) {
		dc.evictChan <- filepath.Join(dir, "nonexistent_fill")
	}

	time.Sleep(100 * time.Millisecond)
	dc.cleanup() // Trigger cleanup; evictChan full → synchronous deletion

	// Wait for cleanup to complete
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, dc.Len())
}

// TestDiskCache_evictOne_EvictChanFull tests evictOne fallback when evictChan is full.
func TestDiskCache_evictOne_EvictChanFull(t *testing.T) {
	const blockSize = 10
	dc := newTestCache(t, blockSize*2, time.Hour)

	for len(dc.evictChan) < cap(dc.evictChan) {
		dc.evictChan <- filepath.Join(dc.baseDir, "nonexistent_fill")
	}

	require.NoError(t, dc.Set("k1", bytes.Repeat([]byte("a"), blockSize)))
	require.NoError(t, dc.Set("k2", bytes.Repeat([]byte("b"), blockSize)))
	require.NoError(t, dc.Set("k3", bytes.Repeat([]byte("c"), blockSize))) // Triggers eviction

	time.Sleep(50 * time.Millisecond)
	assert.LessOrEqual(t, dc.Size(), int64(blockSize*2))
}

// TestDiskCache_queueExpiredForDeletion_EvictChanFull tests expired deletion fallback.
func TestDiskCache_queueExpiredForDeletion_EvictChanFull(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             50 * time.Millisecond,
		DiskCacheCleanupInterval: time.Hour,
	}

	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		require.NoError(t, dc1.Set(fmt.Sprintf("key%d", i), []byte("data")))
	}
	dc1.Close()

	time.Sleep(100 * time.Millisecond)

	dc2 := &DiskCache{
		baseDir:         dir,
		maxSize:         10 * 1024 * 1024,
		ttl:             50 * time.Millisecond,
		cleanupInterval: time.Hour,
		evictChan:       make(chan string, 1), // Capacity of 1 for easy filling
		stopChan:        make(chan struct{}),
	}
	for i := 0; i < shardCount; i++ {
		dc2.shards[i] = &cacheShard{
			index:   make(map[string]*list.Element),
			lruList: list.New(),
		}
	}
	defer dc2.Close()

	dc2.evictChan <- "fill"

	_, expiredPaths, err := dc2.walkCacheDir(time.Now())
	require.NoError(t, err)
	assert.NotEmpty(t, expiredPaths)

	dc2.queueExpiredForDeletion(expiredPaths)

	time.Sleep(200 * time.Millisecond)
	for _, p := range expiredPaths {
		_, statErr := os.Stat(p)
		assert.True(t, os.IsNotExist(statErr), "expired file should have been deleted: %s", p)
	}
}

// TestDiskCache_loadIndex_WalkError tests loadIndex error on unreadable directory.
func TestDiskCache_loadIndex_WalkError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping permission test when running as root")
	}

	parent := t.TempDir()
	dir := filepath.Join(parent, "cache")

	cfg := &Config{
		DiskCacheDir:             dir,
		DiskCacheMaxSize:         10 * 1024 * 1024,
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: time.Hour,
	}

	dc1, err := NewDiskCache(cfg)
	require.NoError(t, err)
	require.NoError(t, dc1.Set("key1", []byte("data")))
	dc1.Close()

	// 将目录权限设为不可读不可执行，filepath.Walk 无法列出目录内容
	require.NoError(t, os.Chmod(dir, 0000))
	defer func() { _ = os.Chmod(dir, 0755) }()

	_, err = NewDiskCache(cfg)
	assert.Error(t, err)
}

// TestDiskCache_Config_EmptyDir tests empty dir rejection.
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

// TestDiskCache_Config_ZeroMaxSize tests zero maxSize rejection.
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

// TestDiskCache_Config_NegativeTTL tests negative TTL rejection.
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

// TestDiskCache_Config_ZeroCleanupInterval tests zero cleanup interval rejection.
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

// TestDiskCache_Close_WaitsForGoroutines tests Close blocks until goroutines exit.
func TestDiskCache_Close_WaitsForGoroutines(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	for i := 0; i < 10; i++ {
		p := filepath.Join(dc.baseDir, fmt.Sprintf("close_test_%d", i))
		_ = os.WriteFile(p, []byte("x"), 0644)
		dc.evictChan <- p
	}
	dc.Close()
	for i := 0; i < 10; i++ {
		p := filepath.Join(dc.baseDir, fmt.Sprintf("close_test_%d", i))
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "files should be deleted after Close returns: %s", p)
	}
}

// TestDiskCache_Cleanup_AfterMoveToBack tests cleanup after MoveToBack disrupts order.
func TestDiskCache_Cleanup_AfterMoveToBack(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 200*time.Millisecond)
	require.NoError(t, dc.Set("k1", []byte("data1")))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, dc.Set("k2", []byte("data2")))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, dc.Set("k3", []byte("data3")))

	_, err := dc.Get("k1")
	require.NoError(t, err)

	time.Sleep(250 * time.Millisecond)

	dc.cleanup()
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 0, dc.Len(), "cleanup should remove all expired nodes")
	assert.Equal(t, int64(0), dc.Size())
}

// TestDiskCache_Get_ConcurrentSetOverwrite tests concurrent Get and Set-overwrite
// do not corrupt the linked list.
func TestDiskCache_Get_ConcurrentSetOverwrite(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, time.Hour)
	const key = "shared_key"
	require.NoError(t, dc.Set(key, []byte("initial")))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = dc.Get(key)
			}
		}()
	}
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

	shard := dc.getShard(key)
	shard.mu.RLock()
	listLen := shard.lruList.Len()
	indexLen := len(shard.index)
	shard.mu.RUnlock()
	assert.Equal(t, 1, dc.Len(), "after overwrites there should be only 1 entry")
	assert.Equal(t, listLen, indexLen, "list length should match index size (list not corrupted)")
}

// TestDiskCache_Cleanup_ConcurrentSetOverwrite tests concurrent cleanup and Set-overwrite safety.
func TestDiskCache_Cleanup_ConcurrentSetOverwrite(t *testing.T) {
	dc := newTestCache(t, 10*1024*1024, 80*time.Millisecond)
	const count = 20

	for i := 0; i < count; i++ {
		require.NoError(t, dc.Set(fmt.Sprintf("key_%d", i), []byte("old_data")))
	}

	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			dc.cleanup()
			time.Sleep(10 * time.Millisecond)
		}
	}()
	for i := 0; i < count/2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", id)
			_ = dc.Set(key, []byte("new_data"))
		}(i)
	}
	wg.Wait()

	for i, shard := range dc.shards {
		shard.mu.RLock()
		listLen := shard.lruList.Len()
		indexLen := len(shard.index)
		shard.mu.RUnlock()
		assert.Equal(t, listLen, indexLen, "shard %d list length should match index size", i)
	}
}
