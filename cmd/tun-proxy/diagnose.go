package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/interfaceinfo"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	runtimestatus "github.com/hailinpan/tun-proxy/internal/status"
	"github.com/hailinpan/tun-proxy/internal/system"
	"golang.org/x/sys/unix"
)

type diagnosticCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type diagnosticConfig struct {
	Path         string `json:"path"`
	Loaded       bool   `json:"loaded"`
	Digest       string `json:"digest,omitempty"`
	Error        string `json:"error,omitempty"`
	Outbounds    int    `json:"outbounds,omitempty"`
	Rules        int    `json:"rules,omitempty"`
	DefaultRoute bool   `json:"default_route,omitempty"`
}

type diagnosticService struct {
	Available bool                  `json:"available"`
	Status    *launchservice.Status `json:"status,omitempty"`
	Error     string                `json:"error,omitempty"`
}

type diagnosticRuntime struct {
	State    *system.State           `json:"state,omitempty"`
	Alive    bool                    `json:"alive"`
	Snapshot *runtimestatus.Snapshot `json:"snapshot,omitempty"`
	Error    string                  `json:"error,omitempty"`
}

type diagnosticInterface struct {
	Name      string   `json:"name"`
	Index     int      `json:"index,omitempty"`
	Up        bool     `json:"up"`
	Running   bool     `json:"running"`
	Addresses []string `json:"addresses,omitempty"`
	Required  bool     `json:"required"`
	Error     string   `json:"error,omitempty"`
}

type hostsConflict struct {
	Line    int    `json:"line"`
	Address string `json:"address"`
	Domain  string `json:"domain"`
}

type diagnosisReport struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Overall     string                `json:"overall"`
	Config      diagnosticConfig      `json:"config"`
	Service     diagnosticService     `json:"service"`
	Runtime     diagnosticRuntime     `json:"runtime"`
	Interfaces  []diagnosticInterface `json:"interfaces,omitempty"`
	HostsPath   string                `json:"hosts_path"`
	Hosts       []hostsConflict       `json:"hosts_conflicts,omitempty"`
	Checks      []diagnosticCheck     `json:"checks"`
}

type diagnosisOptions struct {
	ConfigPath string
	StatePath  string
	HostsPath  string
}

func diagnoseCommand(args []string) error {
	flags := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fprintUsage(flags.Output(), []string{"diagnose"}) }
	configPath := flags.String("config", defaultUserConfigPath(), "configuration to inspect")
	statePath := flags.String("state", launchservice.DefaultLayout().State, "runtime recovery state")
	hostsPath := flags.String("hosts", "/etc/hosts", "hosts file to scan")
	jsonOutput := flags.Bool("json", false, "print complete report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("diagnose received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	report := collectDiagnosis(context.Background(), diagnosisOptions{
		ConfigPath: *configPath, StatePath: *statePath, HostsPath: *hostsPath,
	})
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	printDiagnosis(report)
	return nil
}

func collectDiagnosis(ctx context.Context, options diagnosisOptions) diagnosisReport {
	report := diagnosisReport{
		GeneratedAt: time.Now().UTC(), Overall: "ok", HostsPath: options.HostsPath,
		Config: diagnosticConfig{Path: options.ConfigPath},
	}
	add := func(name, status, message string) {
		report.Checks = append(report.Checks, diagnosticCheck{Name: name, Status: status, Message: message})
		if status == "error" {
			report.Overall = "error"
		} else if status == "warning" && report.Overall == "ok" {
			report.Overall = "warning"
		}
	}

	runtimeConfig, digest, configErr := config.LoadFileWithDigest(options.ConfigPath)
	if configErr != nil {
		report.Config.Error = configErr.Error()
		add("config", "error", configErr.Error())
	} else {
		report.Config.Loaded = true
		report.Config.Digest = digest
		report.Config.Outbounds = len(runtimeConfig.Outbounds)
		report.Config.Rules = len(runtimeConfig.Rules)
		report.Config.DefaultRoute = runtimeConfig.Capture.DefaultRoute
		add("config", "pass", fmt.Sprintf("loaded %d outbounds and %d rules", len(runtimeConfig.Outbounds), len(runtimeConfig.Rules)))
	}

	if os.Geteuid() != 0 {
		report.Service.Error = "root privileges are required for managed service inspection"
		add("service", "warning", "managed launchd status unavailable without sudo")
	} else {
		status, err := newServiceManager().Status(ctx)
		if err != nil {
			report.Service.Error = err.Error()
			add("service", "error", err.Error())
		} else {
			report.Service.Available = true
			report.Service.Status = &status
			if status.Installed && status.Loaded && status.Runtime.Running && status.Runtime.Phase == "running" {
				add("service", "pass", fmt.Sprintf("managed service running with supervisor PID %d", status.Runtime.PID))
			} else {
				add("service", "warning", fmt.Sprintf("installed=%t loaded=%t running=%t phase=%s", status.Installed, status.Loaded, status.Runtime.Running, status.Runtime.Phase))
			}
		}
	}

	state, stateErr := system.ReadState(options.StatePath)
	if stateErr != nil {
		report.Runtime.Error = stateErr.Error()
		if errors.Is(stateErr, os.ErrNotExist) {
			add("runtime_state", "warning", "runtime state file does not exist")
		} else {
			add("runtime_state", "warning", stateErr.Error())
		}
	} else {
		report.Runtime.State = &state
		processErr := unix.Kill(state.PID, 0)
		report.Runtime.Alive = processErr == nil || errors.Is(processErr, unix.EPERM)
		if report.Runtime.Alive {
			add("runtime_process", "pass", fmt.Sprintf("PID %d is alive in phase %s", state.PID, state.Phase))
		} else {
			add("runtime_process", "error", fmt.Sprintf("PID %d is not alive", state.PID))
		}
		if state.StatusSocket == "" {
			add("runtime_status", "warning", "state has no runtime status socket")
		} else if snapshot, err := runtimestatus.Query(ctx, state.StatusSocket); err != nil {
			report.Runtime.Error = joinMessage(report.Runtime.Error, err.Error())
			add("runtime_status", "warning", err.Error())
		} else {
			report.Runtime.Snapshot = &snapshot
			addRuntimeChecks(add, runtimeConfig, digest, state, snapshot)
		}
	}

	interfaces, interfaceErr := interfaceinfo.List()
	if interfaceErr != nil {
		add("interfaces", "error", interfaceErr.Error())
	} else {
		required := requiredInterfaces(runtimeConfig)
		byName := make(map[string]interfaceinfo.Interface, len(interfaces))
		for _, iface := range interfaces {
			byName[iface.Name] = iface
		}
		for _, name := range sortedSet(required) {
			iface, ok := byName[name]
			if !ok {
				report.Interfaces = append(report.Interfaces, diagnosticInterface{Name: name, Required: true, Error: "interface does not exist"})
				add("interface."+name, "error", "configured direct interface does not exist")
				continue
			}
			item := diagnosticInterface{Name: iface.Name, Index: iface.Index, Up: iface.Up(), Running: iface.Running(), Addresses: iface.Addresses, Required: true}
			report.Interfaces = append(report.Interfaces, item)
			if !iface.Up() || !hasUsableIPv4(iface.Addresses) {
				add("interface."+name, "error", fmt.Sprintf("up=%t usable_ipv4=%t", iface.Up(), hasUsableIPv4(iface.Addresses)))
			} else {
				add("interface."+name, "pass", "configured direct interface is up with IPv4")
			}
		}
	}

	if runtimeConfig != nil {
		conflicts, err := scanHostsConflicts(options.HostsPath, runtimeConfig.Rules)
		if err != nil {
			add("hosts", "warning", err.Error())
		} else {
			report.Hosts = conflicts
			if len(conflicts) == 0 {
				add("hosts", "pass", "no policy domains are overridden by the hosts file")
			} else {
				add("hosts", "error", fmt.Sprintf("%d hosts entries bypass Fake DNS policy", len(conflicts)))
			}
		}
	}

	sort.Slice(report.Interfaces, func(i, j int) bool { return report.Interfaces[i].Name < report.Interfaces[j].Name })
	return report
}

func addRuntimeChecks(add func(string, string, string), runtimeConfig *config.Config, digest string, state system.State, snapshot runtimestatus.Snapshot) {
	if snapshot.ConfigDigest == state.ConfigDigest && (digest == "" || snapshot.ConfigDigest == digest) {
		add("config_digest", "pass", "configuration digests agree")
	} else {
		add("config_digest", "warning", fmt.Sprintf("file=%s state=%s runtime=%s", digest, state.ConfigDigest, snapshot.ConfigDigest))
	}
	if snapshot.Reload.LastError != "" {
		add("reload", "warning", snapshot.Reload.LastError)
	} else {
		add("reload", "pass", fmt.Sprintf("successes=%d failures=%d", snapshot.Reload.Successes, snapshot.Reload.Failures))
	}
	if snapshot.Network.LastError != "" {
		add("network_refresh", "warning", snapshot.Network.LastError)
	}
	if snapshot.DNS.Failures != 0 || snapshot.DNS.CapacityRejects != 0 {
		add("dns", "warning", fmt.Sprintf("queries=%d failures=%d capacity_rejects=%d", snapshot.DNS.Queries, snapshot.DNS.Failures, snapshot.DNS.CapacityRejects))
	} else {
		add("dns", "pass", fmt.Sprintf("queries=%d failures=0 capacity_rejects=0", snapshot.DNS.Queries))
	}
	if snapshot.IPv6.Configured && !snapshot.IPv6.Enabled {
		add("ipv6", "warning", snapshot.IPv6.FallbackReason)
	}
	if runtimeConfig != nil && snapshot.Limits.TCPFlows != runtimeConfig.Sessions.MaxTCPFlows {
		add("runtime_limits", "warning", "runtime TCP flow limit differs from loaded configuration")
	}
}

func requiredInterfaces(runtime *config.Config) map[string]struct{} {
	result := make(map[string]struct{})
	if runtime == nil {
		return result
	}
	for _, route := range runtime.Outbounds {
		if route.Type == "direct" && route.Interface != "" {
			result[route.Interface] = struct{}{}
		}
	}
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func hasUsableIPv4(addresses []string) bool {
	for _, raw := range addresses {
		address, _, err := net.ParseCIDR(raw)
		if err == nil && address.To4() != nil && !address.IsLoopback() {
			return true
		}
	}
	return false
}

func scanHostsConflicts(path string, configRules []config.Rule) ([]hostsConflict, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open hosts file %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect hosts file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("hosts file %q must be a regular file no larger than 1 MiB", path)
	}
	exact := make(map[string]struct{})
	var suffixes []string
	for _, rule := range configRules {
		for _, domain := range rule.Domains {
			exact[strings.ToLower(domain)] = struct{}{}
		}
		for _, suffix := range rule.DomainSuffixes {
			suffixes = append(suffixes, strings.ToLower(suffix))
		}
	}
	var result []hostsConflict
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if comment := strings.IndexByte(text, '#'); comment >= 0 {
			text = text[:comment]
		}
		fields := strings.Fields(text)
		if len(fields) < 2 {
			continue
		}
		address, err := netip.ParseAddr(fields[0])
		if err != nil || address.Is4In6() {
			continue
		}
		for _, rawDomain := range fields[1:] {
			domain := strings.TrimSuffix(strings.ToLower(rawDomain), ".")
			if policyDomainMatch(domain, exact, suffixes) {
				result = append(result, hostsConflict{Line: line, Address: address.String(), Domain: domain})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read hosts file %q: %w", path, err)
	}
	return result, nil
}

func policyDomainMatch(domain string, exact map[string]struct{}, suffixes []string) bool {
	if _, ok := exact[domain]; ok {
		return true
	}
	for _, suffix := range suffixes {
		if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
			return true
		}
	}
	return false
}

func joinMessage(left, right string) string {
	if left == "" {
		return right
	}
	return left + "; " + right
}

func printDiagnosis(report diagnosisReport) {
	fmt.Printf("diagnosis overall=%s generated=%s\n", report.Overall, report.GeneratedAt.Format(time.RFC3339))
	for _, check := range report.Checks {
		fmt.Printf("%-7s %-24s %s\n", strings.ToUpper(check.Status), check.Name, check.Message)
	}
	if len(report.Hosts) != 0 {
		fmt.Println("hosts conflicts:")
		for _, conflict := range report.Hosts {
			fmt.Printf("  %s:%d %s %s\n", report.HostsPath, conflict.Line, conflict.Address, conflict.Domain)
		}
	}
}
