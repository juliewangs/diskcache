package main

import (
	"fmt"
	"log"
	"time"

	"github.com/juliewangs/diskcache"
)

func main() {
	cfg := &diskcache.Config{
		DiskCacheDir:             "./example_cache",
		DiskCacheMaxSize:         10 * 1024 * 1024, // 10MB
		DiskCacheTTL:             time.Hour,
		DiskCacheCleanupInterval: 5 * time.Minute,
	}

	// Create cache instance
	dc, err := diskcache.New(cfg)
	if err != nil {
		log.Fatal("create disk cache: ", err)
	}
	defer dc.Close()

	fmt.Println("DiskCache example started...")

	// Example 1: Basic Set and Get
	fmt.Println("\n1. Basic Set and Get:")
	if err := dc.Set("greeting", []byte("Hello, World!")); err != nil {
		log.Fatal("set: ", err)
	}

	data, err := dc.Get("greeting")
	if err != nil {
		log.Fatal("get: ", err)
	}
	fmt.Printf("Got: %s\n", string(data))

	// Example 2: Overwrite
	fmt.Println("\n2. Overwrite:")
	if err := dc.Set("greeting", []byte("Hello, DiskCache!")); err != nil {
		log.Fatal("overwrite: ", err)
	}

	data, err = dc.Get("greeting")
	if err != nil {
		log.Fatal("get after overwrite: ", err)
	}
	fmt.Printf("After overwrite: %s\n", string(data))

	// Example 3: Delete
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
