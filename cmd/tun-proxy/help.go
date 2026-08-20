package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

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

options:
  -generate     write the embedded default configuration
  -finder       reveal the selected configuration file in Finder
  -config PATH  configuration to generate or reveal
                (default: ~/.config/tun-proxy/config.yaml)
  -force        overwrite an existing file when used with -generate

commands:
  validate [-config PATH] [-service] [-json]

Generation validates the embedded template and refuses to overwrite an
existing file unless -force is explicit. Finder reveal is read-only. Use
-config to select a specific YAML file.
`,
	"config validate": `usage: tun-proxy config validate [options]

options:
  -config PATH  YAML configuration (default: ~/.config/tun-proxy/config.yaml)
  -service      also enforce the fixed managed-service path contract
  -json         print a machine-readable validation result

This command parses and semantically validates YAML. It does not modify the
host and does not require root. Use "tun-proxy check" for live host preflight.
`,
	"check": `usage: tun-proxy check [options]

options:
  -config PATH  YAML configuration (default: ~/.config/tun-proxy/config.yaml)
  -service      validate installed split-privilege storage and prerequisites

The live preflight checks root privileges, interfaces, routes, paths, and DNS
listener availability. It does not start the proxy.
`,
	"explain": `usage: tun-proxy explain [options]

options:
  -config PATH       YAML configuration (default: ~/.config/tun-proxy/config.yaml)
  -domain NAME       destination domain
  -ip ADDRESS        resolved/literal address; repeat for multiple answers
  -protocol tcp|udp  flow protocol (default: tcp)
  -port PORT         destination port (default: 443)
  -family ipv4|ipv6  address family used by -resolve (default: ipv4)
  -resolve           query the configured interface-bound DNS upstreams
  -timeout DURATION  DNS resolution timeout (default: 10s)
  -json              print machine-readable output

Without -ip or -resolve, explain is offline. CIDR rules may remain deferred
until a real address is supplied.
`,
	"diagnose": `usage: tun-proxy diagnose [options]

options:
  -config PATH  configuration to inspect
                (default: ~/.config/tun-proxy/config.yaml)
  -state PATH   runtime recovery state
  -hosts PATH   hosts file to scan (default: /etc/hosts)
  -json         print the complete report as JSON

Diagnose is read-only. Run it with sudo to inspect a root-owned managed state
file and status socket; without sudo it still reports available checks.
`,
	"run": `usage: tun-proxy run [options]

options:
  -config PATH  YAML configuration (default: ~/.config/tun-proxy/config.yaml)
`,
	"status": `usage: tun-proxy status [options]

options:
  -state PATH  runtime state (default: /var/run/tun-proxy/state.json)
  -json        print the complete runtime snapshot as JSON
`,
	"cleanup": `usage: tun-proxy cleanup [options]

options:
  -state PATH        runtime state (default: /var/run/tun-proxy/state.json)
  -lock PATH         fallback stale lock path
  -timeout DURATION  maximum cleanup duration (default: 30s)
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

options:
  -config PATH  configuration to install
                (default: ~/.config/tun-proxy/config.yaml)
  -binary PATH  binary to install (default: current executable)
  -start BOOL   start after installation (default: true)
`,
	"service start":   "usage: tun-proxy service start\n",
	"service stop":    "usage: tun-proxy service stop\n",
	"service restart": "usage: tun-proxy service restart\n",
	"service reload": `usage: tun-proxy service reload [options]

options:
  -timeout DURATION  wait for runtime confirmation (default: 15s)

The installed configuration is re-read. Immutable changes are rejected and
the current runtime remains active.
`,
	"service status": `usage: tun-proxy service status [options]

options:
  -json  print machine-readable output
`,
	"service logs": `usage: tun-proxy service logs [options]

options:
  -lines N                  number of trailing lines (default: 100)
  -n N                      alias for -lines
  -follow, -f               continue following appended data
  -stream stdout|stderr|both  select managed log stream (default: both)
`,
	"service upgrade": `usage: tun-proxy service upgrade [options]

options:
  -binary PATH  replacement binary (default: current executable)
  -config PATH  optional replacement configuration
`,
	"service uninstall": `usage: tun-proxy service uninstall [options]

options:
  -purge  also remove installed config, mappings, and logs
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
	text, ok := commandUsages[strings.Join(topic, " ")]
	if !ok {
		text = commandUsages[""]
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
