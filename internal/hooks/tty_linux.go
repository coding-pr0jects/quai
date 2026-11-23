//go:build linux

package hooks

import (
	"syscall"
	"unsafe"
)

// isTerminal asks the kernel whether fd is a terminal. An ioctl keeps terminal
// detection inside the standard library, since quai allows no extra dependency.
func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}
