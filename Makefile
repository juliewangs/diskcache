.PHONY: test bench clean fmt vet lint race check example

# Format check (CI must pass)
fmt:
	@echo "==> Checking gofmt..."
	@test -z "$$(gofmt -l .)" || (echo "gofmt check failed:" && gofmt -l . && exit 1)

# Static analysis (CI must pass)
vet:
	go vet ./...

# Unit tests
test:
	go test -count=1 ./pkg/disk_cache/

# Race detection tests
race:
	go test -race -count=1 ./...

# Benchmarks
bench:
	go test -bench=. -benchmem ./test/benchmarks/

# Quality gate (recommended for CI)
check: fmt vet race

# Clean
clean:
	go clean
	rm -rf example_cache test_cache

# Run example
example:
	go run examples/basic_usage.go