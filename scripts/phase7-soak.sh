#!/bin/bash

set -u
set -o pipefail

duration_seconds="${1:-86400}"
interval_seconds="${2:-60}"
https_timeout_seconds="${3:-45}"
status_bin="${STATUS_BIN:-./bin/tun-proxy}"
output_file="${SOAK_OUTPUT:-/tmp/tun-proxy-soak-$(date -u +%Y%m%dT%H%M%SZ)-$$.ndjson}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "error: run this monitor with sudo so it can read the root-only status socket" >&2
  exit 1
fi
if ! [[ "${duration_seconds}" =~ ^[1-9][0-9]*$ && "${interval_seconds}" =~ ^[1-9][0-9]*$ && "${https_timeout_seconds}" =~ ^[1-9][0-9]*$ ]]; then
  echo "usage: sudo $0 [duration_seconds] [interval_seconds] [https_timeout_seconds]" >&2
  exit 1
fi
if [[ ! -x "${status_bin}" ]]; then
  echo "error: status binary is not executable: ${status_bin}" >&2
  exit 1
fi
if [[ -e "${output_file}" || -L "${output_file}" ]]; then
  echo "error: soak output must not already exist: ${output_file}" >&2
  exit 1
fi
umask 077
set -o noclobber
if ! exec 3>"${output_file}"; then
  echo "error: securely create soak output: ${output_file}" >&2
  exit 1
fi
set +o noclobber

started=${SECONDS}
samples=0
workload_failures=0
interrupted=""

handle_signal() {
  interrupted="$1"
}

trap 'handle_signal INT' INT
trap 'handle_signal TERM' TERM

while (( SECONDS - started < duration_seconds )); do
  if [[ -n "${interrupted}" ]]; then
    break
  fi
  if ! snapshot="$("${status_bin}" status -json)"; then
    if [[ -n "${interrupted}" ]]; then
      break
    fi
    echo "error: runtime status failed; stopping soak monitor" >&2
    exit 1
  fi
  printf '%s\n' "$(printf '%s' "${snapshot}" | tr -d '\n')" >&3
  samples=$((samples + 1))

  if ! dig +time=5 +tries=1 example.com A >/dev/null 2>&1; then
    if [[ -z "${interrupted}" ]]; then
      workload_failures=$((workload_failures + 1))
      echo "WARN soak workload=dns failed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >&2
    fi
  fi
  if [[ -n "${interrupted}" ]]; then
    break
  fi
  if ! curl --fail --silent --show-error --max-time "${https_timeout_seconds}" --output /dev/null https://example.com/; then
    if [[ -z "${interrupted}" ]]; then
      workload_failures=$((workload_failures + 1))
      echo "WARN soak workload=https failed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ) timeout_seconds=${https_timeout_seconds}" >&2
    fi
  fi
  if [[ -n "${interrupted}" ]]; then
    break
  fi
  sleep "${interval_seconds}"
done

exec 3>&-

if [[ -n "${interrupted}" ]]; then
  echo "STOP soak signal=${interrupted} samples=${samples} workload_failures=${workload_failures} output=${output_file}" >&2
  exit 130
fi
if (( workload_failures != 0 )); then
  echo "FAIL soak samples=${samples} workload_failures=${workload_failures} output=${output_file}" >&2
  exit 1
fi
echo "PASS soak samples=${samples} workload_failures=0 output=${output_file}"
