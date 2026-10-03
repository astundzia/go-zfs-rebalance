package rebalance

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFilesystem asks the filesystem holding the open folder d to write out everything it is
// holding (syncfs(2)).
func syncFilesystem(d *os.File) error {
	conn, err := d.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	if err := conn.Control(func(fd uintptr) { syncErr = unix.Syncfs(int(fd)) }); err != nil {
		return err
	}
	if syncErr != nil {
		return &os.PathError{Op: "syncfs", Path: d.Name(), Err: syncErr}
	}
	return nil
}
