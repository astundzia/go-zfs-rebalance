# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.1] - 2024-04-08

### Fixed
- Fixed bug where multiple passes would stop after the first pass when some files failed to rebalance
- Now continues processing through all configured passes even when some files fail

### Added
- Maximum concurrency limit (128) to prevent resource exhaustion
- Enhanced multi-pass capability that continues through all passes
- Updated documentation with improved multi-pass details

## [1.0.0] - 2024-04-06

### Added
- Initial public release
- In-place file rebalancing with checksums verification
- Multi-pass rebalancing for heavily fragmented file systems
- Concurrent processing with automatic thread calculation
- Progress display with pass information
- Database tracking of rebalanced files
- Graceful shutdown handling
- Size-based filtering for log messages
- Hardlink awareness and skipping
- Customizable options for processing 