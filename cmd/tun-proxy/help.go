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
`,
	"cleanup": `usage: tun-proxy cleanup [options]

{{generated-options}}
`,
	"service": `usage: tun-proxy service <command> [options]

commands:
  install [options]    install the managed LaunchDaemon
  start                start the installed service
  stop                 stop the service cleanly
  restart              stop and start the service
  reload [options]     atomically reload mutable configuration
  status [-json]       show launchd and runtime status
  logs [options]       read or follow managed stdout/stderr logs
  upgrade [options]    transactionally replace binary/configuration
  uninstall [options]  remove the service

Run "tun-proxy help service <command>" for details.
`,
	"service install": `usage: tun-proxy service install [options]

{{generated-options}}
`,
	"service start":   "usage: tun-proxy service start\n",
	"service stop":    "usage: tun-proxy service stop\n",
	"service restart": "usage: tun-proxy service restart\n",
	"service reload": `usage: tun-proxy service reload [options]

{{generated-options}}

The installed configuration is re-read. Immutable changes are rejected and
the current runtime remains active.
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
