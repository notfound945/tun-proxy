# Phase 8.5 post-resolution IP/CIDR rules

Phase 8.5 adds `rules[].ip_cidr` without moving DNS I/O into the pure rules
engine or weakening per-outbound resolver isolation.

## Configuration contract

```yaml
rules:
  - domain_suffix:
      - example.net
    ip_cidr:
      - 203.0.113.0/24
      - 2001:db8::/32
    outbound: wired

  - outbound: wifi
```

Prefixes must be canonical IPv4 or IPv6 CIDRs. Duplicate prefixes within one
rule are removed. IPv4-mapped IPv6 and prefixes containing host bits are
rejected. Fields within one rule are conjunctive; multiple domains, suffixes,
and CIDRs are alternatives within their respective field.

A pure `ip_cidr` rule does not allocate a Fake IP. To apply it to ordinary
real-IP or literal-IP traffic, `capture.default_route` must be `true` so that
the traffic reaches the TUN. A rule combining `domain` or `domain_suffix` with
`ip_cidr` can still work while default-route capture is disabled: the explicit
domain predicate allocates the Fake IP, and the CIDR is evaluated after the
flow enters the TUN and the real destination is resolved.

## Decision contract

For a domain-backed flow:

1. Match domain predicates without treating an unresolved CIDR as a success or
   failure.
2. Resolve through the conclusive non-CIDR candidate outbound. If it is
   reject-only, an earlier base-matching direct CIDR candidate may supply the
   isolated resolver. If every possible winner is reject, stop without I/O.
3. Scan the original rules in YAML order using the complete resolved address
   set. A CIDR rule matches if any address is contained by any configured
   prefix.
4. If the selected policy outbound differs from the resolution outbound,
   discard those addresses and resolve once through the selected outbound.
5. Freeze the rule ID and policy outbound. Resolver or dial fallback may select
   another operational route, but does not re-run policy.

The second answer set is deliberately not re-matched. This prevents two
split-horizon resolvers from oscillating the selected outbound. Direct IPv4 or
IPv6 metadata is matched immediately against its literal destination and never
invokes DNS.

Reload builds a new immutable matcher/resolver/dialer generation. A TCP flow or
UDP session holds the generation acquired at creation until it finishes; only
new flows observe the new CIDRs.

## Automated gate

```sh
go test -race ./...
go vet ./...
go build ./...
```

Coverage includes validation, precedence, combined predicates, multiple A/AAAA
answers, outbound changes and re-resolution, resolver fallback, reject
candidates, direct IP metadata, TCP/UDP dialing, and reload generation lifetime.

## Handoff status (2026-08-19)

- Phase 8.4 automated and strict root acceptance are complete. This includes
  dual-stack split-default capture, scoped physical egress, literal IPv4 UDP,
  normal shutdown, `kill -9` recovery, and refusal of a pre-existing exact
  gateway route. See [`phase8-default-route.md`](phase8-default-route.md).
- Phase 8.5 implementation is present in commit `b301c2f`. `go test -race
  ./...`, `go vet ./...`, and `go build ./...` passed on 2026-08-18.
- Phase 8.5 IPv4 root interface-routing, literal-IP, reload-generation, and
  restoration smoke tests passed on 2026-08-19. The native IPv6 portion remains
  skipped because the accepted host has no usable non-link-local IPv6 address
  on either configured physical outbound.
- `/tmp/tun-proxy-phase85.yaml` from the first Mac is disposable local state.
  It is not in Git and will not exist on another Mac. Recreate it using the
  procedure below and use current DNS answers instead of the addresses seen on
  2026-08-18.

## Root acceptance on another Mac

The commands below use `wifi/en0` as the initial resolution candidate and
`wired/en7` as the CIDR-selected outbound. Those names describe the first test
Mac only. If the new Mac reports different interfaces, update both outbound
`interface` fields and every `tcpdump` command consistently.

### 1. Sync, build, and identify the topology

The commits must have been pushed from the first Mac before `git pull` can see
them.

```sh
git pull --ff-only
git log --oneline -n 10

go test -race ./...
go vet ./...
go build -o ./bin/tun-proxy ./cmd/tun-proxy

./bin/tun-proxy interfaces
route -n get default
route -n get -inet6 default
```

For each intended direct outbound, record its interface name, IPv4 address,
IPv4 gateway, usable IPv6 address and gateway if present, and reachable DNS
server. The following checks assume `en0` and `en7`; substitute the new names.

```sh
ifconfig en0
ifconfig en7
route -n get -ifscope en0 1.1.1.1
route -n get -ifscope en7 9.9.9.9

dig @1.1.1.1 example.net A +short +time=5 +tries=1
dig @9.9.9.9 example.net A +short +time=5 +tries=1
```

Do not continue until both direct outbounds have a usable scoped route and
their configured DNS server answers. If native IPv6 is unavailable, the IPv4
portion remains valid and the IPv6 portion should be recorded as skipped for
environmental reasons.

Create the root-owned runtime directories if the new Mac does not have them:

```sh
sudo install -d -o root -g wheel -m 0755 \
  /var/run/tun-proxy \
  /var/lib/tun-proxy
```

### 2. Capture a clean baseline

```sh
netstat -rn -f inet  > /tmp/tun-proxy-phase85-routes4-before.txt
netstat -rn -f inet6 > /tmp/tun-proxy-phase85-routes6-before.txt
scutil --dns          > /tmp/tun-proxy-phase85-dns-before.txt
```

### 3. Build disposable A and B configurations

Query the candidate resolver immediately before creating the config. CDN
answers can change between machines or runs, so never copy the old exact
addresses. Keep every returned address: matching any member of the complete
answer set is part of the acceptance criterion.

```sh
dig @1.1.1.1 example.net A +short +time=5 +tries=1
dig @1.1.1.1 example.net AAAA +short +time=5 +tries=1
dig @1.1.1.1 speed.cloudflare.com A +short +time=5 +tries=1
dig @1.1.1.1 speed.cloudflare.com AAAA +short +time=5 +tries=1

cp ./configs/config.yaml /tmp/tun-proxy-phase85-a.yaml
cp ./configs/config.yaml /tmp/tun-proxy-phase85-b.yaml
```

Edit both disposable files, not the tracked config. In both files:

- set `capture.default_route: true`;
- set `wifi.interface` and `wired.interface` to the interfaces found above;
- set the `wifi` and `wired` DNS servers to endpoints proven through those
  interfaces;
- keep the state, lock, and persistence paths under the root-owned directories.

In configuration A, replace `rules` with the block below. Replace each example
address with every current answer printed above, using `/32` for IPv4 and
`/128` for IPv6. Omit the IPv6 entries if the new network has no usable native
IPv6 path.

```yaml
rules:
  # Deferred rule: first resolve through the later wifi candidate, then select
  # wired when any current real answer matches.
  - domain:
      - example.net
      - speed.cloudflare.com
    ip_cidr:
      - 192.0.2.10/32       # replace with each current A answer
      - 2001:db8::10/128    # replace with each current AAAA answer
    outbound: wired

  # Literal-IP request: match immediately without DNS.
  - ip_cidr:
      - 192.0.2.10/32       # same current A answers
      - 2001:db8::10/128    # same current AAAA answers
    outbound: wired

  # Pre-resolution candidate for the two hostnames.
  - domain:
      - example.net
      - speed.cloudflare.com
    outbound: wifi

  - outbound: wifi
```

Configuration B is the reload target. Give it the same non-rule settings as A,
but remove both CIDR rules so new flows select `wifi`:

```yaml
rules:
  - domain:
      - example.net
      - speed.cloudflare.com
    outbound: wifi

  - outbound: wifi
```

Validate both files before modifying the system. A stale exact route from a
VPN or an earlier run must be investigated or removed by its owner; do not
weaken the preflight check.

```sh
sudo ./bin/tun-proxy check -config /tmp/tun-proxy-phase85-a.yaml
sudo ./bin/tun-proxy check -config /tmp/tun-proxy-phase85-b.yaml

cp /tmp/tun-proxy-phase85-a.yaml /tmp/tun-proxy-phase85-live.yaml
```

### 4. Prove candidate resolution and CIDR-selected egress

Use four terminals:

1. Start the process with the live file:

   ```sh
   sudo ./bin/tun-proxy run -config /tmp/tun-proxy-phase85-live.yaml
   ```

2. Capture the candidate interface. Replace the DNS address if `wifi` uses a
   different resolver:

   ```sh
   sudo tcpdump -ni en0 -vv \
     '(host 1.1.1.1 and port 53) or tcp port 80 or tcp port 443'
   ```

3. Capture the selected interface. Replace the DNS address if `wired` uses a
   different resolver:

   ```sh
   sudo tcpdump -ni en7 -vv \
     '(host 9.9.9.9 and port 53) or tcp port 80 or tcp port 443'
   ```

4. Generate a fresh domain-backed flow:

   ```sh
   dig @127.0.0.1 example.net A +short
   curl -4 --noproxy '*' --connect-timeout 10 --max-time 30 \
     http://example.net/
   ```

Acceptance requires the first real lookup on the `wifi` interface, the second
lookup on the `wired` interface after the CIDR match changes the outbound, and
the final TCP connection on `wired`. Seeing only the final TCP connection is
not sufficient evidence for the two-stage decision. If the CDN rotated its
answer before the test, regenerate the exact prefixes and restart from the
validation step.

When native IPv6 is enabled in `status`, repeat with `dig ... AAAA` and
`curl -6`. The final IPv6 connection must also use `wired`:

```sh
dig @127.0.0.1 example.net AAAA +short
curl -6 --noproxy '*' --connect-timeout 10 --max-time 30 \
  http://example.net/
```

### 5. Prove literal-IP matching skips DNS

Choose one of the current `example.net` A answers already included as `/32` in
configuration A:

```sh
phase85_literal_ip="$(dig @1.1.1.1 example.net A +short +time=5 +tries=1 | head -n 1)"
echo "$phase85_literal_ip"

curl -4 --noproxy '*' --connect-timeout 10 --max-time 30 \
  -H 'Host: example.net' "http://${phase85_literal_ip}/"
```

Run a narrow `tcpdump` for that address on both physical interfaces while
issuing the request. The TCP flow must appear on `wired`; the request must not
produce a resolver query because literal metadata is matched directly. Ambient
DNS traffic from other applications should not be counted as evidence.

### 6. Prove reload generation isolation

Start a deliberately slow transfer under configuration A. It should first
appear on `wired` and remain active long enough to reload:

```sh
curl -4 --noproxy '*' --limit-rate 16k --max-time 180 \
  -o /dev/null \
  'https://speed.cloudflare.com/__down?bytes=1048576' &
phase85_old_curl_pid=$!
```

Atomically replace the live config with B, signal the PID reported by the
status endpoint, and check the reload counters:

```sh
cp /tmp/tun-proxy-phase85-b.yaml /tmp/tun-proxy-phase85-live.yaml.next
mv /tmp/tun-proxy-phase85-live.yaml.next /tmp/tun-proxy-phase85-live.yaml

phase85_proxy_pid="$(sudo ./bin/tun-proxy status -json | jq -r '.pid')"
sudo kill -HUP "$phase85_proxy_pid"
sudo ./bin/tun-proxy status -json | jq '{config_digest,reload,tcp,udp}'
```

The process log must report `INFO configuration reloaded`, `reload.successes`
must increase, and `reload.failures` must remain zero. While the old transfer
is still visible on `wired`, create a new flow:

```sh
curl -4 --noproxy '*' --connect-timeout 10 --max-time 30 \
  -o /dev/null \
  'https://speed.cloudflare.com/__down?bytes=4096'

wait "$phase85_old_curl_pid"
```

The old connection must continue on `wired` without reconnecting or moving;
the new connection must use `wifi`, as required by configuration B. Save the
two physical-interface captures with the test notes. The automated generation
lifetime tests remain the authoritative check for internal reference release;
this root smoke supplies the host-visible routing and continuity evidence.

### 7. Stop and verify restoration

Stop the foreground process with Ctrl-C. It must report a clean stop. Then
capture the post-run state:

```sh
netstat -rn -f inet  > /tmp/tun-proxy-phase85-routes4-after.txt
netstat -rn -f inet6 > /tmp/tun-proxy-phase85-routes6-after.txt
scutil --dns          > /tmp/tun-proxy-phase85-dns-after.txt

diff -u /tmp/tun-proxy-phase85-routes4-before.txt \
        /tmp/tun-proxy-phase85-routes4-after.txt || true
diff -u /tmp/tun-proxy-phase85-routes6-before.txt \
        /tmp/tun-proxy-phase85-routes6-after.txt || true
diff -u /tmp/tun-proxy-phase85-dns-before.txt \
        /tmp/tun-proxy-phase85-dns-after.txt || true

sudo ./bin/tun-proxy status -json
sudo ls -la /var/run/tun-proxy
sudo lsof -nP -iTCP:53 -iUDP:53 | grep tun-proxy || true
ifconfig utun4
```

Replace `utun4` with the name printed at startup. Acceptance requires ordinary
physical default routes, no tun-proxy DNS diff, no utun, no state file/socket/
lock, and no tun-proxy listener on port 53. Dynamic neighbor-cache rows and
expiration counters may differ in route-table diffs; owned split-default,
Fake-IP, gateway-bypass, or DNS-bypass routes may not remain.

If the process is killed or the machine reboots before normal cleanup, use the
recorded state rather than manually deleting guessed routes:

```sh
sudo ./bin/tun-proxy cleanup -state /var/run/tun-proxy/state.json
```

Do not delete the state file when cleanup refuses ownership: preserve it,
inspect the reported route/interface mismatch, and rerun cleanup after the
kernel has removed a dead utun route or the conflicting owner is understood.

## Acceptance record

Result: **PASS for the available IPv4 environment on 2026-08-19. Native IPv6
root forwarding remains skipped because the host has no usable physical IPv6
path.**

The accepted host was a MacBook Pro `Mac16,7` with Apple M4 Pro, arm64, running
macOS 15.7.7 (24G720). The two direct outbounds were:

- `wifi`: `en0`, source `192.168.11.101`, gateway `192.168.11.1`, resolver
  `1.0.0.1:53`;
- `wired`: `en7`, source `172.20.152.20`, gateway `172.20.152.254`, resolver
  `8.8.4.4:53`.

Both physical interfaces exposed only link-local IPv6. Runtime capability
therefore reported `configured=true`, `enabled=false`, and Fake AAAA remained
NODATA. No IPv6 CIDR forwarding claim is made from this environment.

Configuration A contained the current `/32` answers `104.20.21.8`,
`172.66.175.59`, `172.66.0.218`, and `162.159.140.220`. Configuration B removed
the CIDR rules so new matching flows selected `wifi`. Both strict root checks
passed before startup.

The domain-backed `example.net` test returned Fake IP `198.18.1.161`. At
09:34:26, the candidate lookup appeared on `en0` to `1.0.0.1` and returned
`104.20.21.8` plus `172.66.175.59`. After the CIDR match selected `wired`, a
second lookup appeared on `en7` to `8.8.4.4`, followed by the TCP connection
from `172.20.152.20` to `104.20.21.8:80`. The request completed with HTTP 200.
This proves candidate resolution, ordered CIDR post-match, outbound change,
outbound-local re-resolution, and final interface-bound dialing.

The literal test connected directly to `104.20.21.8:80` with
`Host: example.net`. It completed with HTTP 200 and appeared only on `en7` as
source `172.20.152.20:50555`. Neither physical capture contained an
`example.net` lookup during the test window, proving that literal metadata was
matched without DNS.

The first reload test was the authoritative A-to-B generation test. The old
1 MiB rate-limited transfer opened at 09:42:44.430 as
`172.20.152.20:50684 -> 162.159.140.220:443` and remained exclusively on `en7`
for 1,350 captured packet records until its normal FIN at 09:43:48.099. After
reload, the new 4 KiB flow opened at 09:42:51.499 as
`192.168.11.101:50686 -> 162.159.140.220:443`, appeared exclusively on `en0`,
and completed normally at 09:42:51.726. The old flow therefore remained on its
acquired generation for roughly 56 seconds after the new flow completed; it
did not move, reconnect, or reset.

The operator accidentally ran the reload command block twice. The process
reported two successful reloads and zero failures. During the second B-to-B
run, both ports `50728` and `50731` correctly appeared on `en0`; this redundant
run was not used as the A-to-B isolation proof and does not weaken the first
capture.

Normal Ctrl-C shutdown reported a clean stop with 382 TCP flows, 11 UDP
sessions, 442 DNS queries, 46,726 TUN receive packets, and 50,021 TUN transmit
packets. Post-run DNS was byte-for-byte unchanged. Route-table diffs contained
only normal ARP/neighbor cache churn: ordinary defaults remained on `en7` and
`en0`, with no split-default, Fake IPv4, Fake IPv6, gateway-bypass, or
resolver-bypass route left behind. `utun7`, the state file, status socket,
lock, port-53 listener, and tun-proxy process were absent. The runtime
directory was empty, and both persistent mapping files remained `0600`
`root:wheel`.

Saved local evidence is under `/tmp/tun-proxy-phase85-*`, including separate
candidate/selected-interface, literal-IP, reload, route, and DNS captures.
