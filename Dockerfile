FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS builder

# Install build dependencies, cross-compilation tools, and utilities
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    gcc \
    g++ \
    gcc-x86-64-linux-gnu \
    libc6-dev-amd64-cross \
    gcc-aarch64-linux-gnu \
    libc6-dev-arm64-cross \
    gcc-mingw-w64-x86-64 \
    g++-mingw-w64-x86-64 \
    ca-certificates \
    libsqlite3-dev \
    pkg-config \
    vim \
    less \
    tree \
    && rm -rf /var/lib/apt/lists/*

# Install golangci-lint
ARG GOLANGCI_LINT_VERSION=v1.58.1 # Specify desired version
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin ${GOLANGCI_LINT_VERSION}
ENV PATH="/go/bin:${PATH}"

# TARGETPLATFORM, TARGETOS, TARGETARCH, TARGETVARIANT are set automatically by buildx.
# ARG TARGETPLATFORM
# ARG TARGETOS
# ARG TARGETARCH

WORKDIR /app

# Copy go modules and download dependencies first for caching
COPY go.mod go.sum* ./
RUN go mod download

# Copy the entire source code
COPY . .

# Configure Go for CGO
ENV CGO_ENABLED=1
# Ensure compatibility if needed
ENV GOAMD64=v1

# Build using buildx-provided arguments
# Set appropriate CC based on TARGETARCH and TARGETOS
RUN mkdir -p /app/binaries && \
    export GOOS=${TARGETOS:-linux} && \
    export GOARCH=${TARGETARCH:-amd64} && \
    # CGO_ENABLED is set via ENV now
    # export CGO_ENABLED=1 && \
    if [ "$GOOS" = "linux" ] && [ "$GOARCH" = "amd64" ]; then export CC=x86_64-linux-gnu-gcc; \
    elif [ "$GOOS" = "linux" ] && [ "$GOARCH" = "arm64" ]; then export CC=aarch64-linux-gnu-gcc; \
    elif [ "$GOOS" = "windows" ] && [ "$GOARCH" = "amd64" ]; then export CC=x86_64-w64-mingw32-gcc; \
    else echo "Unsupported platform: $GOOS/$GOARCH"; exit 1; fi && \
    echo "Building for $GOOS/$GOARCH with CC=$CC" && \
    OUTPUT_NAME="rebalance-${GOOS}-${GOARCH}" && \
    if [ "$GOOS" = "windows" ]; then OUTPUT_NAME="${OUTPUT_NAME}.exe"; fi && \
    OUTPUT_PATH="/app/binaries/${OUTPUT_NAME}" && \
    go build -v -o "${OUTPUT_PATH}" ./cmd/rebalance && \
    echo "Build completed: ${OUTPUT_PATH}"

# --- Final Stage --- 
FROM scratch AS final

# Copy only the binaries from the builder stage
COPY --from=builder /app/binaries /