# Phase 8.3 IPv6 root acceptance

Phase 8.3 uses a runtime safety gate. `fake_ipv6` may remain configured on a
host without native IPv6, but the process falls back to IPv4-only operation:
it does not configure IPv6 on utun, does not install the Fake IPv6 route, and
returns NODATA for AAAA. The prepared IPv6 pool is still restored, persisted,
and reported. Restarting on a network with a usable non-link-local IPv6 address
and IPv6 default route enables the complete path.

## Current host: fallback acceptance

The checked-in `configs/config.yaml` contains the paired `tun.ipv6_*` and
`fake_ipv6` fields. Build and start in terminal A:

```sh
go build -o ./bin/tun-proxy ./cmd/tun-proxy
sudo ./bin/tun-proxy check -config ./configs/config.yaml
sudo ./bin/tun-proxy run -config ./configs/config.yaml
```

Because the current `en0` and `en7` only have link-local IPv6, both commands
must report a warning containing:

```text
IPv6 data path disabled: configured outbound interfaces have no usable non-link-local IPv6 address
```

The process must continue running normally. In terminal B:

```sh
tun_name="$(sudo jq -r .tun_name /var/run/tun-proxy/state.json)"

dig +short example.com A
dig +short example.com AAAA
ifconfig "$tun_name" | sed -n '/inet /p;/inet6 /p'

sudo jq '{phase,tun_name,route,routes}' /var/run/tun-proxy/state.json
sudo ./bin/tun-proxy status -json |
  jq '{ipv6,tun,tcp,udp,dns,fake_ip,fake_ipv6,limits}'

curl -4 --max-time 45 https://example.com/
```

Expected results:

- A returns an address inside `198.18.0.0/15`; AAAA is empty.
- utun has the IPv4 pair but no configured ULA IPv6 pair.
- the recovery state has the IPv4 `route` and no additional `routes` entry.
- status reports `ipv6.configured=true`, `ipv6.enabled=false`, and a non-empty
  `ipv6.fallback_reason`.
- `dns.FakeIPv6Answers` stays zero and IPv4 HTTPS continues to work.

Stop terminal A with Ctrl-C, then verify the normal IPv4 rollback:

```sh
ifconfig "$tun_name" 2>&1 || true
sudo lsof -nP -iUDP:53 -iTCP:53
sudo test ! -e /var/run/tun-proxy/state.json && echo "PASS state removed"
sudo test ! -e /var/run/tun-proxy/state.json.sock && echo "PASS socket removed"
sudo test ! -e /var/run/tun-proxy/tun-proxy.lock && echo "PASS lock removed"
sudo stat -f '%Sp %Su:%Sg %N' /var/lib/tun-proxy/fake-ipv6.yaml
```

The old utun and local DNS listener must disappear, state/socket/lock files
must be absent, and the prepared IPv6 persistence file must remain
`-rw------- root:wheel`.

### Recorded fallback result

On 2026-08-18, the current macOS host passed the fallback acceptance with
`en0` and `en7` exposing only link-local IPv6. Both `check` and `run` reported
the expected capability warning, while the process continued on `utun7` in
IPv4-only mode. An A query returned `198.18.0.37`, the AAAA query returned
NODATA, and IPv4 HTTPS returned the Example Domain page.

Runtime status reported `ipv6.configured=true`, `ipv6.enabled=false`, the exact
non-link-local-address fallback reason, 22 Fake IPv4 answers, zero Fake IPv6
answers, 16 NODATA answers, and zero DNS failures or capacity rejects. The
prepared IPv6 pool remained unused with no active mappings or references. TUN
processed traffic without malformed packets, IPv6 drops, read/write errors, or
handler errors.

After Ctrl-C, tun-proxy stopped cleanly, `utun7` disappeared, port 53 had no
remaining listener, and the recovery state, status socket, and process lock
were absent. Both `/var/lib/tun-proxy/fake-ip.yaml` and
`/var/lib/tun-proxy/fake-ipv6.yaml` remained `0600 root:wheel`. The fallback
gate is accepted. Complete native IPv6 forwarding remains pending until a host
network with a non-link-local IPv6 address and IPv6 default route is available.

## Native IPv6 host: complete path acceptance

On a later host or network with a non-link-local IPv6 address and IPv6 default
route, restart tun-proxy with the same configuration. The warning must be
absent. Then run:

```sh
fake6="$(dig +short example.com AAAA | head -1)"
tun_name="$(sudo jq -r .tun_name /var/run/tun-proxy/state.json)"

printf 'fake6=%s tun=%s\n' "$fake6" "$tun_name"
ifconfig "$tun_name" | sed -n '/inet6/p'
route -n get -inet6 "$fake6"
sudo ./bin/tun-proxy status -json | jq '{ipv6,dns,fake_ipv6,tcp,udp}'
curl -6 --max-time 45 https://example.com/
```

Expected results:

- the Fake AAAA is inside `fd00:7475:6e70::/96`;
- utun has `fd00:7475:6e70:ffff::2` and the Fake IPv6 route selects it;
- status reports `ipv6.configured=true`, `ipv6.enabled=true`;
- `dns.FakeIPv6Answers` increases and public IPv6 TCP succeeds.

After Ctrl-C, the Fake IPv6 route and utun must disappear. The implementation
does not provide NAT64, so public IPv6 success remains gated by the physical
network.
