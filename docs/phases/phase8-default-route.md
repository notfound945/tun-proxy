# Phase 8.4 default-route and literal-IP acceptance

Phase 8.4 is an explicit opt-in. Existing Fake IP-only installations keep
`capture.default_route: false` and retain their previous routing behavior.

## Design contract

When `capture.default_route` is enabled, startup performs the following work
before changing the effective default path:

1. Resolve every direct outbound DNS endpoint with a scoped route lookup on
   that outbound's configured physical interface.
2. Require a usable same-family gateway and reject any endpoint or gateway
   whose route resolves on a different interface.
3. Reject ambiguous host-route ownership, including one address required by
   multiple interfaces and exact pre-existing host routes that tun-proxy did
   not create.
4. Reuse macOS system-owned direct gateway host routes without recording or
   deleting them; otherwise persist and install the required gateway bypass.
   Then persist one bypass for every distinct upstream DNS address.
5. For every direct outbound, persist and install interface-scoped physical
   `0.0.0.0/1` and `128.0.0.0/1` routes through that interface's gateway.
   Then install the same prefixes globally through utun. When the Phase 8.3
   IPv6 capability gate is enabled, do the same for `::/1` and `8000::/1`.
6. Repeat scoped egress probes after installation and roll everything back
   before starting DNS or packet pumps if any bound physical path now selects
   utun or a different gateway.

The ordinary system default routes are not deleted or replaced. The global
split routes are more specific and therefore capture public literal
destinations. Sockets bound with `IP_BOUND_IF` select the equally specific
physical scoped split route instead, preserving loop-free direct egress.
Connected LAN routes and the recorded host bypasses remain more specific. The
local status control plane is a Unix socket and has no IP endpoint to bypass.

All additions are written to the recovery state before mutation. Shutdown and
crash cleanup remove them in strict reverse order: global utun split routes
first, physical scoped split routes second, then DNS and gateway bypasses,
then the Fake IPv6 and Fake IPv4 routes. Route removal repeats the recorded
scope and checks the current prefix mask, interface, and, where applicable,
gateway before deleting anything. If the recorded interface no longer exists,
its routes are treated as already removed by the kernel; an equally specific
route now selected on another interface is left untouched.

Direct-IP TCP and UDP sessions carry an empty domain plus the literal
destination. Domain predicates cannot match such flows; protocol, destination
port, and the final default rule can. The selected outbound dials the literal
address directly and does not invoke DNS. Recoverable socket failures may use
the already validated fallback chain.

While capture is active, outbound topology is immutable across SIGHUP. After a
sleep or interface change, the process re-proves the recorded bypass plan. A
changed or unprovable plan causes a controlled stop and rollback; restart is
required to install a new plan.

## Automated gate

```sh
go test -race ./...
go vet ./...
go build ./...
```

Coverage includes strict opt-in configuration, immutable captured topology,
scoped IPv4/IPv6 route lookup, gateway-based host routes, conditional cleanup,
ambiguous ownership rejection, split-route ordering, literal-IP rules, and
DNS-free literal TCP dialing.

## Recorded root preflight findings

The first root preflight on 2026-08-18 exposed two Darwin route presentation
details. Active on-link gateways already have cloned `LLINFO/WASCLONED/ROUTER`
host entries; these are now reused as system-owned bypasses and are never
recorded or removed. Also, `route get` renders `0.0.0.0/1` with destination
`default` plus a non-default mask. Route verification and cleanup now parse the
mask so a split route is not confused with the ordinary default.

The same run showed that a previously contacted IPv6 probe can acquire a
cloned host route. IPv6 capability detection now queries the family default
directly instead of inferring it from one public probe address. Regression
tests cover all three forms. Root forwarding acceptance continues below.

A subsequent root run showed that macOS still lets a global utun `/1` outrank
an interface-scoped physical `/0` during `IP_BOUND_IF` lookup. The safety gate
correctly rejected startup and rolled back. Planning now installs equally
specific physical scoped `/1` routes before the global utun `/1` routes, and
persists their scope for exact reverse cleanup. Automated tests cover scoped
IPv4/IPv6 add, lookup, verification, deletion, multi-interface coexistence,
post-install reproof, state round-trip, and reverse rollback ordering.

The first `kill -9` acceptance run exposed the corresponding cleanup case:
closing the utun descriptor immediately removes its global routes, after which
an unscoped lookup can select an equally specific physical scoped `/1`.
Cleanup initially stopped rather than risk deleting that physical route. It
now confirms the recorded utun no longer exists, treats the recorded global
route as already absent, and continues reverse cleanup without mutating the
replacement. Recovery state remains durable on any unrelated inspection
failure.

## Root acceptance

Use a disposable network session with console access. Set:

```yaml
capture:
  default_route: true
```

Before starting, record the IPv4 and IPv6 route tables and system DNS. Run
`sudo ./bin/tun-proxy check -config ./configs/config.yaml`; it must either print
a complete valid summary or refuse before mutation with the exact endpoint,
gateway, and interface that could not be proven.

After `run` becomes ready, verify:

- `route -n get 1.1.1.1` selects utun for an unlisted literal destination.
- `route -n get -ifscope en0 1.1.1.1` and the equivalent lookup for every
  configured direct interface select that physical interface and gateway.
- Each configured upstream DNS address has an exact host route through its
  recorded physical gateway.
- TCP and connected UDP to literal IPv4 destinations follow protocol/port or
  default policy without a Fake IP mapping.
- When native IPv6 is available, repeat with an IPv6 literal and confirm the
  two IPv6 split routes.
- Packet capture on utun shows ingress, while the selected physical interface
  shows egress; physical egress must not re-enter utun.
- A conflicting pre-existing host route makes `check` and `run` refuse before
  adding any route.
- Ctrl-C removes all four (or two IPv4-only) split routes and all owned bypass
  routes while preserving the original defaults byte-for-byte.
- After `kill -9`, `cleanup` performs the same conditional reverse rollback and
  leaves no state, status socket, lock, listener, or utun.

## Recorded root acceptance results

On 2026-08-18, the IPv4 and IPv6 global `/1` lookups selected `utun4`, while
scoped lookups on both `en0` and `en7` selected the matching physical `/1` and
gateway. Literal IPv4 and IPv6 HTTP requests completed without Fake IP lookup;
Fake DNS returned stable A/AAAA mappings and a mapped domain request completed.
The observed `1.0.0.1:443` timeout reproduced while bypassing utun on both
physical interfaces, isolating it from the proxy data path.

Ctrl-C restored both family defaults, DNS, listeners, and all runtime files.
The first `kill -9` cleanup safely stopped on the equal-prefix lookup described
above and retained recovery state. After the missing-interface fix, cleanup
resumed that same state successfully, restored both defaults and DNS, and left
no utun, route, state, socket, lock, or listener. Route-table diffs contained
only macOS ARP/NDP/cloned-host cache churn.

Status: automated coverage plus dual-stack routing, literal TCP forwarding,
normal shutdown, and crash recovery acceptance are complete. A literal UDP/53
query to unlisted `8.8.4.4` returned successfully with zero UDP failures or
fallbacks. Simultaneous captures showed the original
`10.255.0.2 -> 8.8.4.4` datagram pair on `utun4` and the interface-bound
`192.168.115.172 -> 8.8.4.4` pair on `en0`, with zero kernel capture drops.

A temporary exact host route for `203.0.113.53` through the en0 gateway was
then injected into a disposable configuration. Both `check` and `run` refused
it before creating recovery state. Removing the owned test route restored the
ordinary en7 default and the runtime directory remained empty.

Status: Phase 8.4 automated and strict root acceptance are complete.
