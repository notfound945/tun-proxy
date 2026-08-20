# Phase 8.7b least-privilege separation

Phase 8.7a deliberately runs the entire LaunchDaemon as root. Phase 8.7b must
replace that arrangement with a narrow privileged lifecycle owner and a
non-root policy/data-plane worker without weakening route, DNS, or cleanup
transactions.

This document records the boundary audit, capability gate, production split,
and strict acceptance. The capability gate alone did not complete the phase;
the production topology and lifecycle acceptance described below did.

## Current privilege audit

`internal/app/run_darwin.go` currently combines all of the following in one
root process:

- utun creation, address assignment, route installation, DNS mutation, network
  refresh, and transactional cleanup;
- binding the loopback Fake DNS UDP/TCP port, normally port 53;
- Fake IP persistence, rule compilation, resolver selection, gVisor, TCP/UDP
  sessions, relay I/O, reload, status, and metrics.

Dropping UID in that process after startup is not sufficient. Route and DNS
refresh still require a privileged lifecycle owner, while cleanup must retain
the exact state and authority needed to roll back only changes made by this
instance. Conversely, policy evaluation, netstack processing, resolver queries,
and relay sockets do not need root once their required descriptors and data
ownership are established.

The intended production boundary is therefore:

```text
system LaunchDaemon
└── root supervisor
    ├── validates fixed paths, ownership, and service identity
    ├── creates/configures utun
    ├── binds loopback UDP/TCP port 53
    ├── owns route and system-DNS transactions and refresh
    ├── retains cleanup journal and original utun descriptor
    └── execs a dedicated non-root worker with only:
        ├── private control descriptor
        ├── inherited utun descriptor
        ├── inherited UDP DNS descriptor
        └── inherited TCP DNS descriptor

non-root worker
├── Fake DNS serving and Fake IP persistence
├── rules, resolver, gVisor, sessions, and relays
├── interface-bound outbound TCP/UDP sockets
├── mutable reload compilation
└── status and metrics payloads
```

The first implementation should keep the control channel private to the
parent/child relationship. A public root helper socket would require a separate
authenticated protocol, peer-credential checks, request authorization, replay
rules, and versioning, none of which are necessary merely to split this one
service.

## Service identity requirement

The capability probe uses macOS `nobody` only to prove kernel behavior. It is
not an acceptable production identity:

- it is shared rather than dedicated to tun-proxy;
- it cannot safely own stable Fake IP mapping files;
- granting it access would grant the same access to unrelated processes using
  that identity.

Production separation requires a dedicated `_tun-proxy` account and a reviewed,
transactional ownership migration for its data directory. Installation,
upgrade, rollback, preservation-safe uninstall, and purge must all agree on the
identity and file modes before the account is created. Until then, the existing
root-owned production persistence remains unchanged.

Process attribution remains unsupported. Moving descriptors across this
boundary does not repair the process-exit race or `SO_REUSEPORT` ambiguity
recorded in [`phase8-process-attribution.md`](phase8-process-attribution.md).

## Capability implementation

The completed capability probe ran one root supervisor and execed the same
binary as an already-demoted worker. The supervisor cleared supplementary groups
and passed only file descriptors 3–6. Its disposable command was removed after
the production split-privilege topology passed strict acceptance:

| Child FD | Resource | Created by |
|---:|---|---|
| 3 | private Unix control socket | root supervisor |
| 4 | configured utun | root supervisor |
| 5 | bound UDP Fake DNS socket | root supervisor |
| 6 | bound TCP Fake DNS socket | root supervisor |

The repository is under a user-private `~/Documents` directory, so `nobody`
cannot traverse to the development binary. The supervisor copies its current
executable into a fresh root-owned mode-`0755` directory under `/tmp`, creates a
root-owned mode-`0600` persistence probe beside it, execs the staged copy, and
removes the exact temporary directory during cleanup. This models execution
from the future fixed helper path without changing installed service files.

The worker must prove all of the following:

1. its first application instruction observes the requested nonzero UID/GID
   and only the requested supplementary group;
2. it can reconstruct and read/write the inherited utun without running
   `ifconfig` itself;
3. real Fake DNS serves both UDP and TCP over inherited listeners after the
   supervisor closes all of its DNS copies;
4. an explicit upstream DNS query succeeds through
   `IP_BOUND_IF` as the non-root worker;
5. the worker cannot read the root-owned mode-`0600` persistence probe;
6. worker exit plus supervisor cleanup leaves no listener, temporary directory,
   or utun behind.

`internal/fakedns.Server.StartWithListeners` is reusable production groundwork.
On successful startup the running server owns the inherited sockets; on
validation failure ownership remains with the caller for deterministic
rollback. `internal/tun.OpenFile` similarly takes ownership of one inherited
utun descriptor and performs no interface reconfiguration.

## Archived capability gate

The dedicated Phase 8.7b command and its probe-only packet and credential tests
were removed after the descriptor handoff, inherited utun and DNS listeners,
non-root `IP_BOUND_IF`, credential boundary, and cleanup behavior were implemented
and accepted in the production supervisor/worker architecture. Current changes
must be validated through the production test suite and managed-service
acceptance procedure; this document retains the original strict-root result
below as historical evidence.

## Strict root acceptance result

Accepted on 2026-08-19 using the selected physical interface `en7`, upstream
`8.8.4.4:53`, and inherited loopback DNS address `127.0.0.1:53`.

The first two strict replays exposed Darwin representation details in the
probe rather than privilege-boundary failures:

- a raw `*os.File` wrapping the Unix socketpair does not support deadlines;
  both ends are now converted with `net.FileConn` before the JSON control
  protocol starts;
- the `nobody` account is reported as unsigned `4294967294` by `os/user`, while
  `geteuid`, `getegid`, and `getgroups` report signed `-2`; credential checks
  now compare the same underlying 32-bit `uid_t`/`gid_t` value.

Both cases have non-root regression tests. Each failed replay also restored
the utun baseline and removed its DNS listeners and staging directory.

The final strict replay produced all stable success markers:

```text
PASS worker already demoted pid=74141 uid=4294967294 gid=4294967294 groups=[4294967294]
PASS inherited utun opened name=utun7 without reconfiguration
PASS root-owned 0600 persistence probe denied to worker=nobody
PASS non-root IP_BOUND_IF DNS interface=en7 upstream=8.8.4.4:53 answers=2
PASS inherited UDP DNS listener=127.0.0.1:53 fake_answer=198.18.87.10
PASS inherited TCP DNS listener=127.0.0.1:53 fake_answer=198.18.87.10
PASS inherited utun read/write round trip local=10.255.87.2 peer=10.255.87.1
RESULT least_privilege_capability=supported
```

Simultaneous capture on `en7` observed the worker's UDP query from
`172.20.152.20:64969` to `8.8.4.4:53` and the successful response containing
two A records. After exit, `utun7`, the loopback DNS listeners, and the exact
`/tmp/tun-proxy-phase87b-*` staging directory were absent. This establishes
that the selected macOS kernel permits the already-demoted worker to open and
use every inherited data-plane descriptor required by the proposed split.

## Production implementation

The capability result was promoted into the managed LaunchDaemon with all
remaining production requirements implemented:

- `_tun-proxy` is a dedicated Open Directory user and group whose numeric IDs
  are assigned by the directory service and resolved at runtime;
- account creation, worker-directory ownership, Fake IP persistence ownership,
  upgrade rollback, preservation-safe uninstall, and purge are one managed
  transaction;
- a private versioned protocol covers worker identity confirmation, descriptor
  handoff, readiness, refresh, reload, shutdown, error reporting, and metrics;
- the root supervisor keeps the original utun descriptor, recovery state,
  route/system-DNS authority, refresh responsibility, and cleanup ordering;
- the non-root worker runs Fake DNS, persistence, policy, gVisor, resolvers,
  interface-bound TCP/UDP sockets, relays, reload compilation, status, and
  metrics;
- route and system-DNS mutation commit only after the worker has accepted its
  resources and become ready;
- worker failure is reported through the supervisor, which performs exact
  reverse cleanup before launchd recovers the service pair;
- `tun-proxy check -service -config PATH` validates root-owned state/lock paths
  separately from worker-owned IPv4/IPv6 persistence paths;
- service start accepts a published ready runtime even if the `launchctl
  kickstart` client is killed after successful activation, while retaining a
  hard failure for a runtime that never becomes ready.

Automated coverage includes protocol validation, malformed and stale message
handling, credential and descriptor checks, worker readiness/failure,
supervisor cleanup, storage/account rollback, install/upgrade/uninstall/purge,
managed preflight ownership, launchctl transport failure after readiness, and
the corresponding never-ready failure.

## Strict production acceptance result

Accepted on 2026-08-19. Transactional upgrade, clean stop/start, managed
preflight, PID replacement, and split-privilege process ownership all passed.
The observed launchd job had a root supervisor and a directly parented worker
running as UID/GID 499 (`_tun-proxy`). Runtime status reported the service as
installed, loaded, and running with configuration digest
`sha256:a68cd483d178306125bf09d87c9356cf235e1fb5791b7207d9744996b2290b9d`.

The accepted configuration uses `en7` for an intranet outbound with DNS
upstreams `192.168.100.51:53`, `192.168.96.51:53`, and
`192.168.196.51:53`, a ten-second connect timeout, and reject fallback. Rules
select it for `code.266.com` and label-boundary suffixes `oa.com`, `mtt.xyz`,
and `ifere.com`. Captures observed the expected DNS and TCP/TLS traffic on
`en7` without matching `en0` leakage. Application-level probes succeeded for
`code.266.com`, `phl.oa.com`, `mtt.xyz`, and `ifere.com`; the exercised HTTP
results were 302, 200, and 301.

After obsolete `/etc/hosts` overrides were removed, a controlled five-round
DNS replay issued 20 queries and received 20 Fake IPv4 answers. Runtime
counters changed by `Queries +20`, `FakeAnswers +20`, and `Failures +0`, with
zero capacity rejects. The existing cumulative failure count of 3 therefore
did not represent a failure in the acceptance window. The host had no usable
non-link-local physical IPv6 address, so the already implemented IPv6 path was
correctly reported as configured but runtime-disabled. Phase 8.7b is complete.
