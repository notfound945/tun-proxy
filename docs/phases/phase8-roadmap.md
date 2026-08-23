# Phase 8 expansion roadmap

Phase 8 expands the validated IPv4 Fake IP proxy without weakening its current
rollback and per-interface routing guarantees. The deferred Phase 7 24-hour
soak remains a release gate; it is not treated as passed merely because Phase 8
development has started.

## Delivery slices

### 8.1 Dual-stack outbound foundation

- Accept explicit IPv4 or IPv6 DNS upstream endpoints.
- Select `udp4`/`tcp4` or `udp6`/`tcp6` from each upstream address.
- Add outbound TCP and connected UDP dialing for IPv6 destinations using
  `IPV6_BOUND_IF`.
- Add isolated A and AAAA resolver caches and typed no-address errors.
- Preserve all existing IPv4 configuration and runtime behavior.

Acceptance: configuration, resolver UDP-to-TCP fallback, A/AAAA cache isolation,
IPv6 interface binding, invalid-family handling, race tests, vet, and build all
pass. This slice does not yet expose Fake AAAA answers or route IPv6 into utun.

### 8.2 Fake IPv6 identity

- Add an explicit Fake IPv6 prefix and bounded address pool.
- Persist IPv4 and IPv6 mappings without allowing cross-family reuse.
- Prepare stable Fake AAAA mappings, but keep externally served AAAA responses
  at NODATA until the IPv6 route and netstack path in 8.3 are live.
- Extend pool statistics, capacity failures, TTL protection, and corruption
  recovery to both families.

Acceptance: concurrent allocation, reverse lookup, reference protection,
exhaustion, reclamation, restart stability, and A/AAAA separation tests pass;
normal DNS clients still receive NODATA for AAAA during this slice.

### 8.3 IPv6 utun data path

- Configure the utun IPv6 point-to-point address and install a recorded Fake
  IPv6 route transactionally.
- Enable IPv6 in the gVisor adapter and inject packets according to their actual
  IP version.
- Carry Fake IPv6 TCP and UDP sessions through the family-matched resolver and
  bound outbound socket.
- Enable Fake AAAA answers only after the IPv6 route and packet pumps are live,
  and restore NODATA behavior on any startup rollback.
- Extend status and cleanup accounting to both route families.

Acceptance: in-memory IPv6 TCP/UDP tests, route rollback tests, root Fake AAAA
DNS/TCP/UDP smoke tests, interface capture, and clean shutdown all pass.

### 8.4 Default-route and literal-IP capture

- Design explicit host-route bypasses for every physical gateway, outbound DNS
  server, and control endpoint before changing a default route.
- Capture direct IPv4/IPv6 destinations without requiring a Fake DNS mapping.
- Refuse startup if a loop-free route transaction cannot be proven and fully
  recorded for cleanup.

Acceptance: literal IP traffic follows policy, proxy egress never re-enters
utun, both default routes restore exactly, and crash cleanup is conditional and
idempotent.

### 8.5 Post-resolution IP/CIDR rules

- Add validated IPv4/IPv6 CIDR predicates.
- Perform the documented two-stage decision: domain pre-match, candidate
  outbound resolution, IP/CIDR post-match, then re-resolve if the outbound
  changes.
- Keep one immutable decision for the lifetime of each flow.

Acceptance: precedence, multiple answers, outbound changes, fallback, direct
IP metadata, and reload generation tests pass.

### 8.6 Process attribution spike

- Establish whether macOS exposes reliable per-flow process identity to this
  non-NetworkExtension architecture.
- Do not ship process rules unless attribution is deterministic under port
  reuse, process exit, and concurrent connects.

The spike may conclude that process-level routing requires a privileged helper
or is unsupported by the chosen architecture. That conclusion must be recorded
before configuration fields are exposed.

Status: **complete**. The `libproc` probe uniquely attributed concurrent TCP
connections, but reproduced both loss of identity after process exit and
multiple valid owners for cross-process unconnected UDP `SO_REUSEPORT`.
Polling is therefore diagnostic-only and process rule fields are unsupported
by this architecture. A helper that performs the same polling does not fix the
lifetime race. See
[`phase8-process-attribution.md`](phase8-process-attribution.md).

### 8.7 launchd service and least-privilege helper

This slice is split so service lifecycle work is not confused with a completed
least-privilege design:

- **8.7a transactional LaunchDaemon:** define install, start, stop, status,
  upgrade, crash recovery, and uninstall transactions; use fixed privileged
  paths; require root for lifecycle control; and preserve the existing recovery
  log. The full daemon still runs as root.
- **8.7b least-privilege separation:** move only required utun, route, DNS, and
  socket-binding operations into a narrow authenticated privileged boundary;
  run policy and relay logic without root where practical.

Status: **complete. 8.7a passed transactional root lifecycle acceptance. 8.7b
now runs the production service as a root supervisor plus dedicated non-root
`_tun-proxy` worker and has passed strict lifecycle, ownership, preflight,
upgrade, DNS, and interface-routing acceptance.** See
[`phase8-launchd-service.md`](phase8-launchd-service.md) and
[`phase8-least-privilege.md`](phase8-least-privilege.md).

## Execution order

Slices are delivered and committed independently in the order above. Root
smoke tests occur only after the corresponding automated and rollback tests
pass. The first implementation target is 8.1 because it creates the family-safe
resolver and socket primitives required by every later IPv6 data-path change.

## Recorded 8.1 result

On 2026-08-18, the dual-stack outbound foundation was completed without
enabling Fake AAAA responses or changing the IPv4 TUN path. Strict configuration
now accepts bracketed IPv6 DNS endpoints while rejecting loopback, unspecified,
IPv4-mapped IPv6, and zero-port upstreams. The resolver selects UDP/TCP socket
families per upstream, preserves truncation fallback over IPv6, and stores A and
AAAA answers under separate bounded cache keys. Typed no-IPv6-answer failures
are terminal policy results just like no-IPv4-answer failures.

Direct TCP validates that the requested network matches the destination family;
connected UDP derives its socket family from the destination. Both paths reuse
the existing per-socket interface control, which applies `IPV6_BOUND_IF` for
IPv6. Tests exercised real TCP and UDP connections bound to macOS `lo0`, an
IPv6 DNS UDP-to-TCP fallback, cache isolation, unsafe upstream rejection,
family mismatch rejection, and recoverable missing-interface errors. Full
`go test -race ./...`, `go vet ./...`, and `go build ./...` passed. Slice 8.2,
Fake IPv6 identity and persistence, is next.

## Recorded 8.2 result

On 2026-08-18, the Fake IP pool was made address-family safe without changing
the version-1 snapshot schema. Existing IPv4 snapshots remain readable and are
never rewritten into a combined format. An optional `fake_ipv6` configuration
block accepts only explicit unique-local IPv6 prefixes, enforces the reserved
address and mapping limits, requires a persistence path distinct from IPv4,
and is immutable across reloads. IPv6 snapshots use their own atomic `0600`
file and the same corruption quarantine and restart-protection behavior.

Allocation uses standard-library `netip.Addr` values for both families. It
examines at most one more candidate than the number of occupied mappings, so a
large IPv6 prefix cannot cause a prefix-sized scan; capacity reporting
saturates safely when the usable space exceeds `uint64`. Tests cover stable
IPv6 allocation, concurrency inherited from the shared pool, reservations,
exhaustion, large prefixes, reverse lookup, cross-family snapshot rejection,
restart stability, strict configuration, immutable reloads, and status
round-tripping. Full `go test -race ./...`, `go vet ./...`, and
`go build ./...` passed.

When configured, startup now validates the IPv6 prefix route, restores the
separate pool, flushes it during normal shutdown, and exposes its limit and
statistics as `fake_ipv6`; the concise status output explicitly reports
`aaaa_enabled=false`. Fake DNS continues returning AAAA NODATA. Slice 8.3 will
add the transactional IPv6 utun route and gVisor data path before enabling
Fake AAAA answers.

## Recorded 8.3 data-plane foundation

The first 8.3 implementation increment enables IPv4 and IPv6 together inside
the bounded TUN pump and gVisor adapter. Inbound and outbound IPv6 packets are
validated using the fixed header and payload length, injected with the IPv6
protocol number, and emitted without being mistaken for IPv4. TCP and UDP flow
metadata now preserves either standard-library address family. In-memory gVisor
tests complete IPv6 TCP and UDP echo flows and verify source, destination, and
port metadata; TUN tests cover IPv4/IPv6 input, output, malformed packets, and
the Darwin packet offset.

This increment does not yet configure an IPv6 address on utun, install a host
route, select the IPv6 Fake IP pool in sessions, or enable Fake AAAA. Those
host-visible steps remain in progress and must pass rollback tests before root
acceptance begins.

## Recorded 8.3 host-path implementation

The host-visible 8.3 path now requires `tun.ipv6_address` and `tun.ipv6_peer`
as a pair whenever `fake_ipv6` is enabled. Startup configures IPv4 and then
IPv6 on the same utun, records the legacy IPv4 route plus an additional IPv6
route before mutation, installs and verifies both routes, and starts Fake DNS
only after both packet pumps are live. Shutdown and crash cleanup remove
additional routes in reverse order, persist progress after every deletion, and
retain the recovery file if an earlier rollback step fails. Version-1 state
files containing only the original `route` field remain compatible.

TCP and UDP sessions select their mapping pool and A/AAAA resolver from the
intercepted Fake IP family. TCP uses `tcp4` or `tcp6` explicitly; UDP uses the
family-aware connected packet dialer. A no-address DNS result is a terminal
business result for either family and does not trigger an interface fallback.
Fake DNS preserves AAAA NODATA when IPv6 is omitted and returns stable Fake
AAAA only when the complete IPv6 configuration is present. Status reports
separate Fake IPv4 and Fake IPv6 answer counts.

Automated configuration, utun command, IPv4/IPv6 route command, reverse-order
rollback, pool selection, A/AAAA behavior, IPv6 TCP/UDP session, race, vet, and
build checks pass before root testing. Root route/DNS smoke acceptance remains
pending. The development host currently has only link-local IPv6 on `en0` and
`en7` and no public IPv6 default route, so public `curl -6` success requires a
different IPv6-capable network; this implementation intentionally does not add
NAT64.

The subsequent root preflight exposed two macOS-specific edge cases. IPv6
`route get` requires an explicit `-inet6`, and an empty IPv6 routing table can
print `not in table` while still exiting successfully. Both forms are now
handled without weakening conflict detection or idempotent cleanup. Startup
also gates the host IPv6 path on a configured outbound having a usable
non-link-local IPv6 address and owning the IPv6 default route. If that gate is
not met, the process logs and reports the reason, leaves utun and routing in
IPv4-only mode, and keeps AAAA at NODATA. A restart re-evaluates capability.

The no-native-IPv6 fallback was accepted on the development host on
2026-08-18. Configuration remained dual-stack-prepared, status exposed the
expected disabled state and reason, A/IPv4 HTTPS continued successfully, AAAA
remained NODATA, all runtime error counters stayed clear, and shutdown removed
utun, DNS listeners, state, socket, and lock while preserving both root-only
mapping files. Native IPv6 root forwarding acceptance remains environment-
gated and is not marked complete by this fallback result.

## Recorded 8.4 implementation

The default-route and literal-IP capture path is implemented as an explicit
`capture.default_route` opt-in and remains disabled in shipped configurations.
It preserves the system defaults and installs recorded IPv4 `/1` split routes,
plus IPv6 `/1` routes only when the Phase 8.3 capability gate is enabled.
Before those routes are added, scoped route lookups must prove a unique
physical interface and same-family gateway for every direct outbound DNS
endpoint. macOS system-owned direct gateway host routes are reused but never
recorded or deleted; other gateway and DNS bypasses are persisted before
mutation. Routed pre-existing host entries and cross-interface address
ambiguity are rejected rather than assumed or overwritten.

Captured literal TCP/UDP destinations skip DNS and can match CIDR or default
rules; domain predicates do not match domainless flows. Outbound
topology becomes reload-immutable while capture is active. Sleep or interface
changes re-prove the bypass plan and trigger a controlled reverse rollback if
the plan changed. Conditional cleanup verifies both interface and gateway.
Automated configuration, route command, persistence, plan rejection, literal
policy, and direct dialing tests pass. Root IPv4/IPv6 split routing, scoped
egress, literal TCP forwarding, default/DNS preservation, normal shutdown, and
crash cleanup have passed. Literal UDP, simultaneous utun/physical-interface
packet capture, and deliberate conflict injection also passed; see
[`phase8-default-route.md`](phase8-default-route.md).

Initial root preflight then exposed normal macOS cloned gateway and IPv6 probe
host routes plus the `destination: default`/non-default-mask rendering used for
IPv4 `/1`. Planning now reuses but never owns cloned on-link gateway entries,
IPv6 capability queries the default route directly, and route verification and
cleanup distinguish split routes by mask. Automated regressions cover these
Darwin forms. A later run proved that global utun `/1` routes outrank a scoped
physical default during bound-socket lookup, so each direct interface now gets
equal-length scoped physical `/1` routes before global capture is installed.
Their scope and gateway are persisted and conditionally removed after the
global routes during rollback. Crash testing then showed that macOS removes
utun routes with the dead process before cleanup, allowing lookup to fall
through to an equal-length physical scoped route. Cleanup now treats a route
whose recorded interface no longer exists as kernel-removed and never deletes
the replacement. The retained recovery state then completed cleanup
successfully: both defaults and DNS were restored and no route, utun, state,
socket, lock, or listener remained.

The final strict root checks sent literal UDP/53 through utun and observed its
interface-bound relay on en0 with successful response and zero failures,
fallbacks, or capture drops. A disposable exact gateway host route was rejected
by both `check` and `run` before mutation, then removed without residue. Phase
8.4 automated and strict root acceptance are complete.

## Recorded 8.5 implementation

Rules accept canonical IPv4 and IPv6 `ip_cidr` predicates, including
combinations with domain and suffix constraints.
Pure CIDR rules require `capture.default_route: true` to observe ordinary
real-IP or literal-IP traffic. Combined domain/suffix plus CIDR rules do not:
their explicit domain predicate causes Fake-IP capture before CIDR evaluation.
Candidate matching defers CIDR evaluation, resolves through the candidate
outbound's isolated resolver, and then repeats the ordered match against the
real address set. Any answer may satisfy a CIDR, but YAML rule order remains
authoritative. When the final outbound changes, the domain is resolved once
more through that outbound and the immutable decision is not re-matched.

Resolver and dial fallback remain operational behavior rather than a policy
rewrite. Literal destinations match CIDRs without DNS. If the conclusive
pre-resolution rule is reject, an earlier matching direct CIDR candidate may
provide resolution; when every possible winner is reject, the flow terminates
without network I/O. Reload constructs a new immutable engine/routes
generation, while active TCP flows and UDP sessions retain their acquired
generation until completion.

Automated coverage includes canonical validation, IPv4/IPv6 matching,
precedence, combined predicates, multiple answers, outbound re-resolution,
resolver fallback, reject candidates, literal metadata, TCP/UDP prepared
addresses, and reload generation lifetime.

The IPv4 root interface-routing smoke passed on 2026-08-19 on a dual-interface
Mac. Captures proved candidate DNS on `en0`, CIDR-selected re-resolution and
TCP dialing on `en7`, literal-IP matching without DNS, and A-to-B reload
generation isolation: the old transfer remained on `en7` through normal FIN
while the post-reload flow completed on `en0`. Normal shutdown restored DNS
and routes and removed utun, runtime state, the listener, and the process. The
host had no usable non-link-local physical IPv6 address, so native IPv6 root
forwarding remains an environment-gated acceptance item. See
[`phase8-cidr-rules.md`](phase8-cidr-rules.md) for the packet-level record.

## Recorded 8.6 process-attribution decision

The TUN flow metadata contains the original application's protocol and
client-side source/destination tuple before tun-proxy creates an outbound
socket, so the architecture can query the correct tuple. A Darwin-only
diagnostic scanner now enumerates process FDs through `libproc` and classifies
the tuple as no owner, one unique owner, or multiple owners. It remains fully
isolated from production configuration and flow decisions.

The controlled 2026-08-19 probe found the expected unique PID for each of six
concurrent TCP connections. It then proved the two disqualifying cases: after
one child exited, the former TCP tuple had no queryable owner; two different
processes using unconnected UDP `SO_REUSEPORT` on the same source endpoint both
matched the same observed UDP flow. Root privilege can widen visibility but
cannot restore historical identity or select one of multiple legitimate
owners. The strict root replay passed on the same date, reproducing `none` after
process exit and two distinct owners for the shared UDP endpoint. Phase 8.6 is
therefore fully accepted with process-level routing explicitly unsupported for
the current non-NetworkExtension architecture. Revisit only with flow-creation
identity, not another polling layer. Full rationale and the recorded probe are in
[`phase8-process-attribution.md`](phase8-process-attribution.md).

## Recorded 8.7a implementation result

The service lifecycle now installs a root-owned binary, managed configuration,
and direct-exec LaunchDaemon plist at fixed system paths. Installation,
replacement, and removal stage same-directory files and use atomic renames;
startup failures restore prior files and state. The plist starts a registered
job at load, restarts unsuccessful exits, and permits a clean SIGTERM stop to
remain stopped. The hidden service entry point performs stale recovery before
preflight and confines every reload to the fixed state, lock, and mapping
paths.

Upgrade is intentionally a graceful stop, launchd unload, atomic binary/config/
plist replacement, and prior-state restoration—not a hot reload. Default
uninstall preserves configuration, mappings, and logs and remains directly
reinstallable; purge deletes only exact known regular files and removes
directories only when empty. Lifecycle commands require root and no mutable
control socket was added in this historical slice; the later production control
plane uses the root-only v2 socket with external reload request IDs and cached
idempotent retries.
This 8.7a implementation still ran as root; Phase 8.7b subsequently replaced
it with the production split described below. Automated transaction, rollback,
ownership, mode, and CLI gates pass. Real launchd installation, fixed direct execution,
permissions, Fake DNS, and HTTPS forwarding passed on 2026-08-19. Clean
stop/start, including complete runtime-file removal and forwarding after
restart, also passed that day. Transactional binary replacement, PID rollover,
and forwarding after upgrade passed as well. Launchd restarted the process
after `SIGKILL` and restored forwarding. Default uninstall removed the service
and runtime artifacts while preserving configuration, refreshed Fake IP
snapshots, and logs. Phase 8.7a acceptance is complete; Phase 8.7b was gated
separately.

## Recorded 8.7b implementation result

The managed LaunchDaemon now keeps only the lifecycle supervisor as root. It
creates the utun and loopback DNS listeners, owns route/system-DNS mutation and
the recovery journal, then transfers the required descriptors over a private
parent/child channel to a worker running under the dedicated `_tun-proxy`
identity. The worker owns Fake IP persistence and runs Fake DNS, policy,
gVisor, resolver, TCP/UDP relay, reload, status, and metrics logic. Installation,
upgrade, rollback, preservation-safe uninstall, and purge transactionally
manage the account and worker-owned storage.

Managed preflight now validates the fixed split ownership model: state and
lock paths remain root-owned, while IPv4/IPv6 Fake IP persistence paths and
their parent directory must be owned by the resolved worker UID. The public
`check -service -config PATH` command exercises that installed-service layout.
The current supervisor also owns a mode-`0600`
`/var/run/tun-proxy/control.sock`, accepts only UID 0 peers, compares the CLI's
expected configuration digest, and returns the worker's final reload result.
SIGHUP remains available only as a manual compatibility entry point, and the
worker status counters are no longer used to associate a CLI reload request.
Service start also treats an observed running supervisor/worker runtime as
authoritative when `launchctl kickstart` itself times out or is killed after
the job has already become ready; a never-ready runtime still returns both the
transport and readiness failures.

Strict production acceptance passed on 2026-08-19. Transactional upgrade and
clean stop/start replaced PIDs while preserving the split relationship; the
observed supervisor ran as UID 0 and its worker as UID/GID 499 with the worker
parented directly to the supervisor. Managed service status and runtime status
reported the service loaded and running with the expected configuration
digest. IPv6 was correctly runtime-disabled because the configured outbound
interfaces had no usable non-link-local IPv6 address.

The installed configuration added an `en7` intranet outbound with three
interface-bound DNS upstreams and reject-on-failure behavior for
`code.266.com`, `*.oa.com`, `*.mtt.xyz`, and `*.ifere.com`. Packet capture
proved the selected DNS and TCP/TLS traffic on `en7` with no matching `en0`
leakage. Normal Fake DNS/application probes returned Fake IPv4 addresses for
`code.266.com`, `phl.oa.com`, `mtt.xyz`, and `ifere.com`; HTTP results were
302, 200, and 301 for the exercised endpoints. A final controlled replay made
20 DNS queries, received 20 Fake answers, added no capacity rejects, and left
the cumulative DNS failure counter unchanged at 3. Phase 8.7b is complete.
