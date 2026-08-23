package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateReleaseStopsWhenInstalledVersionIsLatest(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}
	scriptPath := filepath.Join(filepath.Dir(filename), "update-release.sh")
	tempDir := t.TempDir()
	bindir := filepath.Join(tempDir, "bin")
	fakeBin := filepath.Join(tempDir, "fake-bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(bindir, "tun-proxy"), `#!/bin/sh
if [ "${1:-}" = version ]; then
  printf '%s\n' 'tun-proxy 1.1.11 (commit test, built test)'
  exit 0
fi
exit 64
`)
	writeExecutable(t, filepath.Join(fakeBin, "id"), `#!/bin/sh
if [ "${1:-}" = -u ]; then
  printf '%s\n' 501
  exit 0
fi
exit 64
`)
	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "${1:-}" in
  -s) printf '%s\n' Darwin ;;
  -m) printf '%s\n' arm64 ;;
  *) exit 64 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
printf '%s\n' "$*" >> "$CURL_LOG"
printf '%s' "$LATEST_RELEASE_URL"
`)

	curlLog := filepath.Join(tempDir, "curl.log")
	command := exec.Command("/bin/bash", scriptPath)
	command.Env = testEnvironment(map[string]string{
		"PATH":                   fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PREFIX":                 tempDir,
		"BINDIR":                 bindir,
		"CONFIG_DIR":             filepath.Join(tempDir, "config"),
		"CONFIG_PATH":            filepath.Join(tempDir, "config", "config.yaml"),
		"INSTALL_RELEASE_SCRIPT": filepath.Join(tempDir, "missing-install-release.sh"),
		"CURL_LOG":               curlLog,
		"LATEST_RELEASE_URL":     "https://github.com/notfound945/tun-proxy/releases/tag/v1.1.11",
	})
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("update-release.sh failed: %v\n%s", err, output)
	}
	outputText := string(output)
	if !strings.Contains(outputText, "当前 CLI: tun-proxy 1.1.11") {
		t.Fatalf("missing current version output:\n%s", outputText)
	}
	if !strings.Contains(outputText, "已是最新版本: tun-proxy 1.1.11") {
		t.Fatalf("missing already-latest output:\n%s", outputText)
	}
	if strings.Contains(outputText, "下载安装程序") || strings.Contains(outputText, "下载 tun-proxy") {
		t.Fatalf("unexpected download after latest-version check:\n%s", outputText)
	}

	curlCalls, err := os.ReadFile(curlLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(curlCalls)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "releases/latest") {
		t.Fatalf("curl calls = %q, want only latest-release lookup", string(curlCalls))
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func testEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[key]; overridden {
				continue
			}
		}
		environment = append(environment, entry)
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}
