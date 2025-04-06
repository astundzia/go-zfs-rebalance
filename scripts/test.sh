#!/bin/bash
set -e

# Clear any cached Go test results
go clean -testcache

# Setup environment based on target platform
export CGO_ENABLED=1

if [ "$TARGET_PLATFORM" = "linux/amd64" ]; then
    export GOOS=linux
    export GOARCH=amd64
    export CC=gcc
    echo "--- GCC Version ---"
    gcc --version || echo "gcc command failed"
    echo "--- Default Go CGO Flags ---"
    go env CGO_CFLAGS CGO_LDFLAGS || echo "go env failed"
    # echo "--- Unsetting CGO Flags ---" # Keep default CGO flags
elif [ "$TARGET_PLATFORM" = "linux/arm64" ]; then
    export GOOS=linux
    export GOARCH=arm64
    # Use standard ARM64 cross-compiler
    export CC=aarch64-linux-gnu-gcc
    echo "Using CC=aarch64-linux-gnu-gcc for Linux ARM64"
    # Restore PKG_CONFIG_PATH for Debian-based image
    export PKG_CONFIG_PATH=$PKG_CONFIG_PATH_ORIGINAL # Restore if set
elif [ "$TARGET_PLATFORM" = "windows/amd64" ]; then
    export GOOS=windows
    export GOARCH=amd64
    # Use standard Windows cross-compiler
    export CC=x86_64-w64-mingw32-gcc
fi

echo "===== TEST ENVIRONMENT ====="
echo "Platform: $TARGET_PLATFORM"
echo "GOOS: $GOOS"
echo "GOARCH: $GOARCH"
echo "CC: $CC"
echo "CGO_ENABLED: $CGO_ENABLED"

# Optimize for parallel builds
export GOMAXPROCS=$(nproc)

echo "===== DIRECTORY STRUCTURE ====="
find . -type d | sort
echo "===== GO.MOD ====="
cat go.mod
echo "===== TEST FILES ====="
find . -name "*_test.go"

# Set output path with .exe extension for Windows
OUTPUT_PATH="/output/rebalance-${GOOS}-${GOARCH}"
if [ "$GOOS" = "windows" ]; then
    OUTPUT_PATH="${OUTPUT_PATH}.exe"
    echo "===== BUILDING FOR WINDOWS ======"
    go build -v -o "$OUTPUT_PATH" ./cmd/rebalance
    echo "BUILD COMPLETED"
else
    # For other platforms, run tests first
    echo "===== RUNNING TESTS ====="
    CC=$CC go test -v -count=1 ./internal/... ./pkg/... ./tests/... || {
        echo "Tests failed, attempting fallback with explicit CGO settings..."
        CGO_ENABLED=1 CC=$CC go test -v -count=1 ./internal/... ./pkg/... ./tests/...
    }
    echo "TESTS COMPLETED"

    # Only build if tests passed
    echo "===== BUILDING BINARY ====="
    go build -v -o "$OUTPUT_PATH" ./cmd/rebalance
    echo "BUILD COMPLETED"
fi

echo "===== ALL DONE! =====" 