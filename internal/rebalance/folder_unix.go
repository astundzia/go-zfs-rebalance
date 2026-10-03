//go:build linux || darwin

package rebalance

import (
	"os"
	"syscall"
)

// openDir opens the folder rel inside root, to check it and set its times through the same
// descriptor. Something other than a folder in its place gives an error (and a pipe can't block the
// open); a symlink to another folder in the root is followed, so the caller checks which folder it
// got.
func openDir(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY, 0)
}
