//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd

package ui

func checkTerminalFD(fd uintptr) bool {
	return true
}
