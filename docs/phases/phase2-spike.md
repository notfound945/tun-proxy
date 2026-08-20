# Phase 2 utun and route spike

Status: **PASS**

Validated on 2026-08-18 with macOS 15.7.7/arm64. The probe created `utun7`,
verified the Fake IP route, received three generated IPv4 UDP packets (87
bytes), removed the route, closed the device, and removed recovery state.

The Phase 2 probe creates and configures a temporary utun, records recovery
state before installing `198.18.0.0/15`, verifies the route resolves to that
utun, counts packets, and then removes the route before closing the device. It
never changes system DNS.

The disposable Phase 2 command was removed after its utun, route, packet-pump,
and recovery behavior was integrated into the production runtime. This file is
an acceptance record rather than a runnable procedure. Current validation must
use the production `tun-proxy` lifecycle, status, cleanup, and automated tests.
