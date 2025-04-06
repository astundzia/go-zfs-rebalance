#!/bin/bash
set -e

# This script runs the Docker buildx build process for all platforms
# It's a wrapper around the 'make buildx-build' command for use in CI/CD

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
PROJECT_ROOT="$( cd "$SCRIPT_DIR/.." >/dev/null 2>&1 && pwd )"

echo "===== STARTING PARALLEL BUILD PROCESS ====="
echo "Building for platforms: $(grep 'BUILDX_PLATFORMS=' "$PROJECT_ROOT/Makefile" | cut -d'=' -f2)"
echo

# Change to project root
cd "$PROJECT_ROOT"

# Run the buildx-build target from Makefile
echo "Running make buildx-build..."
make buildx-build

exit_code=$?
if [ $exit_code -ne 0 ]; then
    echo "ERROR: Build process failed with exit code $exit_code"
    exit $exit_code
fi

echo "===== PARALLEL BUILD PROCESS COMPLETED SUCCESSFULLY ====="
exit 0 