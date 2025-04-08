.PHONY: all clean test unit-test integration-test build build-all build-debug build-all-debug package install install-debug run run-debug lint copy-to-dist

BINARY_NAME=rebalance
MAIN_PKG=./cmd/rebalance
DOCKER_TEST_IMAGE=rebalance-test:latest
BUILDX_PLATFORMS=linux/amd64,linux/arm64,windows/amd64

all: clean build-all test package copy-to-dist

clean:
	rm -rf bin/ dist/

test:
	@echo "===== RUNNING TESTS ====="
	go test -v ./internal/... ./pkg/... ./tests

unit-test:
	@echo "===== RUNNING LOCAL UNIT TESTS ====="
	go test -v ./internal/... ./pkg/... ./tests

integration-test:
	go test -v ./tests/integration/...

buildx-test-image:
	docker build -t $(DOCKER_TEST_IMAGE) -f Dockerfile.test .

buildx-test: buildx-test-image
	@echo "===== RUNNING TESTS VIA DOCKER ====="
	@/bin/bash -c '\
		platforms="$(BUILDX_PLATFORMS)"; \
		IFS="," read -ra ADDR <<< "$$platforms"; \
		for plat in "$${ADDR[@]}"; do \
			echo "--- Testing $$plat ---"; \
			docker run --rm -e TARGET_PLATFORM="$$plat" $(DOCKER_TEST_IMAGE) || exit 1; \
		done \
	'
	@echo "===== ALL DOCKER TESTS COMPLETED ====="

lint:
	@echo "===== RUNNING GOLANGCI-LINT ======"
	golangci-lint run ./...
	@echo "===== LINTING COMPLETE ======"

build:
	mkdir -p bin/$(shell go env GOOS)_$(shell go env GOARCH)
	CGO_ENABLED=1 go build -o bin/$(shell go env GOOS)_$(shell go env GOARCH)/$(BINARY_NAME)-$(shell go env GOOS)-$(shell go env GOARCH) $(MAIN_PKG)
	ln -sf $(BINARY_NAME)-$(shell go env GOOS)-$(shell go env GOARCH) bin/$(shell go env GOOS)_$(shell go env GOARCH)/$(BINARY_NAME)

build-debug:
	@echo "===== BUILDING DEBUG VERSION WITH RACE DETECTOR ====="
	mkdir -p bin/$(shell go env GOOS)_$(shell go env GOARCH)
	CGO_ENABLED=1 go build -race -o bin/$(shell go env GOOS)_$(shell go env GOARCH)/$(BINARY_NAME)-$(shell go env GOOS)-$(shell go env GOARCH)-debug $(MAIN_PKG)
	ln -sf $(BINARY_NAME)-$(shell go env GOOS)-$(shell go env GOARCH)-debug bin/$(shell go env GOOS)_$(shell go env GOARCH)/$(BINARY_NAME)-debug
	@echo "Debug binary with race detector available at bin/$(shell go env GOOS)_$(shell go env GOARCH)/$(BINARY_NAME)-debug"

build-all:
	@echo "===== BUILDING SEQUENTIALLY FOR ALL PLATFORMS ====="
	@./scripts/build-and-test.sh
	@echo "===== BUILD COMPLETED ====="
	@echo "Binaries output to bin/ directory"

build-all-debug:
	@echo "===== BUILDING DEBUG VERSIONS WITH RACE DETECTOR FOR ALL PLATFORMS ====="
	@DEBUG=1 ./scripts/build-and-test.sh
	@echo "===== DEBUG BUILD COMPLETED ====="
	@echo "Debug binaries with race detector output to bin/ directory"

copy-to-dist: build-all
	@echo "===== COPYING PLATFORM BINARIES TO DIST ====="
	scripts/copy_to_dist.sh
	@echo "===== PLATFORM BINARIES COPIED ====="
	@echo "Individual binaries available in dist/ directory"

package: build-all
	scripts/package.sh

install:
	@OS=$$(go env GOOS); ARCH=$$(go env GOARCH); \
	 if [ -f "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}" ]; then \
		 cp "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}" /usr/local/bin/$(BINARY_NAME); \
		 echo "Installed bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH} to /usr/local/bin/$(BINARY_NAME)"; \
	 else \
		 echo "Binary for $${OS}/$${ARCH} not found in bin/. Run 'make build' or 'make build-all' first."; \
		 exit 1; \
	 fi

install-debug:
	@OS=$$(go env GOOS); ARCH=$$(go env GOARCH); \
	 if [ -f "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}-debug" ]; then \
		 cp "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}-debug" /usr/local/bin/$(BINARY_NAME)-debug; \
		 echo "Installed bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}-debug to /usr/local/bin/$(BINARY_NAME)-debug"; \
	 else \
		 echo "Debug binary for $${OS}/$${ARCH} not found in bin/. Run 'make build-debug' first."; \
		 exit 1; \
	 fi

run:
	@OS=$$(go env GOOS); ARCH=$$(go env GOARCH); \
	 if [ -f "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}" ]; then \
		 "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}" $${ARGS}; \
	 else \
		 echo "Binary for $${OS}/$${ARCH} not found in bin/. Run 'make build' or 'make build-all' first."; \
		 exit 1; \
	 fi

run-debug:
	@OS=$$(go env GOOS); ARCH=$$(go env GOARCH); \
	 if [ -f "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}-debug" ]; then \
		 "bin/$${OS}_$${ARCH}/$(BINARY_NAME)-$${OS}-$${ARCH}-debug" $${ARGS}; \
	 else \
		 echo "Debug binary for $${OS}/$${ARCH} not found in bin/. Run 'make build-debug' first."; \
		 exit 1; \
	 fi
