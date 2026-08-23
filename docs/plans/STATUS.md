# tun-proxy current status

Last updated: 2026-08-22

This document records the current implementation status, acceptance evidence,
and remaining release work. Detailed procedures and chronological evidence live
in the corresponding documents under `docs/phases/`.

## Phase status

Phases 0–3 have passed on macOS 15.7.7/arm64. The Phase 4–5 TCP MVP now has a
gVisor TCP stack, Fake IP flow lookup, deterministic rules, per-interface DNS
and TCP sockets, guarded fallback/reject behavior, and a transactional `run`
lifecycle. Automated race tests include an in-memory 256 KiB TCP Echo flow and
TCP half-close handling. Phase 6 adds bounded UDP sessions and interface-bound
UDP forwarding. Root HTTPS, a complete 10 MiB download, timeout fallback, and
clean system restoration have passed. Plain HTTP and successful two-interface
packet-capture evidence remain for the TCP MVP. UDP/53, concurrent sessions, a
1,139-byte DNSSEC response, and clean restoration have passed root acceptance;
QUIC awaits an HTTP/3-capable client.

Phase 7 adds durable Fake IP mappings, atomic SIGHUP reloads, live runtime
metrics, explicit resource limits, and automatic recovery from interface
changes; root reload/restart and network recovery passed, while the strict
24-hour soak remains a release gate.

Phase 8.1–8.3 add dual-stack outbound, Fake IPv6, and a runtime-gated IPv6 utun
path. Phase 8.4 adds opt-in split-default and literal-IP capture; its root
dual-stack routing, literal TCP forwarding, and normal/crash cleanup have
passed. Literal UDP, two-interface packet-capture evidence, and deliberate
conflict injection have also passed the strict acceptance checklist.

Phase 8.5 adds two-stage post-resolution IPv4/IPv6 CIDR policy and keeps each
flow's final decision immutable across outbound re-resolution and reload. Its
IPv4 dual-interface routing, literal-IP, reload-generation, and restoration
root acceptance passed; native IPv6 forwarding remains pending on an
IPv6-capable physical network.

Phase 8.6 proves that macOS `libproc` socket polling is diagnostically useful
but cannot provide deterministic process routing under process exit and
cross-process UDP `SO_REUSEPORT`; process rule fields remain unsupported and
are not exposed.

Phase 8.7a adds a transactional root LaunchDaemon lifecycle with fixed managed
paths, crash recovery, atomic upgrade rollback, and preservation-safe
uninstall. Its automated gates pass; strict root launchd installation and
forwarding acceptance passed on 2026-08-19, followed by clean stop/restart and
transactional upgrade acceptance. Crash recovery and preservation-safe
uninstall also passed, so Phase 8.7a is complete.

Least-privilege process separation is complete in Phase 8.7b: a root supervisor
owns the privileged system transaction while a dedicated `_tun-proxy` worker
runs Fake DNS, policy, netstack, resolvers, and relays. Transactional upgrade,
clean stop/start, managed-service preflight, PID replacement, worker ownership,
and en7 intranet DNS/TCP routing passed strict acceptance on 2026-08-19. The
controlled DNS replay produced 20 Fake IP answers with no increase in the DNS
failure counter.

Phase 9 adds the CLI operations and diagnostics documented in
[`../phases/phase9-cli-operations.md`](../phases/phase9-cli-operations.md).
The CLI is the only planned configuration, diagnostics, and service-management
interface; no graphical management interface is planned. Managed configuration
synchronization is separate from hot reload: `service sync-user-config` keeps a
stopped service stopped or restarts a running service with rollback, while
`service reload` remains available only to a running service. Managed reload now
uses the root-only `/var/run/tun-proxy/control.sock`, sends an expected config
digest, and waits for the final supervisor/worker result; SIGHUP remains only as
a manual compatibility entry point.

The one-off Phase 0, 2, 3, 8.6, and 8.7b spike commands were removed after
production implementation and acceptance. Their historical evidence remains
under `docs/phases/`, and `cmd/tun-proxy` is now the only command entry point.

## Remaining release work

- Complete the strict 24-hour Phase 7 soak.
- Complete pending TCP MVP plain HTTP and two-interface packet-capture evidence.
- Run QUIC acceptance with an HTTP/3-capable client.
- Run native IPv6 forwarding acceptance on an IPv6-capable physical network.
- Run the remaining root Phase 9 restart, configuration-sync, reload, and logs acceptance during a
  maintenance window that does not disrupt the active service.
