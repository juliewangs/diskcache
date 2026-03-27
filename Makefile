.PHONY: test bench clean fmt vet lint race check example

# 格式化检查（CI 必须通过）
fmt:
	@echo "==> Checking gofmt..."
	@test -z "$$(gofmt -l .)" || (echo "gofmt check failed:" && gofmt -l . && exit 1)

# 静态分析（CI 必须通过）
vet:
	go vet ./...

# 单元测试
test:
	go test -count=1 ./pkg/disk_cache/

# 竞态检测测试
race:
	go test -race -count=1 ./...

# 基准测试
bench:
	go test -bench=. -benchmem ./test/benchmarks/

# 质量门禁（CI 推荐使用）
check: fmt vet race

# 清理
clean:
	go clean
	rm -rf example_cache test_cache

# 运行示例
example:
	go run examples/basic_usage.go