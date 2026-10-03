//go:build linux || darwin

package fileutil

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"hash"
	"io/fs"
	"os"
	"path"
	"slices"
	"syscall"
	"time"
)

// beforeSwapHook, when set by tests, runs right before the final check that the file is unchanged.
var beforeSwapHook func()

// tempAttempts bounds retries when a randomly chosen temporary name is already taken.
const tempAttempts = 10

// ReplaceGroup is ReplaceInPlace for a set of hardlinks. rels must be every name of one inode
// (len(rels) equal to its link count, all inside root); rels[0] is the primary name.
//
// The data is copied and verified once. Each other name is then switched over by linking the copy
// under a new temporary name next to it, checking that the name still refers to the original, and
// renaming the link over it; rels[0] is switched last. Every switch is atomic, so if the process
// stops part-way the names already switched point at the new, identical copy and nothing is lost.
func ReplaceGroup(ctx context.Context, root *os.Root, rels []string, opts Options) (Result, error) {
	start := time.Now()
	h, err := opts.Checksum.newHash()
	if err != nil {
		return Result{}, err
	}
	if len(rels) == 0 {
		return Result{}, errors.New("no file names were given")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fail("", err)
	}
	r := &replacer{root: root, names: make([]string, len(rels))}
	for i, rel := range rels {
		r.names[i] = path.Clean(rel)
	}
	defer r.cleanup()

	res, err := r.run(ctx, h, opts.BufferSize)
	if err != nil {
		return Result{}, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

func (c ChecksumType) newHash() (hash.Hash, error) {
	switch c {
	case "", ChecksumSHA256:
		return sha256.New(), nil
	case ChecksumMD5:
		return md5.New(), nil
	}
	_, err := ParseChecksumType(string(c))
	return nil, err
}

// replacer holds the state of one ReplaceGroup call so that cleanup can undo it.
type replacer struct {
	root  *os.Root
	names []string
	src   *os.File
	tmp   *os.File // open until it is closed successfully
	tmpID FileID
	temps []string // temporary names created and not yet renamed over a real name
}

func (r *replacer) run(ctx context.Context, h hash.Hash, bufSize int) (Result, error) {
	before, err := r.openSource()
	if err != nil {
		return Result{}, err
	}

	tmp, tmpRel, err := createTemp(r.root, path.Dir(r.names[0]), randomTempName)
	if err != nil {
		return Result{}, fail("couldn't create a temporary copy next to the file", err)
	}
	r.tmp = tmp
	r.temps = append(r.temps, tmpRel)
	tmpInfo, err := statFile(tmp)
	if err != nil {
		return Result{}, fail("couldn't check the new copy", err)
	}
	r.tmpID = tmpInfo.ID

	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	if before.Size < int64(bufSize) {
		bufSize = int(before.Size) + 1 // small files: one read, plus room to see the end
	}
	buf := make([]byte, bufSize)

	n, err := copyData(ctx, plainWriter{tmp}, plainReader{r.src}, h, buf, before.Size)
	if err != nil {
		return Result{}, err
	}
	if n != before.Size {
		return Result{}, failKind(ErrModified, "", nil)
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, fail("couldn't save the new copy to disk", err)
	}
	sum := h.Sum(nil)
	h.Reset()
	if err := verifyCopy(ctx, tmp, buf, h, sum, n); err != nil {
		return Result{}, err
	}

	if err := copyMetadata(r.root, r.src, tmp, tmpRel, before); err != nil {
		return Result{}, err
	}
	r.tmp = nil
	if err := tmp.Close(); err != nil {
		return Result{}, fail("couldn't save the new copy to disk", err)
	}

	if beforeSwapHook != nil {
		beforeSwapHook()
	}
	if err := r.checkUnchanged(before, tmpRel); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fail("", err)
	}
	if err := r.swap(before, tmpRel); err != nil {
		return Result{}, err
	}
	syncDirs(r.root, r.names)
	return Result{Size: n, Before: before, NewID: r.tmpID}, nil
}

// openSource checks that every name is a regular file and one of the inode's names, then opens it.
func (r *replacer) openSource() (Info, error) {
	type entry struct {
		dir  FileID
		base string
	}
	seen := make(map[entry]bool, len(r.names))
	var first Info
	for i, name := range r.names {
		info, err := Lstat(r.root, name)
		if err != nil {
			return Info{}, fail("couldn't check the file", err)
		}
		if !info.Mode.IsRegular() {
			return Info{}, failKind(ErrNotRegular, "", nil)
		}
		if i == 0 {
			first = info
		} else if info.ID != first.ID {
			return Info{}, failKind(ErrLinkMismatch, "", nil)
		}
		if len(r.names) > 1 {
			// Two spellings of the same directory entry would leave a real name behind.
			fi, err := r.root.Stat(path.Dir(name))
			if err != nil {
				return Info{}, fail("couldn't check the file", err)
			}
			dir, err := InfoOf(fi)
			if err != nil {
				return Info{}, fail("couldn't check the file", err)
			}
			e := entry{dir.ID, path.Base(name)}
			if seen[e] {
				return Info{}, failKind(ErrLinkMismatch, "the same name was given twice", nil)
			}
			seen[e] = true
		}
	}

	// O_NONBLOCK keeps a pipe swapped in at the last moment from blocking the open.
	src, err := r.root.OpenFile(r.names[0], os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Info{}, fail("couldn't open the file", err)
	}
	r.src = src
	before, err := statFile(src)
	switch {
	case err != nil:
		return Info{}, fail("couldn't check the file", err)
	case !before.Mode.IsRegular():
		return Info{}, failKind(ErrNotRegular, "", nil)
	case before.ID != first.ID:
		return Info{}, failKind(ErrModified, "", nil)
	case before.Nlink != uint64(len(r.names)):
		return Info{}, failKind(ErrLinkMismatch, "", nil)
	}
	return before, nil
}

// checkUnchanged makes sure nothing touched the original or the copy while the copy was made.
func (r *replacer) checkUnchanged(before Info, tmpRel string) error {
	for _, name := range r.names {
		now, err := Lstat(r.root, name)
		if err != nil {
			return fail("couldn't check the file", err)
		}
		if now.ID != before.ID || now.Size != before.Size || !now.Mtime.Equal(before.Mtime) ||
			!now.Ctime.Equal(before.Ctime) || now.Nlink != before.Nlink {
			return failKind(ErrModified, "", nil)
		}
	}
	return r.checkTemp(tmpRel)
}

// checkTemp makes sure tempRel is still a name of our verified copy.
func (r *replacer) checkTemp(tempRel string) error {
	if now, err := Lstat(r.root, tempRel); err != nil || now.ID != r.tmpID {
		return failKind(ErrModified, "its temporary copy was removed or replaced by another program", err)
	}
	return nil
}

// swap renames the verified copy over every name, the primary last. Link counts and ctimes change
// as names move over, so each name is only checked for identity, size and mtime.
func (r *replacer) swap(before Info, tmpRel string) error {
	for i := 1; i < len(r.names); i++ {
		linkRel, err := linkTemp(r.root, tmpRel, path.Dir(r.names[i]), randomTempName)
		if err != nil {
			return r.partial(i-1, fail("couldn't link the new copy to the file's other names", err))
		}
		r.temps = append(r.temps, linkRel)
		if err := r.swapOne(before, linkRel, r.names[i]); err != nil {
			return r.partial(i-1, err)
		}
	}
	if err := r.swapOne(before, tmpRel, r.names[0]); err != nil {
		return r.partial(len(r.names)-1, err)
	}
	return nil
}

func (r *replacer) swapOne(before Info, tempRel, name string) error {
	if err := r.checkTemp(tempRel); err != nil {
		return err
	}
	now, err := Lstat(r.root, name)
	switch {
	case err != nil:
		return fail("couldn't check the file", err)
	case now.ID != before.ID:
		return failKind(ErrLinkMismatch, "", nil)
	case now.Size != before.Size || !now.Mtime.Equal(before.Mtime):
		return failKind(ErrModified, "", nil)
	}
	if err := r.root.Rename(tempRel, name); err != nil {
		return fail("couldn't swap in the new copy", err)
	}
	r.temps = slices.DeleteFunc(r.temps, func(t string) bool { return t == tempRel })
	return nil
}

// partial records on err how many names were already switched to the new copy.
func (r *replacer) partial(swapped int, err error) error {
	var f *failure
	if swapped > 0 && errors.As(err, &f) {
		f.swapped, f.names = swapped, len(r.names)
	}
	return err
}

// cleanup closes the files and removes every temporary name that was not renamed over a real name,
// which after a failure is all of them.
func (r *replacer) cleanup() {
	if r.tmp != nil {
		_ = r.tmp.Close()
	}
	if r.src != nil {
		_ = r.src.Close()
	}
	for _, rel := range r.temps {
		if err := r.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			clearACL(r.root, rel, r.tmpID)
			_ = r.root.Remove(rel)
		}
	}
}

// createTemp creates a new, empty temporary file in dir, named by newName. O_EXCL guarantees the
// file is new, so a symlink, pipe or user file that already has the name is never followed, opened
// or truncated; another name is tried instead.
func createTemp(root *os.Root, dir string, newName func() string) (*os.File, string, error) {
	var err error
	for range tempAttempts {
		rel := path.Join(dir, newName())
		var f *os.File
		f, err = root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		fi, err := f.Stat()
		if err == nil && !fi.Mode().IsRegular() {
			err = errors.New("the temporary copy isn't a regular file")
		}
		if err != nil {
			_ = f.Close()
			_ = root.Remove(rel)
			return nil, "", err
		}
		return f, rel, nil
	}
	return nil, "", err
}

// linkTemp adds a new temporary name in dir for the file target, retrying if the name is taken.
func linkTemp(root *os.Root, target, dir string, newName func() string) (string, error) {
	var err error
	for range tempAttempts {
		rel := path.Join(dir, newName())
		err = root.Link(target, rel)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return rel, nil
	}
	return "", err
}

// syncDirs flushes the directories holding names so the renames survive a crash. It is best effort:
// the swap has already happened, and some filesystems can't sync directories.
func syncDirs(root *os.Root, names []string) {
	done := make(map[string]bool, 1)
	for _, name := range names {
		dir := path.Dir(name)
		if done[dir] {
			continue
		}
		done[dir] = true
		if d, err := root.Open(dir); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
}
