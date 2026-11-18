//go:build unix

package server

import "syscall"

// filesystemSize returns the total size in bytes of the filesystem holding
// dir, or 0 if it cannot be determined.
func filesystemSize(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Blocks) * int64(st.Bsize)
}
