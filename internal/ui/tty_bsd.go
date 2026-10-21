//go:build darwin || freebsd || netbsd || openbsd

package ui

import (
	"syscall"
	"unsafe"
)

func checkTerminalFD(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return err == 0
}
