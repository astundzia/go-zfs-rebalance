//go:build !linux

package rebalance

import "os"

// syncFilesystem does nothing: only Linux can sync a single filesystem (syncfs(2)).
func syncFilesystem(*os.File) error { return nil }
