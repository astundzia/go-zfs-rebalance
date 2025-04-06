#!/bin/bash
set -e

# Script to copy each built binary from bin/ to dist/ directory
# This maintains the platform organization for individual binary distribution
# and creates SHA256 checksums for each binary

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
PROJECT_ROOT="$( cd "$SCRIPT_DIR/.." >/dev/null 2>&1 && pwd )"
BIN_DIR="$PROJECT_ROOT/bin"
DIST_DIR="$PROJECT_ROOT/dist"

# Ensure the dist directory exists
mkdir -p "$DIST_DIR"

echo "Copying binaries from bin/ to dist/..."

# Find all platform directories in bin/
for PLATFORM_DIR in "$BIN_DIR"/*/ ; do
    if [ -d "$PLATFORM_DIR" ]; then
        PLATFORM=$(basename "$PLATFORM_DIR")
        echo "Processing platform: $PLATFORM"
        
        # Create corresponding platform directory in dist/
        mkdir -p "$DIST_DIR/$PLATFORM"
        
        # Copy all binaries from this platform
        # Use -type f without -executable for macOS compatibility
        find "$PLATFORM_DIR" -type f -name "rebalance*" | while read BIN_FILE; do
            BIN_NAME=$(basename "$BIN_FILE")
            echo "  Copying $BIN_NAME to dist/$PLATFORM/"
            cp "$BIN_FILE" "$DIST_DIR/$PLATFORM/$BIN_NAME"
            # Make executable in case it's not already
            chmod +x "$DIST_DIR/$PLATFORM/$BIN_NAME"
            
            # Generate SHA256 checksum
            echo "  Generating SHA256 checksum for $BIN_NAME"
            CHECKSUM_FILE="$DIST_DIR/$PLATFORM/$BIN_NAME.sha256"
            if command -v shasum >/dev/null 2>&1; then
                # macOS uses shasum
                (cd "$DIST_DIR/$PLATFORM" && shasum -a 256 "$BIN_NAME" > "$BIN_NAME.sha256")
            elif command -v sha256sum >/dev/null 2>&1; then
                # Linux uses sha256sum
                (cd "$DIST_DIR/$PLATFORM" && sha256sum "$BIN_NAME" > "$BIN_NAME.sha256")
            else
                echo "Warning: Neither shasum nor sha256sum found. Skipping checksum generation."
            fi
        done
    fi
done

# Also create a checksum for the multiarch package if it exists
if [ -f "$DIST_DIR/rebalance-multiarch" ]; then
    echo "Generating SHA256 checksum for rebalance-multiarch"
    if command -v shasum >/dev/null 2>&1; then
        # macOS uses shasum
        (cd "$DIST_DIR" && shasum -a 256 rebalance-multiarch > rebalance-multiarch.sha256)
    elif command -v sha256sum >/dev/null 2>&1; then
        # Linux uses sha256sum
        (cd "$DIST_DIR" && sha256sum rebalance-multiarch > rebalance-multiarch.sha256)
    else
        echo "Warning: Neither shasum nor sha256sum found. Skipping checksum generation."
    fi
fi

echo "Binary copying and checksum generation complete. All files available in dist/ directory." 