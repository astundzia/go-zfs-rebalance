//go:build !linux && !darwin

package fileutil

import (
	"context"
	"os"
)

const immutableHint = "read-only"

// InfoOf is only implemented on Linux and macOS.
func InfoOf(os.FileInfo) (Info, error) { return Info{}, ErrUnsupportedPlatform }

// ReplaceGroup is only implemented on Linux and macOS.
func ReplaceGroup(context.Context, *os.Root, []string, Options) (Result, error) {
	return Result{}, ErrUnsupportedPlatform
}

// RemoveStaleTemp is only implemented on Linux and macOS.
func RemoveStaleTemp(*os.Root, string) (bool, error) { return false, ErrUnsupportedPlatform }
