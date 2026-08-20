# Phase 7 stability and observability acceptance

Phase 7 adds atomic Fake IP persistence, `SIGHUP` reloads for mutable
configuration, a root-only runtime status socket, explicit resource limits,
and automatic rebuilding of interface-bound dialers and resolvers after
network changes or a sleep interval.

## Build and start

Build as the normal user, then run the already validated root transaction:

```sh
go build -o ./bin/tun-proxy ./cmd/tun-proxy
sudo ./bin/tun-proxy check -config ./configs/config.yaml
sudo ./bin/tun-proxy run -config ./configs/config.yaml
```

In another terminal, inspect the concise and machine-readable snapshots:

```sh
sudo ./bin/tun-proxy status
sudo ./bin/tun-proxy status -json
```

The snapshot includes active and total TCP/UDP sessions, DNS and Fake IP
counters, TUN and netstack bytes/packets, capacity rejects, goroutines, open
file descriptors, Go heap use, configured limits, reload outcomes, and network
refresh outcomes. The status socket is recorded in the recovery state and is
removed on both normal shutdown and `cleanup` after a crash.

## SIGHUP reload

Change only a mutable field in `configs/config.yaml`, such as a rule outbound,
fallback, upstream DNS server, log level, DNS TTL, connect timeout, or UDP idle
timeout. Then signal the live PID reported by the status endpoint:

```sh
pid="$(sudo ./bin/tun-proxy status -json | python3 -c 'import json,sys; print(json.load(sys.stdin)["pid"])')"
sudo kill -HUP "$pid"
sudo ./bin/tun-proxy status
```

The process must print `INFO configuration reloaded`, `reloads` must increase,
and existing flows must continue. Invalid YAML, a missing/down interface, or a
change to an immutable field must print `WARN configuration reload rejected`;
the current generation and config digest must remain active.

Immutable fields are the state/lock paths, TUN address/peer/MTU/queue/buffer
settings, Fake IP CIDR/mapping TTL/capacity/persistence path, DNS listener and
protocols/concurrency, and the global TCP flow limit.

## Persistence and network recovery

Resolve a domain, stop cleanly, restart, and resolve it again. Its Fake IP must
remain identical, and `/var/lib/tun-proxy/fake-ip.yaml` must be mode `0600`.
New allocations are fsynced and atomically renamed before DNS answers are
returned. Invalid file contents are renamed with a `.corrupt-<UTC timestamp>`
suffix and a fresh pool is used; unsafe paths, ownership, or permissions remain
fatal instead of being silently replaced.

While the proxy is running, sleep and wake the Mac, toggle Wi-Fi, or unplug and
reconnect a configured wired interface. During an outage new flows may follow
their configured fallback. Once all configured interfaces are usable again,
the process must print `INFO network state refreshed`; the `network.refreshes`
counter must increase and new flows must work without restarting the process.

## 24-hour soak

Leave the proxy running in the first terminal. Run the monitor in a second
terminal. The arguments are duration, sample interval, and optional HTTPS
workload timeout in seconds; the HTTPS timeout defaults to 45 seconds so a
10-second primary attempt and its fallback both have time to complete:

```sh
sudo ./scripts/phase7-soak.sh 86400 60
```

For a short smoke run before the full soak:

```sh
sudo ./scripts/phase7-soak.sh 300 10
```

The script records one compact JSON snapshot per line under `/tmp`, performs a
DNS query and HTTPS request each interval, timestamps each workload failure,
and stops immediately if the status endpoint disappears. A manual `SIGINT` or
`SIGTERM` prints a `STOP` summary with the sample and failure counts instead of
mistaking the interrupted request for a workload failure. Acceptance requires
no sustained upward trend in goroutines, open FDs, heap use, or active sessions
after traffic settles; no capacity rejects under the intended workload;
continued traffic after network recovery; and complete route, DNS, utun, lock,
state, and socket cleanup after the final Ctrl-C.

## Recorded root smoke result

On 2026-08-18, the macOS root smoke test passed with `utun7`. An unchanged
configuration reloaded through `SIGHUP` without interrupting active traffic;
the live snapshot advanced from `reloads=0` to `reloads=1` with no failure.
The persistence file was owned by `root:wheel` with mode `0600`. After a clean
stop and process restart, `example.com` retained `198.18.0.37`. The restarted
snapshot reported 42 goroutines, 19 open file descriptors, about 2.9 MiB of Go
heap, six active TCP flows, and a responsive DNS/TUN data path. The 24-hour
soak and manual sleep/interface-change exercises remain pending.

The five-minute pre-soak then completed 15 workload cycles with zero DNS or
HTTPS failures. It added 111 TCP flows, 38 UDP sessions, and 201 DNS queries.
Ambient traffic briefly raised the process to 236 goroutines, 84 FDs, and 34
active UDP sessions; all three recovered to 89, 35, and 4 respectively. Heap
allocation peaked near 23.5 MiB, dropped to 13.7 MiB after GC, and ended near
18.0 MiB, demonstrating reclamation rather than monotonic growth. The raw 15
snapshot samples were recorded in
`/tmp/tun-proxy-soak-20260818T050232Z.ndjson`.

The initial topology-recovery smoke exposed a state-machine edge case: after a
failed refresh, recovery to the exact original interface fingerprint skipped
the successful rebuild. Commit `574a796` added an explicit pending state and a
regression test for `healthy -> down -> original healthy`. The repeated root
test then passed two recovery cycles. Terminal logs showed both transitions
from `WARN network refresh pending` to `INFO network state refreshed`; the live
snapshot reported `refreshes=2`, a current `last_success`, and no `last_error`.
Both en0 and en7 had restored IPv4 addresses, while resources settled at 91
goroutines, 36 FDs, and about 7.9 MiB heap.

## Recorded three-hour soak checkpoint

The restarted long soak produced 166 snapshots from
`2026-08-18T06:12:09Z` through `2026-08-18T09:25:33Z`, a continuous checkpoint
of 3 hours, 13 minutes, and 24 seconds. During that interval the proxy handled
3,513 new TCP flows, 1,017 new UDP sessions, 6,610 DNS queries, 807,923 TUN
receive packets, and 1,475,434 TUN transmit packets while the machine remained
in normal interactive use.

Goroutines ranged from 107 to 332 and ended at 201; open FDs ranged from 41 to
117 and ended at 73; allocated heap ranged from 9,423,456 to 41,833,560 bytes
and ended at 23,746,744 bytes. Peaks repeatedly returned to lower values, and
Go system memory reached 76,142,872 bytes and then remained stable. The final
snapshot's lifecycle accounting was exact: `2533 + 1116 + 42 = 3691` TCP flows
and `990 + 13 + 18 = 1021` UDP sessions. Its 60 Fake IP references also equaled
42 active TCP flows plus 18 active UDP sessions. DNS capacity rejects and Fake
IP exhaustions remained zero.

The exercise also included physical-interface outages and recoveries. The
runtime recorded three successful network refreshes after eight unavailable
interface observations and continued carrying traffic afterward. Clean daemon
shutdown reported 3,695 TCP flows, 1,027 UDP sessions, 6,833 DNS queries,
822,525 TUN receive packets, and 1,488,961 TUN transmit packets. The Fake IP
route returned to the physical default route, `utun7` disappeared, port 53 had
no remaining proxy listener, and the original system DNS resolvers were
restored.

One HTTPS workload request timed out after 20 seconds while the proxy was still
running. Because the run was started with a 24-hour duration and manually
stopped after the three-hour checkpoint, the script did not reach its terminal
summary, but its accumulated workload failure count was therefore nonzero.
The three-hour resource-stability checkpoint and manual network-recovery gate
pass; the end-to-end workload reliability gate does not pass this run and must
be repeated after the timeout is investigated. The strict uninterrupted
24-hour release gate also remains pending. Raw snapshots are in
`/tmp/tun-proxy-soak-20260818T061209Z.ndjson`.

The timeout investigation found that the monitor's former 20-second HTTPS
budget was too close to the configured route's worst-case timing: the
`example.com` rule first permits a 10-second attempt through `wired/en7`, then
falls back to `wifi/en0`, leaving insufficient headroom for fallback DNS, TLS,
and response transfer. The monitor now defaults to a 45-second HTTPS budget,
timestamps workload failures, and prints an auditable summary when interrupted.
A five-minute regression with the amended monitor passed all 15 DNS/HTTPS
cycles with zero workload failures. Its snapshots added 105 TCP flows, 26 UDP
sessions, 211 DNS queries, 10,910 TUN receive packets, and 8,237 TUN transmit
packets. Final accounting remained exact, with no DNS failure, capacity reject,
or Fake IP exhaustion. The regression snapshots are in
`/tmp/tun-proxy-soak-20260818T093204Z.ndjson`; a long-duration zero-failure run
is still required by the 24-hour release gate.
