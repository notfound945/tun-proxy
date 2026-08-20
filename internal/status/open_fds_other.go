//go:build !darwin

package status

import (
	"fmt"
	"os"
)

func openFDCount() (uint64, error) {
	for _, path := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(path)
		if err == nil {
			return uint64(len(entries)), nil
		}
	}
	return 0, fmt.Errorf("file descriptor enumeration is unavailable")
}
