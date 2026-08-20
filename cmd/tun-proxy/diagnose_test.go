package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func TestScanHostsConflictsUsesDNSLabelBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	contents := "127.0.0.1 corp.example\n127.0.0.1 service.example\n127.0.0.1 not-service.example\n127.0.0.1 exact.test\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	rules := []config.Rule{
		{ID: 1, DomainSuffixes: []string{"corp.example", "service.example"}, Outbound: "special"},
		{ID: 2, Domains: []string{"exact.test"}, Outbound: "special"},
		{ID: 3, Outbound: "default"},
	}
	conflicts, err := scanHostsConflicts(path, rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 3 {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	for _, conflict := range conflicts {
		if conflict.Domain == "not-service.example" {
			t.Fatal("not-service.example incorrectly matched the service.example suffix")
		}
	}
}

func TestCollectDiagnosisRetainsPartialResults(t *testing.T) {
	report := collectDiagnosis(t.Context(), diagnosisOptions{
		ConfigPath: "../../configs/config.yaml",
		StatePath:  filepath.Join(t.TempDir(), "missing-state.json"),
		HostsPath:  filepath.Join(t.TempDir(), "missing-hosts"),
	})
	if !report.Config.Loaded || report.Config.Digest == "" {
		t.Fatalf("config result = %+v", report.Config)
	}
	if report.Overall == "ok" || len(report.Checks) == 0 {
		t.Fatalf("partial report did not retain warnings: %+v", report)
	}
}
