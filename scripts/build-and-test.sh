#!/bin/bash
set -e

# Combined script to run both build and test processes in sequence
# Now running builds and tests sequentially rather than in parallel

TOTAL_TIMEOUT_MINUTES=30
START_TIME=$(date +%s)
PLATFORMS="linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64"

# Check if we should build with the race detector
RACE_FLAG=""
if [ "${DEBUG:-0}" = "1" ]; then
    echo "DEBUG=1 detected, building with -race flag enabled"
    RACE_FLAG="-race"
fi

# Function to check overall timeout
check_total_timeout() {
    local current_time=$(date +%s)
    local elapsed_time=$((current_time - START_TIME))
    local timeout_seconds=$((TOTAL_TIMEOUT_MINUTES * 60))
    
    if [ $elapsed_time -gt $timeout_seconds ]; then
        echo "ERROR: Total process timed out after ${TOTAL_TIMEOUT_MINUTES} minutes!"
        exit 1
    fi
}

echo "===== STARTING SEQUENTIAL BUILD AND TEST PROCESS ====="
echo "Total timeout: ${TOTAL_TIMEOUT_MINUTES} minutes"
echo

# Define Docker build function for all platforms
build_for_platform() {
    local platform=$1
    local os=$(echo $platform | cut -d/ -f1)
    local arch=$(echo $platform | cut -d/ -f2)
    
    echo "===== BUILDING FOR ${platform} ====="
    mkdir -p bin/${os}_${arch}
    
    # Common Docker parameters - we use linux/amd64 Docker container to cross-compile for all platforms
    local output_file="rebalance-${os}-${arch}"
    local docker_args="-v $(pwd):/app -w /app golang:1.23"
    local build_cmd="go build ${RACE_FLAG} -o bin/${os}_${arch}/${output_file}"

    # Add debug suffix if building with race detector
    if [ -n "${RACE_FLAG}" ]; then
        output_file="${output_file}-debug"
        build_cmd="go build ${RACE_FLAG} -o bin/${os}_${arch}/${output_file}"
    fi

    # Add .exe extension for Windows
    if [ "$os" = "windows" ]; then
        output_file="${output_file}.exe"
        build_cmd="go build ${RACE_FLAG} -o bin/${os}_${arch}/${output_file}"
    fi
    
    echo "Building ${platform} binary via Docker (using linux/amd64 build container)..."
    
    # Run Docker-based build
    if [ "$os" = "windows" ]; then
        # Windows build - use cross compiler
        docker run --platform linux/amd64 --rm ${docker_args} bash -c "apt-get update && apt-get install -y gcc-mingw-w64-x86-64 && CGO_ENABLED=1 GOOS=${os} GOARCH=${arch} CC=x86_64-w64-mingw32-gcc ${build_cmd} ./cmd/rebalance"
    elif [ "$os" = "linux" ] && [ "$arch" = "arm64" ]; then
        # Linux ARM64 build - use cross compiler
        docker run --platform linux/amd64 --rm ${docker_args} bash -c "apt-get update && apt-get install -y gcc-aarch64-linux-gnu && CGO_ENABLED=1 GOOS=${os} GOARCH=${arch} CC=aarch64-linux-gnu-gcc ${build_cmd} ./cmd/rebalance"
    elif [ "$os" = "darwin" ]; then
        # macOS builds - no CGO needed
        docker run --platform linux/amd64 --rm ${docker_args} bash -c "CGO_ENABLED=0 GOOS=${os} GOARCH=${arch} ${build_cmd} ./cmd/rebalance"
    else
        # Default Linux AMD64 build
        docker run --platform linux/amd64 --rm ${docker_args} bash -c "CGO_ENABLED=1 GOOS=${os} GOARCH=${arch} ${build_cmd} ./cmd/rebalance"
    fi
    
    # Create symlink for convenience
    if [ "$os" = "windows" ]; then
        if [ -n "${RACE_FLAG}" ]; then
            ln -sf "${output_file}" "bin/${os}_${arch}/rebalance-debug.exe"
        else
            ln -sf "${output_file}" "bin/${os}_${arch}/rebalance.exe"
        fi
    else
        if [ -n "${RACE_FLAG}" ]; then
            ln -sf "${output_file}" "bin/${os}_${arch}/rebalance-debug"
        else
            ln -sf "${output_file}" "bin/${os}_${arch}/rebalance"
        fi
    fi
    
    echo "Build completed for ${platform}"
}

# Define common test function
test_for_platform() {
    local platform=$1
    local os=$(echo $platform | cut -d/ -f1)
    local arch=$(echo $platform | cut -d/ -f2)
    
    echo "===== TESTING FOR ${platform} ====="
    
    # Skip tests for cross-compilation targets
    if [ "$os" = "windows" ] || [ "$os" = "darwin" ] || [ "$arch" = "arm64" ]; then
        echo "Skipping test execution for ${platform} platform (cross-compilation)"
        return
    fi
    
    # Only test native Linux/AMD64 since we're building in Docker
    if [ "$os" = "linux" ] && [ "$arch" = "amd64" ]; then
        echo "Running tests for ${platform}..."
        docker run --platform linux/amd64 --rm -v $(pwd):/app -w /app golang:1.23 bash -c "CGO_ENABLED=1 go test ./internal/... ./pkg/... ./tests/..."
    fi
    
    echo "Tests completed for ${platform}"
}

# Clean before starting
echo "Cleaning output directories..."
rm -rf bin/
mkdir -p bin/

# Run builds and tests sequentially for each platform
for platform in $PLATFORMS; do
    echo
    echo "===== PROCESSING PLATFORM: ${platform} ====="
    
    # Build for this platform
    build_for_platform $platform
    check_total_timeout
    
    # Test for this platform
    test_for_platform $platform
    check_total_timeout
    
    echo "===== COMPLETED PLATFORM: ${platform} ====="
    echo
done

echo "===== BUILD AND TEST PROCESS COMPLETED SUCCESSFULLY ====="
exit 0 