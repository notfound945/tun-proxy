package system

import (
	"fmt"
	"os"
)

func RequireRoot() error {
	if uid := os.Geteuid(); uid != 0 {
		return fmt.Errorf("root privileges are required (effective UID is %d); run with sudo", uid)
	}
	return nil
}
