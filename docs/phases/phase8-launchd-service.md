# Phase 8.7a transactional LaunchDaemon service

Phase 8.7a packages the existing tun-proxy runtime as a system LaunchDaemon
with transactional install, start, stop, status, upgrade, crash recovery, and
uninstall operations. Automated gates are complete. Strict root installation,
forwarding, clean stop/restart, and transactional upgrade acceptance passed on
2026-08-19. Crash recovery and preservation-safe uninstall also passed that
day, completing Phase 8.7a root acceptance.

This historical slice deliberately did **not** claim least privilege. Its
full-root LaunchDaemon was subsequently replaced by the accepted Phase 8.7b
root-supervisor/non-root-worker topology documented in
[`phase8-least-privilege.md`](phase8-least-privilege.md).

## Managed layout

The service uses fixed paths so a root process cannot be redirected by a
mutable configuration to arbitrary state or persistence files:

| Artifact | Path | Mode |
|---|---|---:|
| Executable | `/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy` | `0755` |
| Configuration | `/Library/Application Support/tun-proxy/config.yaml` | `0600` |
| LaunchDaemon plist | `/Library/LaunchDaemons/cn.notfound945.tun-proxy.plist` | `0644` |
| Standard output | `/Library/Logs/tun-proxy/stdout.log` | launchd-created |
| Standard error | `/Library/Logs/tun-proxy/stderr.log` | launchd-created |
| Runtime state | `/var/run/tun-proxy/state.json` | existing runtime contract |
| Runtime lock | `/var/run/tun-proxy/tun-proxy.lock` | existing runtime contract |
| Fake IPv4 map | `/var/lib/tun-proxy/fake-ip.yaml` | `0600` |
| Fake IPv6 map | `/var/lib/tun-proxy/fake-ipv6.yaml` | `0600` |

The binary, configuration, plist, and their managed directories must be owned
by root. Source and installed artifacts must be regular files rather than
symlinks. The installed configuration must retain the exact state, lock, and
Fake IP persistence paths above. Every SIGHUP reload repeats that confinement
check.

## Commands and lifecycle

Build a non-root source binary first, then use the service commands through
`sudo`:

```sh
go build -o ./bin/tun-proxy ./cmd/tun-proxy

sudo ./bin/tun-proxy service install \
  -binary ./bin/tun-proxy \
  -config ./configs/config.yaml \
  -start=true

sudo ./bin/tun-proxy service status -json
sudo ./bin/tun-proxy service stop
sudo ./bin/tun-proxy service start

sudo ./bin/tun-proxy service upgrade \
  -binary ./bin/tun-proxy \
  -config ./configs/config.yaml

sudo ./bin/tun-proxy service uninstall
```

`install` defaults to the current executable and
`~/.config/tun-proxy/config.yaml` (using the invoking `SUDO_USER` home when
run through `sudo`).
`-start=false` installs the files without bootstrapping the job in the current
boot session. Because the plist is installed under `/Library/LaunchDaemons`
with `RunAtLoad=true`, it is eligible to load and start at the next system
boot.

The plist invokes the installed binary directly as `_service-run`; it does not
use a shell. `RunAtLoad=true` starts a registered job, while
`KeepAlive.SuccessfulExit=false` restarts it after a non-successful exit. A
normal `service stop` sends SIGTERM, lets tun-proxy restore its recorded system
state, and exits successfully, so the loaded job remains stopped until an
explicit start or the next boot. A crash is restarted by launchd, and the
hidden service entry point runs the existing stale-state cleanup before the
new process performs preflight.

`upgrade` is a controlled stop-and-restart, not a hot reload: active flows are
allowed to drain within the normal shutdown deadline; the loaded job is
removed; the binary, optional configuration, and regenerated plist are
atomically replaced; and the prior loaded/running state is restored. If the
new version cannot start, its job is removed before the old files and prior
launchd state are restored.

The default `uninstall` removes the plist and executable but preserves the
installed configuration, Fake IP mappings, and logs. `uninstall -purge`
additionally removes only the exact known managed files listed above. Unknown
files are never recursively deleted, and managed directories are removed only
when empty. A later `service install` may atomically replace the preserved
managed configuration, so default uninstall remains directly reinstallable.

## Local authorization boundary

There is no new mutable control socket in Phase 8.7a. Lifecycle commands
require effective UID 0 and operate on the launchd system domain. Runtime
status continues to use the existing root-owned local status socket. This is a
local administrative boundary, not a least-privilege process boundary.

Process-rule configuration also remains unsupported. Moving the Phase 8.6
`libproc` polling probe into a root daemon would not recover an exited
process's historical ownership or resolve a shared UDP `SO_REUSEPORT` owner.

## Automated gates

The automated suite covers:

- valid plist XML and direct fixed program arguments;
- `RunAtLoad`, failure-only restart, timeout, umask, and log paths;
- managed-path confinement and symlink rejection;
- transactional activation, removal, rollback, and residue cleanup;
- install with and without immediate start;
- reinstall after preservation-safe default uninstall;
- failed bootstrap cleanup;
- successful plist-reloading upgrade and failed-upgrade job/file restoration;
- stale runtime recovery;
- default uninstall preservation and exact purge behavior;
- failed-uninstall restoration of both files and prior service state;
- root ownership/mode enforcement and bounded launchctl output.

Run the complete non-root gate with:

```sh
go test -race ./...
go vet ./...
go build ./...
```

## Strict root acceptance

Run this section on macOS. It installs system files and changes live routes and
DNS while the service is running.

### 1. Install, inspect, and forward traffic

```sh
go build -o ./bin/tun-proxy ./cmd/tun-proxy

sudo ./bin/tun-proxy service install \
  -binary ./bin/tun-proxy \
  -config ./configs/config.yaml \
  -start=true

sudo ./bin/tun-proxy service status -json
sudo launchctl print system/cn.notfound945.tun-proxy

sudo stat -f '%Sp %Su:%Sg %N' \
  /Library/PrivilegedHelperTools/cn.notfound945.tun-proxy \
  '/Library/Application Support/tun-proxy/config.yaml' \
  /Library/LaunchDaemons/cn.notfound945.tun-proxy.plist

dig @127.0.0.1 example.com A +short
curl -4 --max-time 45 https://example.com/
```

Expected: `installed`, `loaded`, and `runtime.running` are true; the runtime PID
is nonzero; the three installed files are root-owned with modes `0755`, `0600`,
and `0644`; DNS returns a Fake IPv4 address; HTTPS succeeds.

Recorded 2026-08-19 result: **PASS**. The service was loaded as
`system/cn.notfound945.tun-proxy`, ran the fixed binary and configuration
arguments with PID 18820, exposed the configured stdout/stderr paths, and
reported `installed=true`, `loaded=true`, and `runtime.running=true`. The
binary, configuration, and plist were root-owned with modes `0755`, `0600`,
and `0644` respectively (`root:admin` for the mode-`0600` configuration is
acceptable because group access is absent). DNS returned `198.18.0.37`, and
the HTTPS request to `example.com` succeeded.

### 2. Clean stop and restart

```sh
sudo ./bin/tun-proxy service stop
sudo ./bin/tun-proxy service status -json

for runtime_file in \
  /var/run/tun-proxy/state.json \
  /var/run/tun-proxy/state.json.sock \
  /var/run/tun-proxy/tun-proxy.lock
do
  sudo test ! -e "$runtime_file" && echo "PASS absent: $runtime_file"
done

sudo ./bin/tun-proxy service start
sudo ./bin/tun-proxy service status -json
```

Expected after stop: the service remains installed and loaded, but is not
running, and all three runtime files are absent. Expected after start: it is
running again with a nonzero PID.

Recorded 2026-08-19 result: **PASS**. A clean stop left the service installed
and loaded but not running, with PID 0, phase empty, and all three runtime files
absent. The registered launchd job remained present in `not running` state and
reported last exit code 0. An explicit start restored `running=true` with new
PID 22407; Fake DNS again returned `198.18.0.37`, and HTTPS forwarding to
`example.com` succeeded.

### 3. Transactional upgrade

```sh
old_pid="$(sudo ./bin/tun-proxy service status -json | jq -r .runtime.pid)"

sudo ./bin/tun-proxy service upgrade \
  -binary ./bin/tun-proxy \
  -config ./configs/config.yaml

new_pid="$(sudo ./bin/tun-proxy service status -json | jq -r .runtime.pid)"
printf 'old_pid=%s new_pid=%s\n' "$old_pid" "$new_pid"

test "$old_pid" != "$new_pid" &&
  test "$new_pid" -gt 0 &&
  echo 'PASS upgrade restarted service'
```

Recorded 2026-08-19 result: **PASS**. The controlled upgrade replaced the
installed binary with the locally built candidate (matching SHA-256
`4097115c45f2e6b7aefed2e5a36390a92054c9efee6b705960bc3905a9fba6ca`),
changed the running PID from 22407 to 27631, and retained the fixed executable,
configuration, and log paths in the registered system LaunchDaemon. Fake DNS
again returned `198.18.0.37`, and HTTPS forwarding succeeded after replacement.

### 4. Crash recovery

```sh
old_pid="$(sudo ./bin/tun-proxy service status -json | jq -r .runtime.pid)"
sudo kill -KILL "$old_pid"

phase87_recovered=false
for i in {1..30}; do
  phase87_status="$(sudo ./bin/tun-proxy service status -json 2>/dev/null || true)"
  new_pid="$(jq -r '.runtime.pid // 0' <<<"$phase87_status")"
  running="$(jq -r '.runtime.running // false' <<<"$phase87_status")"
  if [[ "$running" == true && "$new_pid" != "$old_pid" && "$new_pid" != 0 ]]; then
    echo "PASS crash recovery old_pid=$old_pid new_pid=$new_pid"
    phase87_recovered=true
    break
  fi
  sleep 1
done

[[ "$phase87_recovered" == true ]]
```

Recorded 2026-08-19 result: **PASS**. Sending `SIGKILL` to PID 27631 made the
first status sample report not running; launchd restarted the service by the
second sample with PID 30144. The job remained in running state, its run count
increased to 2, and launchd recorded `Killed: 9` as the last terminating
signal. Fake DNS again returned `198.18.0.37`, and HTTPS forwarding succeeded
after recovery.

### 5. Default uninstall and preservation

```sh
sudo ./bin/tun-proxy service uninstall

sudo test ! -e /Library/LaunchDaemons/cn.notfound945.tun-proxy.plist &&
  echo 'PASS plist removed'
sudo test ! -e /Library/PrivilegedHelperTools/cn.notfound945.tun-proxy &&
  echo 'PASS binary removed'

sudo test -e '/Library/Application Support/tun-proxy/config.yaml' &&
  echo 'PASS config preserved'
sudo test -e /var/lib/tun-proxy/fake-ip.yaml &&
  echo 'PASS Fake IPv4 map preserved'

sudo ./bin/tun-proxy service status -json
```

Expected final status: not installed, not loaded, and not running. The
configuration and existing mappings remain available for a later reinstall.
The Fake IP persistence files are flushed during the clean stop, and each
snapshot records a new `saved_at` value. Therefore byte-for-byte hashes are not
expected to remain stable across uninstall; preservation is verified by file
existence, safe ownership/mode, and a loadable snapshot rather than an
unchanged digest.

Recorded 2026-08-19 result: **PASS**. Default uninstall reported that managed
data was preserved and left the service not installed, not loaded, and not
running. The plist and installed binary were removed; the managed
configuration, both Fake IP persistence files, and stdout/stderr logs remained.
The two persistence digests changed because shutdown flushed fresh snapshots,
which is expected due to `saved_at`. Runtime state, status socket, lock, utun7,
the launchd registration, and the tun-proxy DNS listener were all removed.

After preservation has been inspected, optional exact-file cleanup is:

```sh
sudo ./bin/tun-proxy service uninstall -purge
```

Phase 8.7a root acceptance is complete: install/start, clean stop, upgrade,
crash restart, default uninstall preservation, and final system restoration all
passed on 2026-08-19. Phase 8.7b remains pending regardless of this result.
