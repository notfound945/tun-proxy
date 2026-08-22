# Phase 9 CLI operations and diagnostics

Phase 9 makes the command line the complete and only day-to-day operating
surface for configuration, inspection, diagnosis, and managed-service control.
The project does not plan a graphical management interface. The deferred Phase
7 24-hour soak remains a separate release gate.

## Command surface

### Help

```sh
tun-proxy help
tun-proxy help explain
tun-proxy help diagnose
tun-proxy help config validate
tun-proxy help service
tun-proxy help service reload
tun-proxy help service logs
```

Help is hierarchical and keeps the private `_service-run` and
`_service-worker` entry points hidden.

### Offline configuration validation

Generate the embedded default configuration without requiring a repository
checkout:

```sh
tun-proxy config -generate
tun-proxy config -generate -force
```

Generation defaults to `~/.config/tun-proxy/config.yaml`, validates the
embedded template before writing, creates the private directory and file with
`0700`/`0600` permissions, and refuses to replace an existing file unless
`-force` is explicit.

Reveal the configuration in Finder without modifying it:

```sh
tun-proxy config -finder
tun-proxy config -finder -config "$HOME/.config/tun-proxy/config.yaml"
```

The default is `~/.config/tun-proxy/config.yaml`. When invoked through `sudo`,
the CLI prefers the `SUDO_USER` home directory. Finder reveal uses the absolute
file path and rejects missing or non-regular paths.

Validate configuration offline:

```sh
tun-proxy config validate
tun-proxy config validate -json
tun-proxy config validate -service
```

Validation parses strict YAML, compiles semantic configuration, and reports
the exact source digest without changing the host or requiring root. The
`-service` option additionally enforces the fixed managed state, lock, and
mapping paths. Live interface, storage ownership, route, and listener checks
remain the responsibility of `check` / `check -service`.

### Flow explanation

```sh
tun-proxy explain \
  -domain api.cursor.sh

tun-proxy explain \
  -domain example.com \
  -ip 203.0.113.9 \
  -json
```

The default path is offline. It reports the domain-stage rule, any pending
post-resolution CIDR decision, final outbound, interface, DNS upstreams,
fallback, and reject outcome. `-ip` is repeatable for deterministic simulation.
`-resolve` is explicit and uses the same interface-bound DNS and recoverable
fallback policy as production rather than the system resolver.

### Read-only diagnosis

```sh
tun-proxy diagnose
sudo tun-proxy diagnose -json
```

The report retains partial results when a root-owned artifact cannot be read.
It checks configuration and digest agreement, launchd/runtime state, reload and
network errors, DNS counters, IPv6 runtime gating, required direct interfaces,
and `/etc/hosts` entries that would bypass Fake DNS policy. Domain suffixes use
DNS label boundaries: `cursor.sh` and `api.cursor.sh` match `cursor.sh`, while
`not-cursor.sh` does not.

### Cleanup recovery

```sh
sudo tun-proxy cleanup
sudo tun-proxy cleanup -clear-dns -config ~/.config/tun-proxy/config.yaml
sudo tun-proxy cleanup -clear-fake-ip -config ~/.config/tun-proxy/config.yaml
sudo tun-proxy cleanup -clear-dns -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
```

Plain cleanup restores DNS and routes from the managed state record while
preserving ownership checks. `-clear-dns` is a conservative recovery path for
a missing state record: it resets an enabled macOS network service to automatic
DNS only when the service's complete current DNS list is the single configured
`dns.listen` address. It includes enabled services that are currently inactive,
but does not overwrite custom, mixed, or externally changed DNS lists.

`-clear-fake-ip` removes the configured IPv4/IPv6 snapshots and journals. Both
clear flags first run recorded-state recovery and share one instance-lock guard,
so either operation is refused while another instance may be starting or
running.

### Managed-service lifecycle and logs

```sh
sudo tun-proxy service stop
sudo tun-proxy service restart
sudo tun-proxy service reload -timeout 15s
sudo tun-proxy service logs -lines 200
sudo tun-proxy service logs -stream stderr -follow
```

Stop performs a clean shutdown and waits for runtime state to be removed while
leaving the installed launchd job available for a later explicit start.
Restart performs that clean stop followed by the existing readiness-checked
start. Reload signals the launchd job with `SIGHUP`, then waits for the worker
status socket to increment either the success or failure counter; immutable
changes return the runtime rejection without replacing the active generation.

Logs are limited to the fixed managed stdout/stderr paths. The reader rejects
symlinks and non-regular files, bounds the tail window to 8 MiB, limits the
requested line count to 10,000, and handles truncation or replacement while
following.

## Acceptance status

As of 2026-08-22, command routing, help topics, configuration validation,
domain-suffix boundaries, offline/pending and resolved explain decisions,
partial diagnosis, service manager restart/reload signaling, reload success and
failure confirmation, cleanup recovery safety, log tail edge cases, and symlink
rejection are covered by automated tests.

The remaining root acceptance is intentionally deferred to a maintenance
window so the currently running managed service is not disturbed:

1. build and install the updated binary transactionally;
2. verify `service logs` against both managed streams;
3. apply a mutable configuration change and confirm `service reload` increments
   the success counter without replacing the worker PID;
4. inject an immutable change and confirm reload rejection preserves the active
   configuration;
5. run `service restart` and verify clean PID replacement, readiness, Fake DNS,
   forwarding, and restored runtime metrics.

The strict 24-hour soak remains pending and is not part of this CLI slice.
