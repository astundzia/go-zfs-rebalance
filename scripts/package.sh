#!/bin/bash
set -e

# Create a self-extracting package with multiple architecture binaries
# This script creates a "fat" binary package that contains all architectures
# and extracts the appropriate one based on the detected system.

# Define version
VERSION=$(git describe --tags --always 2>/dev/null || echo "dev")
PACKAGE_DIR="dist"
PACKAGE_NAME="rebalance-multiarch"
EXTRACT_DIR="/tmp/rebalance-extract-$$"

# Ensure we have a build directory
mkdir -p $PACKAGE_DIR

# Create a temporary directory for packaging
mkdir -p $EXTRACT_DIR

# Copy binaries with correct arch names regardless of source filename
echo "Copying binaries to $EXTRACT_DIR from bin/..."

# Handle Linux AMD64
if [ -d "bin/linux_amd64" ]; then
    if [ -f "bin/linux_amd64/rebalance-linux-amd64" ]; then
        echo "  Copying bin/linux_amd64/rebalance-linux-amd64 -> $EXTRACT_DIR/rebalance-linux-amd64"
        cp "bin/linux_amd64/rebalance-linux-amd64" "$EXTRACT_DIR/rebalance-linux-amd64"
        chmod +x "$EXTRACT_DIR/rebalance-linux-amd64"
    else
        echo "  Warning: rebalance-linux-amd64 not found in bin/linux_amd64"
    fi
fi

# Handle Linux ARM64
if [ -d "bin/linux_arm64" ]; then
    if [ -f "bin/linux_arm64/rebalance-linux-arm64" ]; then
        echo "  Copying bin/linux_arm64/rebalance-linux-arm64 -> $EXTRACT_DIR/rebalance-linux-arm64"
        cp "bin/linux_arm64/rebalance-linux-arm64" "$EXTRACT_DIR/rebalance-linux-arm64"
        chmod +x "$EXTRACT_DIR/rebalance-linux-arm64"
    else
        echo "  Warning: rebalance-linux-arm64 not found in bin/linux_arm64"
    fi
fi

# Handle Windows
if [ -d "bin/windows_amd64" ]; then
    if [ -f "bin/windows_amd64/rebalance-windows-amd64.exe" ]; then
        echo "  Copying bin/windows_amd64/rebalance-windows-amd64.exe -> $EXTRACT_DIR/rebalance-windows-amd64.exe"
        cp "bin/windows_amd64/rebalance-windows-amd64.exe" "$EXTRACT_DIR/rebalance-windows-amd64.exe"
        chmod +x "$EXTRACT_DIR/rebalance-windows-amd64.exe"
    else
        echo "  Warning: rebalance-windows-amd64.exe not found in bin/windows_amd64"
    fi
fi

# Handle macOS AMD64
if [ -d "bin/darwin_amd64" ]; then
    if [ -f "bin/darwin_amd64/rebalance-darwin-amd64" ]; then
        echo "  Copying bin/darwin_amd64/rebalance-darwin-amd64 -> $EXTRACT_DIR/rebalance-darwin-amd64"
        cp "bin/darwin_amd64/rebalance-darwin-amd64" "$EXTRACT_DIR/rebalance-darwin-amd64"
        chmod +x "$EXTRACT_DIR/rebalance-darwin-amd64"
    else
        echo "  Warning: rebalance-darwin-amd64 not found in bin/darwin_amd64"
    fi
fi

# Handle macOS ARM64
if [ -d "bin/darwin_arm64" ]; then
    if [ -f "bin/darwin_arm64/rebalance-darwin-arm64" ]; then
        echo "  Copying bin/darwin_arm64/rebalance-darwin-arm64 -> $EXTRACT_DIR/rebalance-darwin-arm64"
        cp "bin/darwin_arm64/rebalance-darwin-arm64" "$EXTRACT_DIR/rebalance-darwin-arm64"
        chmod +x "$EXTRACT_DIR/rebalance-darwin-arm64"
    else
        echo "  Warning: rebalance-darwin-arm64 not found in bin/darwin_arm64"
    fi
fi

# Skip Windows as requested
echo "  Skipping Windows platform as requested"

# Check if any binaries were copied
if [ -z "$(ls -A $EXTRACT_DIR | grep 'rebalance-')" ]; then
    echo "Error: No rebalance binaries found in bin/ subdirectories. Check buildx output structure."
    rm -rf $EXTRACT_DIR
    exit 1
fi

# Create the wrapper script that will be included in the package
cat > $EXTRACT_DIR/run.sh << 'EOF'
#!/bin/bash
# This script detects the current architecture and runs the appropriate binary

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
    linux*) OS="linux" ;;
    darwin*) OS="darwin" ;;
    mingw*|msys*|cygwin*) OS="windows" ;;
    *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Set executable name based on OS
EXEC="rebalance-$OS-$ARCH"
if [ "$OS" = "windows" ]; then
    EXEC="${EXEC}.exe"
fi

# Extract directory is the same as this script's directory
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
BINARY_PATH="$SCRIPT_DIR/$EXEC"

# Make the binary executable
chmod +x "$BINARY_PATH" 2>/dev/null || true

# Run the binary
if [ -f "$BINARY_PATH" ]; then
    exec "$BINARY_PATH" "$@"
else
    echo "Error: Could not find binary for $OS-$ARCH"
    exit 1
fi
EOF

# Make the script executable
chmod +x $EXTRACT_DIR/run.sh

# Create a self-extracting archive
echo "Creating self-extracting package..."
cat > $PACKAGE_DIR/$PACKAGE_NAME << 'EOF'
#!/bin/bash
# Self-extracting installer for rebalance multi-architecture package

# Extract directory
EXTRACT_DIR="${HOME}/.rebalance"
mkdir -p "$EXTRACT_DIR"

# Extract the contents (everything after __ARCHIVE_BELOW__)
ARCHIVE=$(awk '/^__ARCHIVE_BELOW__/ {print NR + 1; exit 0; }' "$0")
# Use --no-xattrs to ignore macOS extended attributes on Linux
tail -n+$ARCHIVE "$0" | tar xz --no-xattrs -C "$EXTRACT_DIR"

# Create symlink in /usr/local/bin if possible
if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
    ln -sf "$EXTRACT_DIR/run.sh" "/usr/local/bin/rebalance"
    echo "Installed rebalance to /usr/local/bin/rebalance"
else
    echo "To install rebalance system-wide, run:"
    echo "sudo ln -sf \"$EXTRACT_DIR/run.sh\" /usr/local/bin/rebalance"
    echo ""
    echo "Or you can run it directly with:"
    echo "$EXTRACT_DIR/run.sh"
fi

exit 0
__ARCHIVE_BELOW__
EOF

# Append the tar archive to the self-extracting script
# Use --no-xattrs to exclude macOS extended attributes (prevents warnings on Linux)
tar czf - --no-xattrs -C $EXTRACT_DIR . >> $PACKAGE_DIR/$PACKAGE_NAME

# Make the self-extracting archive executable with specific permissions (755)
chmod 755 $PACKAGE_DIR/$PACKAGE_NAME

# Clean up
rm -rf $EXTRACT_DIR

echo "Created multiarch package: $PACKAGE_DIR/$PACKAGE_NAME"
echo "To install, run: ./$PACKAGE_DIR/$PACKAGE_NAME" 