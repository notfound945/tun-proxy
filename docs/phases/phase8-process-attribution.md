# Phase 8.6 process attribution spike

Status: **COMPLETE — process rules are unsupported by the current architecture**

Date: 2026-08-19

## Decision

Do not add process-name, executable-path, PID, bundle-ID, or similar rule
fields to the production configuration.

macOS `libproc` can expose a point-in-time mapping from an existing socket FD
to its owning PID. That is useful for diagnostics, but the mapping is not a
flow-creation event and has no historical identity. The Phase 8.6 probe proved
that polling it cannot satisfy the project's required deterministic behavior:

- a TCP socket can disappear with its process before attribution is read;
- two processes can own unconnected UDP sockets with the same local endpoint
  through `SO_REUSEPORT`, making one observed UDP flow match both PIDs;
- scanning the system process and FD tables is not atomic, so process exit and
  FD reuse can occur between enumeration and socket inspection.

Running the scanner as root removes permission failures. It does not remove
these lifetime and ambiguity problems. A privileged helper that performs the
same polling therefore does not change this decision.

## Architecture finding

The TUN/gVisor boundary provides the original client-side protocol, source
address/port, and Fake or literal destination address/port before tun-proxy
creates its separate outbound socket. The available tuple is therefore the
correct tuple to search; the problem is not accidental attribution to
tun-proxy's own outbound connection.

The production flow path currently is:

1. macOS routes the application's packet to utun;
2. gVisor creates a TCP flow or connected UDP session and freezes its tuple;
3. the session layer resolves Fake IP metadata and evaluates rules;
4. tun-proxy creates a separate interface-bound upstream socket.

The diagnostic scanner remains isolated in `internal/procattrib` and is not
imported by the rules, session, or runtime packages. The disposable Phase 8.6
command was removed after the unsupported-process-routing decision was recorded.

## Probe behavior

The Darwin scanner uses the SDK's public `libproc` interfaces:

- `proc_listpids(PROC_ALL_PIDS)` to enumerate processes;
- `proc_pidinfo(PROC_PIDLISTFDS)` to enumerate file descriptors;
- `proc_pidfdinfo(PROC_PIDFDSOCKETINFO)` to read TCP/UDP socket tuples;
- `proc_name` to attach a diagnostic process name.

An answer is `unique` only when exactly one distinct process/socket owns the
flow. Zero matches return `none`; multiple matches return `ambiguous`.
Duplicate FDs for the same process and kernel socket are deduplicated, while a
socket inherited by another process remains multiple ownership.

Unconnected UDP sockets have no foreign endpoint in the socket table. They
must be matched by protocol and local endpoint, which is precisely why
cross-process `SO_REUSEPORT` cannot be reduced to one correct owner after a
packet is observed.

## Automated and local results

Unit tests cover exact and mismatched tuples, wildcard local addresses,
unconnected UDP matching, address-family validation, and the
none/unique/ambiguous classification.

The self-test creates no utun, route, DNS, or configuration changes. It starts
short-lived child processes and performs four checks:

1. six simultaneous TCP connections each resolve to their exact child PID;
2. all six remain unique while concurrent;
3. after one child exits, its former tuple resolves to no owner;
4. two child processes bind the same UDP source endpoint with `SO_REUSEPORT`,
   send to the same destination, and both appear as valid owners.

The development Mac reproduced all four conditions as the normal development
user on 2026-08-19; all child sockets in that run had the same UID as the
scanner. TCP lookup took approximately 1–2 ms per full process-table scan.
Timing is observational only and is not a production latency guarantee.

The strict root replay also passed on 2026-08-19. It uniquely attributed all
six concurrent TCP flows, with observed full-table scans of 2–3 ms, returned
`none` after the first TCP owner exited, and returned two distinct PIDs for the
shared UDP `SO_REUSEPORT` endpoint. It ended with
`RESULT process_attribution=unsupported`. This confirms that permission to
inspect the complete process table does not change either architectural
failure.

## Archived strict probe

The dedicated probe and its tuple-lookup CLI were removed after both normal-user
and strict-root acceptance reproduced the process-exit race and cross-process
UDP `SO_REUSEPORT` ambiguity. The stable result was
`RESULT process_attribution=unsupported`; this document preserves the evidence,
but the historical probe is no longer shipped or built.

## Conditions for revisiting process rules

Process policy can be reconsidered only if the architecture obtains identity
at flow creation rather than by later socket-table polling. Plausible designs
include an entitled Network Extension that supplies source-application
identity with each flow, or explicit cooperation from the originating
application through an authenticated local control/proxy protocol.

Phase 8.7 may still move route, DNS, utun, and socket-binding privileges into a
narrow helper, but that work must not claim to unlock deterministic process
rules by itself.
