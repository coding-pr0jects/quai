package lock

import "syscall"

// ReadAvailableDisk returns the bytes available to unprivileged users on the
// filesystem that holds path. Returns (0, false) when the filesystem cannot be
// inspected.
func ReadAvailableDisk(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
