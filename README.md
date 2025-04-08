# go-zfs-rebalance

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Report Card](https://goreportcard.com/badge/github.com/astundzia/go-zfs-rebalance)](https://goreportcard.com/report/github.com/astundzia/go-zfs-rebalance)
[![Go Version](https://img.shields.io/github/go-mod/go-version/astundzia/go-zfs-rebalance)](https://github.com/astundzia/go-zfs-rebalance)

A high-performance utility for reducing ZFS fragmentation by copying files in-place.

## Overview

go-zfs-rebalance is designed to help mitigate ZFS fragmentation issues by intelligently rebalancing files. The tool copies files in-place, which forces ZFS to allocate new blocks in the most optimal pattern available, helping to improve read and write performance on fragmented pools. It works on any system with ZFS, including TrueNAS Scale.

It now supports multiple passes in sequence, continuing through all configured passes even when some files fail to rebalance, and includes a reasonable maximum concurrency limit of 128 to prevent resource exhaustion.

## Problem & Solution

When expanding a ZFS pool with additional storage devices, existing data remains on the original vdevs, creating an imbalanced distribution. This imbalance can significantly impact I/O performance, as the new devices remain underutilized while original devices continue to handle most of the workload.

go-zfs-rebalance addresses this by performing strategic in-place file rebalancing, which prompts ZFS to redistribute data blocks across all available vdevs. This process optimizes storage utilization and improves overall performance by:

- Balancing I/O load across all devices in the pool
- Utilizing the full bandwidth potential of newly added drives
- Reducing hotspots on heavily utilized original vdevs
- Improving parallel read/write operations across the expanded pool

The result is more consistent performance and better utilization of your entire storage infrastructure after pool expansion.

## Features

- **In-place file rebalancing**: Creates fresh copies of files to improve ZFS block allocation
- **Data integrity**: Verifies all files with SHA256 checksums (or MD5 if specified) to ensure perfect copies
- **Enhanced multi-pass capability**: Supports multiple rebalancing passes for heavily fragmented filesystems, continuing through all passes even when some files fail
- **Attribute preservation**: Maintains file permissions, timestamps, and ownership
- **Concurrent processing**: Multi-threaded design for high-performance operation (up to 128 concurrent jobs)
- **Graceful shutdown**: Safely handles interruptions with CTRL+C (finishes in-progress files)
- **Smart logging**: Configurable output verbosity with size-based filtering
- **Randomized processing**: Default randomized file handling for better I/O distribution
- **Hardlink awareness**: Safely skips hardlinked files by default to prevent duplication
- **Missing file handling**: Option to halt processing when files are no longer on disk

## Installation

### From Pre-built Binaries

Download the latest binary for your platform from the [Releases](https://github.com/astundzia/go-zfs-rebalance/releases) page.

#### Linux (AMD64/ARM64)
```bash
# Download the appropriate binary for your architecture
wget https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-linux-amd64
# OR for ARM64
wget https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-linux-arm64

# Download the SHA256 checksum file
wget https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-linux-amd64.sha256
# OR for ARM64
wget https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-linux-arm64.sha256

# Verify the checksum
sha256sum -c rebalance-linux-amd64.sha256
# OR for ARM64
sha256sum -c rebalance-linux-arm64.sha256

# Make it executable
chmod +x rebalance-linux-amd64

# Move to a directory in your PATH
sudo mv rebalance-linux-amd64 /usr/local/bin/rebalance
```

#### macOS (AMD64/ARM64)
```bash
# Download the appropriate binary for your architecture
curl -L -o rebalance https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-darwin-amd64
# OR for ARM64 (Apple Silicon)
curl -L -o rebalance https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-darwin-arm64

# Download the SHA256 checksum file
curl -L -o rebalance.sha256 https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-darwin-amd64.sha256
# OR for ARM64 (Apple Silicon)
curl -L -o rebalance.sha256 https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/rebalance-darwin-arm64.sha256

# Verify the checksum
shasum -a 256 -c rebalance.sha256

# Make it executable
chmod +x rebalance

# Move to a directory in your PATH
sudo mv rebalance /usr/local/bin/
```

#### Verify Installation
```bash
# Verify the installation
rebalance --version
```

### Building from Source

Requirements:
- Go 1.23 or later
- GCC (for CGO support)
- Docker and Docker Buildx (for cross-platform builds)

```bash
# Clone the repository
git clone https://github.com/astundzia/go-zfs-rebalance.git
cd go-zfs-rebalance

# Build for your local platform only
make build

# Build for all platforms (requires Docker and Docker Buildx)
./scripts/build-and-test.sh
```

## Usage

```
rebalance [options] <path>
```

### Important ZFS Considerations

- **⚠️ Snapshots Warning**: If ZFS snapshots are enabled on datasets being rebalanced, disk space will be consumed very rapidly as snapshots retain the original copy of each rebalanced file. Consider temporarily disabling snapshots during rebalancing.

- **⚠️ Disable Deduplication**: It is strongly recommended to disable deduplication on ZFS pools before rebalancing for optimal performance. Deduplication can significantly slow down the rebalancing process.

### Command-line Options

| Option | Description | Default |
|--------|-------------|---------|
| `--process-hardlinks` | Process files with multiple hardlinks (potentially increasing space usage) | Disabled |
| `--passes X` | Number of times a file may be rebalanced | 10 (0 = unlimited) |
| `--concurrency X` | Number of files to process concurrently | auto (half of CPU cores, minimum 2, maximum 128) |
| `--no-cleanup-balance` | Disable automatic removal of stale .balance files | Enabled |
| `--no-random` | Process files in directory order instead of random | Random enabled |
| `--checksum TYPE` | Checksum type to use (sha256 or md5) | sha256 |
| `--debug` | Enable debug logging (shows all operations) | Disabled |
| `--size-threshold X` | Only show success messages for files >= X MB | 0 MB |
| `--halt-on-missing` | Halt processing when a file is no longer on disk | Disabled |
| `--filename-only` | Display only filenames instead of full paths in logs | Full paths enabled |
| `--help` | Show help message | - |

### Examples

Basic usage (rebalance all files with default settings):
```bash
rebalance /path/to/data
```

Process hardlinks as well (could increase space usage if hardlinks span datasets):
```bash
rebalance --process-hardlinks --concurrency 8 /path/to/data
```

Run multiple rebalancing passes (for severely fragmented pools):
```bash
rebalance --passes 3 /path/to/data
```

Process files in alphabetical order instead of random:
```bash
rebalance --no-random /path/to/data
```

Disable automatic cleanup of temporary .balance files:
```bash
rebalance --no-cleanup-balance /path/to/data
```

Enable verbose debugging output:
```bash
rebalance --debug /path/to/data
```

Only show success messages for files 20MB or larger:
```bash
rebalance --size-threshold 20 /path/to/data
```

Halt processing when a file is found to be missing during rebalance:
```bash
rebalance --halt-on-missing /path/to/data
```

## How It Works

go-zfs-rebalance works by performing the following steps for each file:

1. **File Selection and Validation**:
   - Skips hard-linked files unless specifically enabled
   - Checks if the file has already reached the maximum pass count
   - Verifies the file exists and is a regular file

2. **Copying**:
   - Creates a new temporary file with the .balance extension
   - Performs a byte-by-byte copy to ensure a fresh allocation of blocks
   - The new file is written to a new physical location on disk

3. **Verification**:
   - Calculates and compares SHA256 checksums (or MD5 if specified) of the original and new file
   - Ensures data integrity during the rebalancing process

4. **Replacement**:
   - Removes the original file
   - Renames the temporary file to the original filename
   - Preserves all file attributes (permissions, timestamps, ownership)

5. **Pass Tracking**:
   - Records each successful rebalance in a SQLite database
   - Manages multi-pass rebalancing with a configurable limit

## Inspiration and Comparison

This project was inspired by the functionality of the original [zfs-inplace-rebalancing](https://github.com/markusressel/zfs-inplace-rebalancing) bash script by Markus Ressel. While the core concept remains the same (rebalancing by copying files in-place), this Go implementation offers several enhancements:

*   **Concurrency**: Utilizes Go routines and a `--concurrency` flag for parallel file processing, significantly speeding up operations on multi-core systems compared to the sequential nature of the bash script.
*   **Robust State Management**: Employs a persistent SQLite database to track file processing status and rebalance passes, offering better reliability and lookup performance than the script's text file (`rebalance_db.txt`).
*   **Resilient Multi-Pass Processing**: Intelligently continues through all configured passes even when some files fail to rebalance, with a reasonable concurrency limit (128) to prevent resource exhaustion.
*   **Graceful Shutdown & Error Handling**: Implements signal handling (CTRL+C) for graceful shutdowns, attempting to complete in-progress file operations and providing a timeout mechanism. This contrasts with the potential need for manual cleanup of `.balance` files if the bash script is interrupted.
*   **Automatic Cleanup**: Includes built-in logic (toggleable via `--no-cleanup-balance`) to automatically remove stale `.balance` files left over from previous runs or interruptions, improving robustness.
*   **Dependencies & Portability**: Compiles into a single, self-contained binary without external runtime dependencies (like `perl`, required by the bash script). This simplifies deployment across different Linux distributions and potentially other OSes.
*   **Enhanced Logging & Feedback**: Features a structured logging system (`logrus`) with customizable formatting, color-coding for status (copying, success, error), copy speed reporting, configurable verbosity (`--debug`), and periodic progress reports showing pass information and completion percentage.
*   **Randomized Processing**: Defaults to processing files in a random order (disable with `--no-random`) to potentially improve I/O distribution across vdevs during the rebalance operation.
*   **Success Message Filtering**: Offers a `--size-threshold` option to filter success logs, reducing noise for users primarily interested in the status of larger files.

## Progress Display

go-zfs-rebalance provides progress updates with:

- Periodic updates (every minute) showing overall progress
- Pass count and completion percentage
- Color-coded log messages:
  - Success messages in bold green
  - Warnings in yellow
  - Errors in red

## Technical Details

- **Cross-platform support**: Works on Linux, macOS, and Windows
- **Minimal dependencies**: Uses Go standard library when possible
- **SQLite database**: Tracks file rebalance counts for multi-pass operation
- **Controlled I/O**: Balances performance with system resource usage
- **Safe handling**: Graceful cleanup on interruption and error conditions

## Building for Different Platforms

The project includes scripts for building for multiple architectures. Docker and Docker Buildx are required for cross-platform builds:

```bash
# Build for all supported platforms (requires Docker and Docker Buildx)
./scripts/build-and-test.sh

# Package as a self-extracting multi-arch binary (requires Docker)
./scripts/package.sh
```

## Testing

Run the test suite with:

```bash
# Run all tests
go test -v ./...

# Run specific tests
go test -v ./pkg/rebalance
```

Integration tests create temporary files and verify integrity after rebalancing.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Acknowledgements

Special thanks to the ZFS community for insights on fragmentation and block allocation patterns.

## Disclaimer

**⚠️ IMPORTANT: Always back up your data before running any filesystem maintenance tools.** While go-zfs-rebalance verifies integrity during operation, unforeseen issues can occur.

- **Test in non-production environments first**: Validate the tool's behavior on test datasets before applying to critical data.
- **Consider ZFS snapshots**: Create ZFS snapshots before rebalancing as an additional safety measure.
- **System load**: Be aware that this tool can generate significant I/O and CPU load during operation.
- **Performance results may vary**: Improvements in fragmentation and performance depend on your specific ZFS configuration, usage patterns, and hardware.
- **No liability**: The authors and contributors of go-zfs-rebalance cannot be held responsible for any data loss, corruption, or system issues that may occur.
- **Snapshots warning**: If ZFS snapshots are enabled on datasets being rebalanced, you will rapidly consume disk space as each rebalanced file creates a new copy while snapshots retain the old copy.
- **Disable deduplication**: It is strongly recommended to disable deduplication on ZFS pools before rebalancing for best performance.

Use at your own risk and always ensure you have a tested recovery plan.

 