package main

import (
	"fmt"
	"log"
	"time"

	"github.com/juliehwang/diskcache"
)

func main() {
	cfg := &diskcache.Config{
		DiskCacheDir:             "./example_cache",
		DiskCacheMaxSize:         10 * 1024 * 1024, // 10MB
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: 5 * time.Minute,
	}

	// 创建缓存实例
	dc, err := diskcache.New(cfg)
	if err != nil {
		log.Fatal("create disk cache: ", err)
	}
	defer dc.Close()

	fmt.Println("DiskCache example started...")

	// 示例1: 基本设置和获取
	fmt.Println("\n1. Basic Set and Get:")
	if err := dc.Set("greeting", []byte("Hello, World!")); err != nil {
		log.Fatal("set: ", err)
	}

	data, err := dc.Get("greeting")
	if err != nil {
		log.Fatal("get: ", err)
	}
	fmt.Printf("Got: %s\n", string(data))

	// 示例2: 覆盖写入
	fmt.Println("\n2. Overwrite:")
	if err := dc.Set("greeting", []byte("Hello, DiskCache!")); err != nil {
		log.Fatal("overwrite: ", err)
	}

	data, err = dc.Get("greeting")
	if err != nil {
		log.Fatal("get after overwrite: ", err)
	}
	fmt.Printf("After overwrite: %s\n", string(data))

	// 示例3: 删除
	fmt.Println("\n3. Delete:")
	if err := dc.Delete("greeting"); err != nil {
		log.Fatal("delete: ", err)
	}

	_, err = dc.Get("greeting")
	if err != nil {
		fmt.Println("Key deleted successfully (expected error):", err)
	}

	fmt.Println("\nExample completed successfully!")
}
