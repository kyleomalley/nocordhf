//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// redirectStderr points the Windows STD_ERROR_HANDLE at the given file
// so the Go runtime's panic destination (which writes to FD 2 via the
// Windows handle table) lands in nocordhf-stderr.log instead of the
// detached-process void.
//
// POSIX equivalent in redirect_unix.go uses syscall.Dup2; Windows
// doesn't have dup2 but SetStdHandle gives equivalent semantics —
// after this call, both os.Stderr writes AND raw runtime panic
// writes target the file's underlying Win32 handle.
func redirectStderr(f *os.File) error {
	return windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
}
