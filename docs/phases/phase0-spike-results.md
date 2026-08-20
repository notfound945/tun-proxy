# Phase 0 feasibility spike

Status: **PASS** (packet-capture evidence still recommended)

Date: 2026-08-18

## Environment

| Item | Value |
|---|---|
| macOS | 15.7.7 (24G720) |
| Architecture | arm64 |
| Go | 1.26.1 |
| Active test interfaces | `en0`, `en7` |
| utun implementation | `golang.zx2c4.com/wireguard/tun` |

## Automated results

| Check | Result | Evidence |
|---|---|---|
| Enumerate names, indexes, addresses and flags | PASS | `tun-proxy interfaces` lists both active interfaces and their IPv4 addresses. |
| Bind IPv4 TCP socket with `IP_BOUND_IF` | PASS | Connections on both interfaces used an IPv4 source assigned to the selected interface. |
| Bind IPv4 UDP/DNS socket with `IP_BOUND_IF` | PASS | DNS responses arrived on both interfaces and each socket used the selected interface's source address. |
| Unreachable target is diagnosable | PASS | A public target unavailable through the scoped corporate interface returned a bounded `i/o timeout`, without falling back to the other interface. |
| Build WireGuard utun implementation | PASS | The pinned Darwin implementation compiles on arm64 and exposes `CreateTUN`, batched I/O and `Close`. |
| Create utun and set MTU | PASS | The root probe created `utun7` with MTU 1400 and reported batch size 1. The interface disappeared automatically after the probe process exited. |
| Read and close utun | PASS | After enforcing the four-byte Darwin packet offset, closing `utun7` woke the blocked read and destroyed the interface. |
| Confirm traffic with `tcpdump` | PENDING | Requires root and two terminals. |

No system DNS or route was changed during the completed probes.

## Archived acceptance procedure

The disposable Phase 0 command was removed after the platform assumptions were
integrated into the production implementation and the gate passed. The commands
that produced this record are therefore no longer runnable from the current
tree. Future macOS acceptance should exercise the production `tun-proxy`
binary and use `tcpdump` on each selected physical interface when packet-level
evidence is required.

## Gate decision

The two platform assumptions are proven: sockets remain scoped to either active
physical interface, and the selected utun library has the required lifecycle
behavior. Phase 1 may proceed. Packet captures remain a required item in the
final macOS acceptance run because source-address verification does not replace
the release evidence requested by the test plan.
