//go:build darwin

package status

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	procInfoCallPIDInfo = 2
	procPIDListFDs      = 1
	procFDInfoSize      = 8
)

// openFDCount uses the native proc_info syscall because Go's directory reader
// returns no entries for macOS's special /dev/fd filesystem.
func openFDCount() (uint64, error) {
	entryCapacity := 64
	maximum := unix.Getdtablesize()
	for entryCapacity <= maximum {
		buffer := make([]byte, entryCapacity*procFDInfoSize)
		bytesWritten, _, errno := unix.Syscall6(
			unix.SYS_PROC_INFO,
			procInfoCallPIDInfo,
			uintptr(os.Getpid()),
			procPIDListFDs,
			0,
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer)),
		)
		runtime.KeepAlive(buffer)
		if errno != 0 {
			return 0, fmt.Errorf("list process file descriptors: %w", errno)
		}
		if bytesWritten%procFDInfoSize != 0 {
			return 0, fmt.Errorf("list process file descriptors returned %d unaligned bytes", bytesWritten)
		}
		if bytesWritten < uintptr(len(buffer)) {
			return uint64(bytesWritten / procFDInfoSize), nil
		}
		entryCapacity *= 2
	}
	return 0, fmt.Errorf("open file descriptor count exceeds process table size %d", maximum)
}
