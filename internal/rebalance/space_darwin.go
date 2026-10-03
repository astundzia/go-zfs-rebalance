package rebalance

import "golang.org/x/sys/unix"

// blockSize is the unit of a statfs block count.
func blockSize(st *unix.Statfs_t) uint64 { return uint64(st.Bsize) }
