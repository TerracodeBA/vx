//go:build unix

package service

import "syscall"

// diskSpace is the free (for unprivileged users) and total bytes of the
// file system that holds dir.
func diskSpace(dir string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}
