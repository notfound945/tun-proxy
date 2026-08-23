package system

import (
	"fmt"
	"os"

	"github.com/hailinpan/tun-proxy/internal/apperror"
)

func RequireRoot() error {
	if uid := os.Geteuid(); uid != 0 {
		return apperror.Wrap(apperror.CodeRootRequired, "runtime.preflight", "root privileges are required; run with sudo", fmt.Errorf("effective UID is %d", uid)).WithDetails(map[string]any{"effective_uid": uid})
	}
	return nil
}
