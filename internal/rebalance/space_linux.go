package rebalance

import "golang.org/x/sys/unix"

// blockSize is the unit of a statfs block count: the fragment size where the filesystem reports
// one, as statvfs uses.
func blockSize(st *unix.Statfs_t) uint64 {
	if st.Frsize > 0 {
		return uint64(st.Frsize)
	}
	return uint64(st.Bsize)
}
