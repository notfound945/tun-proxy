package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

const fallbackConfigPath = "./configs/config.yaml"

func defaultUserConfigPath() string {
	home, err := invokingUserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return fallbackConfigPath
	}
	return filepath.Join(home, ".config", "tun-proxy", "config.yaml")
}

func invokingUserHomeDir() (string, error) {
	if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); os.Geteuid() == 0 && sudoUser != "" && sudoUser != "root" {
		account, err := user.Lookup(sudoUser)
		if err == nil && strings.TrimSpace(account.HomeDir) != "" {
			return account.HomeDir, nil
		}
	}
	return os.UserHomeDir()
}
