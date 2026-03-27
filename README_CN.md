# DiskCache

[![CI](https://github.com/juliewangs/diskcache/actions/workflows/ci.yml/badge.svg)](https://github.com/juliewangs/diskcache/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/juliewangs/diskcache)](https://goreportcard.com/report/github.com/juliewangs/diskcache)
[![GoDoc](https://pkg.go.dev/badge/github.com/juliewangs/diskcache)](https://pkg.go.dev/github.com/juliewangs/diskcache)
[![Go Version](https://img.shields.io/github/go-mod/go-version/juliewangs/diskcache)](go.mod)
[![License](https://img.shields.io/badge/license-BSD--2--Clause-blue.svg)](LICENSE)

[English](README.md)

高性能 Go 磁盘缓存库，支持 LRU 淘汰和 TTL 过期。

## 为什么选择 DiskCache？

大多数 Go 缓存库（groupcache、bigcache、freecache）都是**纯内存缓存** — 重启后数据丢失，总容量受限于可用内存。

DiskCache 填补了一个不同的空白：**持久化、基于磁盘的缓存**，同时拥有内存缓存般的易用性。

| | DiskCache | bigcache | freecache | groupcache |
|---|---|---|---|---|
| **存储介质** | 磁盘 | 内存 | 内存 | 内存 |
| **重启后保留** | ✅ | ❌ | ❌ | ❌ |
| **容量上限** | 取决于磁盘 | 取决于内存 | 取决于内存 | 取决于内存 |
| **并发模型** | 分片读写锁 | 分片互斥锁 | 分片互斥锁 | 互斥锁 |
| **淘汰策略** | LRU + TTL | FIFO + TTL | LRU + TTL | LRU |
| **适用场景** | 大文件、热重启 | 热数据、低延迟 | 热数据、零 GC | 分布式缓存 |

**适合选择 DiskCache 的场景：**
- 缓存数据需要**在进程重启后保留**，且不想引入外部依赖（Redis、Memcached）
- 需要缓存**大体积对象**（图片、ML 模型、编译模板），内存放不下
- 需要一个**简单、嵌入式**的方案，零基础设施开销

## 特性

- **分片锁** — 16 个分片（FNV-1a 哈希）降低写竞争
- **O(1) LRU 淘汰** — 每个分片独立的双向链表 + 哈希表
- **TTL 过期** — 后台定期清理 + 访问时惰性删除
- **异步磁盘 I/O** — 淘汰文件通过缓冲通道异步删除，不持有分片锁
- **重启恢复** — 启动时从磁盘重建内存索引
- **零外部依赖** — 仅使用 Go 标准库（测试依赖除外）

## 架构

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
│  │  定期 TTL       │  │  异步文件删除            │  │
│  │  扫描 & 过期    │  │  通过缓冲通道            │  │
│  └─────────────────┘  └──────────────────────────┘  │
└─────────────────────────────────────────────────────┘
						 │
					┌────┴────┐
					│  磁盘   │
					│（扁平   │
					│ 文件）  │
					└─────────┘
```

核心设计决策：
- **锁外磁盘 I/O** — `Set` 在获取分片锁之前写入磁盘；淘汰在释放锁之后删除文件。锁持有时间仅包含纯内存操作。
- **两阶段清理** — 读锁下收集过期键，写锁下移除。扫描期间不阻塞并发读取。
- **异步淘汰** — 淘汰文件路径发送到缓冲通道（容量 512）。通道满时回退为同步删除。

> 更多设计细节请参阅 [docs/design/ARCHITECTURE.md](docs/design/ARCHITECTURE.md)。

## 安装

```bash
go get github.com/juliewangs/diskcache
```

要求 **Go 1.21+**，无 CGO，无外部依赖。

## 快速开始

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/juliewangs/diskcache"
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

| 方法 | 说明 |
|------|------|
| `New(cfg) (*DiskCache, error)` | 创建缓存实例 |
| `Set(key, data) error` | 写入缓存条目 |
| `Get(key) ([]byte, error)` | 读取缓存条目（未命中返回 `os.ErrNotExist`） |
| `Delete(key) error` | 删除缓存条目 |
| `Size() int64` | 返回缓存总大小（字节） |
| `Len() int` | 返回缓存条目数量 |
| `Close()` | 停止后台协程并排空淘汰队列 |

## 配置项

| 字段 | 类型 | 说明 |
|------|------|------|
| `DiskCacheDir` | `string` | 缓存目录路径 |
| `DiskCacheMaxSize` | `int64` | 最大缓存大小（字节） |
| `DiskCacheTTL` | `time.Duration` | 条目存活时间 |
| `DiskCacheCleanupInterval` | `time.Duration` | 后台清理间隔 |

## 基准测试

测试环境：Linux（Intel Xeon Platinum 8255C @ 2.50GHz，16 核），1 KB 数据：

```
goos: linux
goarch: amd64
cpu: Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz

BenchmarkDiskCache_Set-16    59736    20052 ns/op    526 B/op    8 allocs/op
BenchmarkDiskCache_Get-16   121865     9627 ns/op   1541 B/op    6 allocs/op
```

| 操作 | 吞吐量 | 延迟 | 内存分配 |
|------|--------|------|---------|
| **Set**（1 KB） | ~50,000 ops/s | ~20 µs | 8 allocs/op |
| **Get**（1 KB） | ~104,000 ops/s | ~9.6 µs | 6 allocs/op |

> **注意：** 延迟主要由磁盘 I/O 决定。在 NVMe SSD 上性能会显著更好。纯内存索引操作耗时 < 200 ns。

自行运行基准测试：

```bash
cd test/benchmarks && go test -bench=. -benchmem
```

## 使用场景

| 场景 | 示例 |
|------|------|
| **Web 静态资源** | 缓存缩放后的图片、编译后的 CSS/JS |
| **ML 模型服务** | 本地缓存下载的模型权重 |
| **数据管道** | 缓存中间处理结果 |
| **API 响应缓存** | 跨重启持久化上游 API 响应 |
| **文件下载代理** | 远程文件的本地磁盘缓存 |

## 项目结构

```
diskcache/
├── diskcache.go                    # 公共门面（New / Config）
├── pkg/disk_cache/
│   ├── disk_cache.go               # 核心实现
│   └── disk_cache_test.go          # 单元测试
├── test/benchmarks/
│   └── disk_cache_bench_test.go    # 基准测试
├── examples/
│   └── basic_usage.go              # 使用示例
└── docs/design/
	└── ARCHITECTURE.md             # 设计文档
```

## 贡献

欢迎贡献！请遵循以下步骤：

1. Fork 本仓库
2. 创建功能分支（`git checkout -b feature/amazing-feature`）
3. 提交前运行质量检查：
   ```bash
   make check   # 运行 gofmt + go vet + 竞态测试
   ```
4. 提交清晰的 commit 信息（`git commit -m 'feat: add amazing feature'`）
5. Push 并创建 Pull Request

## 许可证

BSD 2-Clause License — 详见 [LICENSE](LICENSE)。
