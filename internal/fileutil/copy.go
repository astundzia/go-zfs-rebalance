package fileutil

import (
	"bytes"
	"context"
	"hash"
	"io"
	"os"
)

// zeroChunk is the granularity at which all-zero data is skipped with a seek instead of written,
// so holes in sparse files stay holes even when compression is off. It matches the default ZFS
// recordsize. SEEK_DATA/SEEK_HOLE are deliberately not used: they were behind the OpenZFS 2.2
// hole-reporting corruption bug.
const zeroChunk = 128 << 10

var zeroes [zeroChunk]byte

// plainReader and plainWriter expose only what the copy loop needs. *os.File also has ReadFrom and
// WriteTo, which io.Copy and friends use to reach copy_file_range or sendfile; with block cloning
// (OpenZFS 2.2+) those share the old blocks instead of writing new ones, which would make the
// rewrite a silent no-op. Hiding them keeps every byte going through read(2) and write(2).
type plainReader struct{ f *os.File }

func (r plainReader) Read(p []byte) (int, error) { return r.f.Read(p) }

type plainWriter struct{ f *os.File }

func (w plainWriter) Write(p []byte) (int, error)                  { return w.f.Write(p) }
func (w plainWriter) Seek(offset int64, whence int) (int64, error) { return w.f.Seek(offset, whence) }
func (w plainWriter) Truncate(size int64) error                    { return w.f.Truncate(size) }

// copyData copies src into dst through buf and feeds every byte read to h. It returns ErrModified
// as soon as src turns out to be longer than limit, so a growing file can't keep it busy forever.
func copyData(ctx context.Context, dst plainWriter, src plainReader, h hash.Hash, buf []byte, limit int64) (int64, error) {
	var total int64
	holeAtEnd := false
	for {
		if err := ctx.Err(); err != nil {
			return total, fail("", err)
		}
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return total, failKind(ErrModified, "", nil)
			}
			h.Write(buf[:n])
			var err error
			if holeAtEnd, err = writeSparse(dst, buf[:n]); err != nil {
				return total, fail("couldn't write the new copy", err)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return total, fail("couldn't read the file", rerr)
		}
	}
	// Trailing zero chunks were skipped, so set the final length explicitly. Only then: on ZFS a
	// truncate checks the copy's ACL again, and the copy already has the original's.
	if holeAtEnd {
		if err := dst.Truncate(total); err != nil {
			return total, fail("couldn't write the new copy", err)
		}
	}
	return total, nil
}

// writeSparse writes p at dst's current offset, seeking over runs of all-zero chunks instead of
// writing them. holeAtEnd reports whether the end of p was skipped rather than written.
func writeSparse(dst plainWriter, p []byte) (holeAtEnd bool, err error) {
	for len(p) > 0 {
		n := min(zeroChunk, len(p))
		zero := isZero(p[:n])
		for n < len(p) {
			next := min(n+zeroChunk, len(p))
			if isZero(p[n:next]) != zero {
				break
			}
			n = next
		}
		if zero {
			_, err = dst.Seek(int64(n), io.SeekCurrent)
		} else {
			_, err = dst.Write(p[:n])
		}
		if err != nil {
			return false, err
		}
		holeAtEnd = zero
		p = p[n:]
	}
	return holeAtEnd, nil
}

func isZero(p []byte) bool { return bytes.Equal(p, zeroes[:len(p)]) }

// verifyCopy reads tmp back from the start with plain reads and checks that it has exactly size
// bytes hashing to want.
func verifyCopy(ctx context.Context, tmp *os.File, buf []byte, h hash.Hash, want []byte, size int64) error {
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail("couldn't read back the new copy", err)
	}
	src := plainReader{tmp}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return fail("", err)
		}
		n, err := io.ReadFull(src, buf)
		total += int64(n)
		h.Write(buf[:n])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return fail("couldn't read back the new copy", err)
		}
	}
	if total != size || !bytes.Equal(h.Sum(nil), want) {
		return failKind(ErrChecksum, "", nil)
	}
	return nil
}
