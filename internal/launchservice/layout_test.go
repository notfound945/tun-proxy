package launchservice

import (
	"encoding/xml"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestManifestUsesFixedProgramArgumentsWithoutBootStartByDefault(t *testing.T) {
	layout := testLayout(t)
	contents, err := Manifest(layout, false)
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(strings.NewReader(string(contents)))
	for {
		if _, err := decoder.Token(); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("manifest is not valid XML: %v", err)
		}
	}
	text := string(contents)
	for _, required := range []string{
		"<string>" + layout.Binary + "</string>",
		"<string>_service-run</string>",
		"<string>-config</string>",
		"<string>" + layout.Config + "</string>",
		"<key>RunAtLoad</key>\n  <false/>",
		"<key>ExitTimeOut</key>\n  <integer>45</integer>",
		"<key>Umask</key>\n  <integer>23</integer>",
		layout.StandardOut,
		layout.StandardErr,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("manifest missing %q", required)
		}
	}
	if strings.Contains(text, "<key>Program</key>") || strings.Contains(text, "/bin/sh") {
		t.Fatal("manifest must not invoke a shell")
	}
	if strings.Contains(text, "<key>KeepAlive</key>") {
		t.Fatal("manifest with boot startup disabled must not contain implicit KeepAlive startup")
	}
}

func TestManifestEnablesBootStartAndFailureRestartWhenRequested(t *testing.T) {
	layout := testLayout(t)
	contents, err := Manifest(layout, true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, required := range []string{
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>\n    <false/>",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("boot-start manifest missing %q", required)
		}
	}
}

func TestManifestStartAtBootReadsGeneratedPolicy(t *testing.T) {
	layout := testLayout(t)
	for _, want := range []bool{false, true} {
		contents, err := Manifest(layout, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ManifestStartAtBoot(contents)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("ManifestStartAtBoot() = %t, want %t", got, want)
		}
	}
}

func TestManifestStartAtBootRejectsMissingPolicy(t *testing.T) {
	if _, err := ManifestStartAtBoot([]byte("<plist><dict/></plist>")); err == nil {
		t.Fatal("manifest without RunAtLoad was accepted")
	}
}

func TestValidateManagedConfig(t *testing.T) {
	layout := DefaultLayout()
	runtime, err := config.LoadFile("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedConfig(runtime, layout); err != nil {
		t.Fatalf("canonical config rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"state", func(value *config.Config) { value.System.StateFile += ".other" }},
		{"lock", func(value *config.Config) { value.System.LockFile += ".other" }},
		{"fake IPv4", func(value *config.Config) { value.FakeIP.PersistenceFile += ".other" }},
		{"fake IPv6", func(value *config.Config) { value.FakeIPv6.PersistenceFile += ".other" }},
		{"UDP DNS disabled", func(value *config.Config) { value.DNS.UDP = false }},
		{"TCP DNS disabled", func(value *config.Config) { value.DNS.TCP = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := *runtime
			if runtime.FakeIPv6 != nil {
				ipv6 := *runtime.FakeIPv6
				copy.FakeIPv6 = &ipv6
			}
			test.mutate(&copy)
			if err := ValidateManagedConfig(&copy, layout); err == nil {
				t.Fatal("unsafe managed path was accepted")
			}
		})
	}
}

func TestDefaultLayoutDefinesDedicatedWorkerContract(t *testing.T) {
	layout := DefaultLayout()
	if layout.WorkerUser != "_tun-proxy" || layout.WorkerGroup != "_tun-proxy" {
		t.Fatalf("worker identity = %s:%s", layout.WorkerUser, layout.WorkerGroup)
	}
	if layout.WorkerDir != "/var/run/tun-proxy/worker" || layout.StatusSocket != "/var/run/tun-proxy/worker/status.sock" {
		t.Fatalf("worker runtime layout = dir=%q socket=%q", layout.WorkerDir, layout.StatusSocket)
	}
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultLayoutUsesCurrentLaunchdIdentity(t *testing.T) {
	layout := DefaultLayout()
	if layout.Label != "cn.notfound945.tun-proxy" {
		t.Fatalf("label = %q", layout.Label)
	}
	if layout.Binary != "/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy" {
		t.Fatalf("binary = %q", layout.Binary)
	}
	if layout.Plist != "/Library/LaunchDaemons/cn.notfound945.tun-proxy.plist" {
		t.Fatalf("plist = %q", layout.Plist)
	}
}

func TestLayoutRejectsStatusSocketOutsideWorkerDirectory(t *testing.T) {
	layout := testLayout(t)
	layout.StatusSocket = filepath.Join(layout.RuntimeDir, "status.sock")
	if err := layout.Validate(); err == nil || !strings.Contains(err.Error(), "worker runtime directory") {
		t.Fatalf("Validate() error = %v", err)
	}
}
