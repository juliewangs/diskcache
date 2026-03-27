# DiskCache - 高性能磁盘缓存库

DiskCache 是一个高性能的 Go 语言磁盘缓存库，提供基于 LRU 淘汰策略的磁盘缓存解决方案。

## 特性

- 🚀 **高性能**: 采用分片锁设计，支持高并发读写
- 💾 **持久化**: 缓存数据持久化到磁盘，支持重启恢复
- ⏰ **TTL 支持**: 支持缓存过期时间设置
- 🔄 **LRU 淘汰**: 基于 LRU 算法自动淘汰旧数据
- 🛡️ **线程安全**: 完全线程安全，支持并发访问
- 📦 **轻量级**: 无外部依赖，纯 Go 实现

## 安装

```bash
go get github.com/juliehwang/diskcache
```

## 快速开始

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/juliehwang/diskcache"
)

func main() {
	// 创建磁盘缓存实例
	cfg := &diskcache.Config{
		DiskCacheDir:             "./cache",
		DiskCacheMaxSize:         100 * 1024 * 1024, // 100MB
		DiskCacheTTL:             24 * time.Hour,    // 24小时
		DiskCacheCleanupInterval: time.Hour,         // 清理间隔
	}

	dc, err := diskcache.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer dc.Close()

	// 写入缓存
	err = dc.Set("user:123", []byte("user data"))
	if err != nil {
		log.Fatal(err)
	}

	// 读取缓存
	data, err := dc.Get("user:123")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Cached data:", string(data))

	// 删除缓存
	err = dc.Delete("user:123")
	if err != nil {
		log.Fatal(err)
	}
}
```

## API 参考

### Config 配置结构

```go
type Config struct {
	DiskCacheDir             string        // 缓存目录路径
	DiskCacheMaxSize         int64         // 最大缓存大小（字节）
	DiskCacheTTL             time.Duration // 缓存过期时间
	DiskCacheCleanupInterval time.Duration // 清理间隔时间
}
```

### 主要方法

- `New(cfg *Config) (*DiskCache, error)` - 创建缓存实例（推荐）
- `NewDiskCache(cfg *Config) (*DiskCache, error)` - 创建缓存实例（兼容）
- `Set(key string, data []byte) error` - 设置缓存
- `Get(key string) ([]byte, error)` - 获取缓存
- `Delete(key string) error` - 删除缓存
- `Close()` - 关闭缓存实例（停止后台清理）

## 性能测试

运行基准测试：

```bash
cd test/benchmarks
go test -bench=.
```

## 许可证

BSD 2-Clause License - 详见 [LICENSE](LICENSE) 文件

## 贡献

欢迎提交 Issue 和 Pull Request！

## 支持

如有问题，请通过 GitHub Issues 联系我们。