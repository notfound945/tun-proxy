// Package launchservice manages the macOS LaunchDaemon installation for
// tun-proxy. The daemon still runs as root in this delivery slice; the package
// deliberately does not claim to be the later least-privilege helper.
package launchservice

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/config"
)

const Label = "cn.notfound945.tun-proxy"

type Layout struct {
	Label        string
	Binary       string
	Config       string
	Plist        string
	LogDirectory string
	StandardOut  string
	StandardErr  string
	RuntimeDir   string
	WorkerUser   string
	WorkerGroup  string
	WorkerDir    string
	StatusSocket string
	DataDir      string
	State        string
	Lock         string
	FakeIPv4     string
	FakeIPv6     string
}

func DefaultLayout() Layout {
	return Layout{
		Label:        Label,
		Binary:       "/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy",
		Config:       "/Library/Application Support/tun-proxy/config.yaml",
		Plist:        "/Library/LaunchDaemons/cn.notfound945.tun-proxy.plist",
		LogDirectory: "/Library/Logs/tun-proxy",
		StandardOut:  "/Library/Logs/tun-proxy/stdout.log",
		StandardErr:  "/Library/Logs/tun-proxy/stderr.log",
		RuntimeDir:   "/var/run/tun-proxy",
		WorkerUser:   "_tun-proxy",
		WorkerGroup:  "_tun-proxy",
		WorkerDir:    "/var/run/tun-proxy/worker",
		StatusSocket: "/var/run/tun-proxy/worker/status.sock",
		DataDir:      "/var/lib/tun-proxy",
		State:        "/var/run/tun-proxy/state.json",
		Lock:         "/var/run/tun-proxy/tun-proxy.lock",
		FakeIPv4:     "/var/lib/tun-proxy/fake-ip.yaml",
		FakeIPv6:     "/var/lib/tun-proxy/fake-ipv6.yaml",
	}
}

func (layout Layout) Validate() error {
	if layout.Label == "" {
		return fmt.Errorf("launchd label is required")
	}
	if layout.WorkerUser == "" || strings.ContainsAny(layout.WorkerUser, "/:\x00") {
		return fmt.Errorf("service worker user is invalid: %q", layout.WorkerUser)
	}
	if layout.WorkerGroup == "" || strings.ContainsAny(layout.WorkerGroup, "/:\x00") {
		return fmt.Errorf("service worker group is invalid: %q", layout.WorkerGroup)
	}
	for name, path := range map[string]string{
		"binary": layout.Binary, "config": layout.Config, "plist": layout.Plist,
		"log directory": layout.LogDirectory, "stdout": layout.StandardOut,
		"stderr": layout.StandardErr, "runtime directory": layout.RuntimeDir,
		"worker runtime directory": layout.WorkerDir, "status socket": layout.StatusSocket,
		"data directory": layout.DataDir, "state": layout.State, "lock": layout.Lock,
		"Fake IPv4 persistence": layout.FakeIPv4, "Fake IPv6 persistence": layout.FakeIPv6,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("service %s must be a clean absolute path: %q", name, path)
		}
	}
	if filepath.Dir(layout.StatusSocket) != layout.WorkerDir {
		return fmt.Errorf("service status socket must be directly inside worker runtime directory %q", layout.WorkerDir)
	}
	if len(layout.StatusSocket) > 103 {
		return fmt.Errorf("service status socket exceeds the macOS path limit: %q", layout.StatusSocket)
	}
	return nil
}

// ValidateManagedConfig confines root service writes to the fixed recovery and
// persistence paths installed for the LaunchDaemon.
func ValidateManagedConfig(runtime *config.Config, layout Layout) error {
	if runtime == nil {
		return fmt.Errorf("service configuration is required")
	}
	if err := layout.Validate(); err != nil {
		return err
	}
	if !runtime.DNS.UDP || !runtime.DNS.TCP {
		return fmt.Errorf("managed service requires both UDP and TCP Fake DNS listeners")
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{name: "system.state_file", got: runtime.System.StateFile, want: layout.State},
		{name: "system.lock_file", got: runtime.System.LockFile, want: layout.Lock},
		{name: "fake_ip.persistence_file", got: runtime.FakeIP.PersistenceFile, want: layout.FakeIPv4},
	}
	if runtime.FakeIPv6 != nil {
		checks = append(checks, struct {
			name string
			got  string
			want string
		}{name: "fake_ipv6.persistence_file", got: runtime.FakeIPv6.PersistenceFile, want: layout.FakeIPv6})
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("managed service requires %s=%q, got %q", check.name, check.want, check.got)
		}
	}
	return nil
}

func Manifest(layout Layout) ([]byte, error) {
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	output.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>`)
	writeXML(&output, layout.Label)
	output.WriteString(`</string>
  <key>ProgramArguments</key>
  <array>
    <string>`)
	writeXML(&output, layout.Binary)
	output.WriteString(`</string>
    <string>_service-run</string>
    <string>-config</string>
    <string>`)
	writeXML(&output, layout.Config)
	output.WriteString(`</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ExitTimeOut</key>
  <integer>45</integer>
  <key>Umask</key>
  <integer>23</integer>
  <key>StandardOutPath</key>
  <string>`)
	writeXML(&output, layout.StandardOut)
	output.WriteString(`</string>
  <key>StandardErrorPath</key>
  <string>`)
	writeXML(&output, layout.StandardErr)
	output.WriteString(`</string>
</dict>
</plist>
`)
	return output.Bytes(), nil
}

func writeXML(output *bytes.Buffer, value string) {
	_ = xml.EscapeText(output, []byte(value))
}
