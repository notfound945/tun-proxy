package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const flagOptionsMarker = "{{generated-options}}"

func commandFlagSet(topic string, output io.Writer) (*flag.FlagSet, bool) {
	switch topic {
	case "config":
		return newConfigFlagSet(output, nil), true
	case "config validate":
		return newConfigValidateFlagSet(output, nil), true
	case "check":
		return newCheckFlagSet(output, nil), true
	case "explain":
		return newExplainFlagSet(output, nil), true
	case "diagnose":
		return newDiagnoseFlagSet(output, nil), true
	case "run":
		return newRunFlagSet(output, nil), true
	case "status":
		return newStatusFlagSet(output, nil), true
	case "cleanup":
		return newCleanupFlagSet(output, nil), true
	case "service install":
		return newServiceInstallFlagSet(output, nil), true
	case "service reload":
		return newServiceReloadFlagSet(output, nil), true
	case "service status":
		return newServiceStatusFlagSet(output, nil), true
	case "service logs":
		return newServiceLogsFlagSet(output, nil), true
	case "service upgrade":
		return newServiceUpgradeFlagSet(output, nil), true
	case "service uninstall":
		return newServiceUninstallFlagSet(output, nil), true
	default:
		return nil, false
	}
}

func newCommandFlagSet(topic string, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(topic, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() { fprintUsage(flags.Output(), strings.Fields(topic)) }
	return flags
}

var commandUsages = map[string]string{
	"": `usage: tun-proxy <command> [options]
       tun-proxy -version

commands:
  interfaces                 list network interfaces
  config [options|command]   generate, reveal, or validate configuration
  check [options]             validate configuration and host prerequisites
  explain [options]           explain the rule and outbound for a flow
  diagnose [options]          collect a read-only health report
  run [options]               run in the foreground
  status [options]            inspect recovery state and runtime metrics
  cleanup [options]           safely restore recorded system state
  self-update                 download and install the latest GitHub Release
  service <command>           manage the LaunchDaemon
  version                     print build version (also: -version, --version)
  help [command]              show command-specific help

Run "tun-proxy help <command>" for details.
`,
	"interfaces": `usage: tun-proxy interfaces

List current interfaces, flags, MTU, and assigned addresses.
`,
	"config": `usage:
  tun-proxy config -generate [-config PATH] [-force]
  tun-proxy config -finder [-config PATH]
  tun-proxy config <command> [options]

{{generated-options}}

commands:
  validate [-config PATH] [-service] [-json]

Generation validates the embedded template and refuses to overwrite an
existing file unless -force is explicit. Finder reveal is read-only. Use
-config to select a specific YAML file.
`,
	"config validate": `usage: tun-proxy config validate [options]

{{generated-options}}

This command parses and semantically validates YAML. It does not modify the
host and does not require root. Use "tun-proxy check" for live host preflight.
`,
	"check": `usage: tun-proxy check [options]

{{generated-options}}

The live preflight checks root privileges, interfaces, routes, paths, and DNS
listener availability. It does not start the proxy.
`,
	"explain": `usage: tun-proxy explain [options]

{{generated-options}}

Without -ip or -resolve, explain is offline. CIDR rules may remain deferred
until a real address is supplied.
`,
	"diagnose": `usage: tun-proxy diagnose [options]

{{generated-options}}

Diagnose is read-only. Run it with sudo to inspect a root-owned managed state
file and status socket; without sudo it still reports available checks.
`,
	"run": `usage: tun-proxy run [options]

{{generated-options}}
`,
	"status": `usage: tun-proxy status [options]

{{generated-options}}

-fake-ip requests the live IPv4/IPv6 mapping list from the running process.
It requires a live status socket and can be combined with -json.
`,
	"cleanup": `usage: tun-proxy cleanup [options]

{{generated-options}}

-clear-dns and -clear-fake-ip load -config, restore recorded system state, and
share one instance-lock guard. DNS clearing only resets services whose complete
DNS list still equals the configured local listener; Fake IP clearing removes
configured IPv4/IPv6 snapshots and WALs.
`,
	"self-update": `usage: tun-proxy self-update

Downloads the release updater from:
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh
and executes it with /bin/bash. This is the same update flow documented for
"curl -fsSL .../scripts/update-release.sh | bash". Run it as a normal user,
not through sudo; the updater requests privilege only for files that require it.
`,
	"service": `usage: tun-proxy service <command> [options]

commands:
  install [options]     install the managed LaunchDaemon
  start                 start the installed service
  stop                  stop the service cleanly
  restart               stop and start the service
  sync-user-config      install user config; restart only if already running
  reload [options]      atomically reload mutable configuration
  status [-json]        show launchd and runtime status
  logs [options]        read or follow managed stdout/stderr logs
  upgrade [options]     transactionally replace binary/configuration
  uninstall [options]   remove the service

Run "tun-proxy help service <command>" for details.
`,
	"service install": `usage: tun-proxy service install [options]

{{generated-options}}
`,
	"service start": `usage: tun-proxy service start

Starts the installed service and waits up to 20 seconds for runtime readiness.
On failure, run "sudo tun-proxy service logs" to inspect managed stdout/stderr.
`,
	"service stop": `usage: tun-proxy service stop

Stops the installed service cleanly, disables its launchd label, and unloads
its job so KeepAlive cannot start it again. The installed files are preserved;
"sudo tun-proxy service start" re-enables and loads the job. On failure, run
"sudo tun-proxy service logs" to inspect managed stdout/stderr.
`,
	"service restart": "usage: tun-proxy service restart\n",
	"service sync-user-config": `usage: tun-proxy service sync-user-config

Validates the invoking user's ~/.config/tun-proxy/config.yaml and atomically
copies it to the managed service configuration. If the service is running, it
is restarted and checked for readiness; startup failure rolls the managed
configuration back and attempts to restore the previous service. If the
service is stopped, it remains stopped. On failure, inspect managed logs with:
  sudo tun-proxy service logs
`,
	"service reload": `usage: tun-proxy service reload [options]

{{generated-options}}

Without -config or -user-config, the running service re-reads the installed
configuration. -user-config selects the invoking user's
~/.config/tun-proxy/config.yaml; -config selects an explicit path. For a running
service, the source is transactionally applied with rollback when runtime
acknowledgement fails. Reload requires a running service; use
"sudo tun-proxy service sync-user-config" to synchronize the user configuration
while preserving stopped/running state. On failure, inspect managed logs with:
  sudo tun-proxy service logs
`,
	"service status": `usage: tun-proxy service status [options]

{{generated-options}}
`,
	"service logs": `usage: tun-proxy service logs [options]

{{generated-options}}

-clear truncates the selected stdout/stderr logs. Combine it with -follow to
clear existing contents and then wait for new log entries.
`,
	"service upgrade": `usage: tun-proxy service upgrade [options]

{{generated-options}}

If the service is ready before the upgrade, the replacement is started and
checked for readiness; failure rolls back the installed artifacts and attempts
to restore the previous service. If the service is stopped or not ready, the upgrade only
replaces installed artifacts and leaves the job stopped and unloaded without a
startup check. The launchd start-at-boot setting is preserved unless overridden.

If an upgrade operation fails, inspect the managed logs with:
  sudo tun-proxy service logs
`,
	"service uninstall": `usage: tun-proxy service uninstall [options]

{{generated-options}}
`,
	"version": "usage: tun-proxy version\n",
}

func helpCommand(args []string) error {
	for len(args) > 0 && args[0] == "help" {
		args = args[1:]
	}
	topic := strings.Join(args, " ")
	if _, ok := commandUsages[topic]; !ok {
		fprintUsage(os.Stderr, nil)
		return fmt.Errorf("unknown help topic %q", topic)
	}
	fprintUsage(os.Stdout, args)
	return nil
}

func fprintUsage(writer io.Writer, topic []string) {
	name := strings.Join(topic, " ")
	text, ok := commandUsages[name]
	if !ok {
		text = commandUsages[""]
	}
	var generated bytes.Buffer
	if flags, hasFlags := commandFlagSet(name, &generated); hasFlags {
		_, _ = io.WriteString(&generated, "options:\n")
		flags.PrintDefaults()
		text = strings.Replace(text, flagOptionsMarker, strings.TrimRight(generated.String(), "\n"), 1)
	}
	_, _ = io.WriteString(writer, text)
}

func usageError(topic []string, message string) error {
	fprintUsage(os.Stderr, topic)
	if message == "" {
		return errors.New("invalid command usage")
	}
	return errors.New(message)
}
