package fileutil

import (
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime"
)

// GetLinkCount returns the number of hardlinks to a file.
func GetLinkCount(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}

	nlink, err := getLinkCountForPlatform(info)
	if err != nil {
		return 0, fmt.Errorf("unsupported system for file %s: %w", path, err)
	}

	return nlink, nil
}

// CheckAttributes checks basic attributes: size, mode, uid, gid, and modification time.
func CheckAttributes(orig, copy string) (bool, string) {
	origInfo, err := os.Stat(orig)
	if err != nil {
		return false, fmt.Sprintf("cannot stat original file: %v", err)
	}

	copyInfo, err := os.Stat(copy)
	if err != nil {
		return false, fmt.Sprintf("cannot stat copy file: %v", err)
	}

	// Size
	if origInfo.Size() != copyInfo.Size() {
		return false, "size mismatch"
	}

	// Mode
	if origInfo.Mode() != copyInfo.Mode() {
		return false, "mode mismatch"
	}

	// Compare UID/GID if possible
	if runtime.GOOS != "windows" {
		origUID, origGID, err1 := getFileOwnership(origInfo)
		copyUID, copyGID, err2 := getFileOwnership(copyInfo)

		if err1 == nil && err2 == nil {
			if origUID != copyUID {
				return false, "uid mismatch"
			}
			if origGID != copyGID {
				return false, "gid mismatch"
			}
		}
	}

	// Compare modification time
	if !origInfo.ModTime().Equal(copyInfo.ModTime()) {
		return false, "mod time mismatch"
	}

	return true, ""
}

// ChecksumType defines the type of checksum to use
type ChecksumType string

const (
	// ChecksumSHA256 uses SHA256 for file verification
	ChecksumSHA256 ChecksumType = "sha256"
	// ChecksumMD5 uses MD5 for file verification
	ChecksumMD5 ChecksumType = "md5"
)

// CompareFileChecksum compares two files by their checksums using the specified algorithm.
// SHA256 is used by default.
func CompareFileChecksum(orig, copy string, checksumType ChecksumType) (bool, string) {
	switch checksumType {
	case ChecksumMD5:
		return CompareFileMD5(orig, copy)
	case ChecksumSHA256:
		return CompareFileSHA256(orig, copy)
	default:
		// Default to SHA256
		return CompareFileSHA256(orig, copy)
	}
}

// CompareFileMD5 compares two files by their MD5 checksums.
func CompareFileMD5(orig, copy string) (bool, string) {
	origHash, err := FileHashMD5(orig)
	if err != nil {
		return false, fmt.Sprintf("error hashing original: %v", err)
	}

	copyHash, err := FileHashMD5(copy)
	if err != nil {
		return false, fmt.Sprintf("error hashing copy: %v", err)
	}

	if origHash != copyHash {
		return false, fmt.Sprintf("MD5 mismatch: %s != %s", origHash, copyHash)
	}

	return true, ""
}

// CompareFileSHA256 compares two files by their SHA256 checksums.
func CompareFileSHA256(orig, copy string) (bool, string) {
	origHash, err := FileHashSHA256(orig)
	if err != nil {
		return false, fmt.Sprintf("error hashing original: %v", err)
	}

	copyHash, err := FileHashSHA256(copy)
	if err != nil {
		return false, fmt.Sprintf("error hashing copy: %v", err)
	}

	if origHash != copyHash {
		return false, fmt.Sprintf("SHA256 mismatch: %s != %s", origHash, copyHash)
	}

	return true, ""
}

// FileHashMD5 returns the hexadecimal MD5 of a file.
func FileHashMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	_, err = io.Copy(h, f)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// FileHashSHA256 returns the hexadecimal SHA256 of a file.
func FileHashSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	_, err = io.Copy(h, f)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// CopyFile copies src to dst, preserving the mode and mod time. Does not handle reflinks.
func CopyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	statSrc, err := s.Stat()
	if err != nil {
		return err
	}

	d, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, statSrc.Mode())
	if err != nil {
		return err
	}
	defer d.Close()

	if _, err = io.Copy(d, s); err != nil {
		return err
	}

	// Preserve mod time
	return os.Chtimes(dst, statSrc.ModTime(), statSrc.ModTime())
}
