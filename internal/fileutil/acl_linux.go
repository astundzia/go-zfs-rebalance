//go:build linux

package fileutil

import "os"

// readACL returns nil: Linux ACLs (POSIX and NFSv4) are extended attributes, copied with the rest.
func readACL(*os.File) ([]byte, error) { return nil, nil }

// writeACL is never needed on Linux, because readACL never returns an ACL.
func writeACL(*os.File, []byte) error { return nil }

// clearACL does nothing: Linux ACLs can't stop a file from being removed.
func clearACL(*os.Root, string, FileID) {}
