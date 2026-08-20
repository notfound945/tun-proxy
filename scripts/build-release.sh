#!/bin/bash

set -euo pipefail

repository_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repository_dir"

output=${1:-bin/tun-proxy}
go_command=${GO:-go}
semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'

tag=$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null) || {
  echo "error: HEAD must have an exact vMAJOR.MINOR.PATCH tag for a release build" >&2
  exit 1
}
if [[ ! "$tag" =~ $semver_re ]]; then
  echo "error: tag '$tag' is not a valid semantic version" >&2
  exit 1
fi
if [[ $(git cat-file -t "refs/tags/$tag") != tag ]]; then
  echo "error: tag '$tag' is lightweight; release builds require an annotated tag" >&2
  exit 1
fi

version=${tag#v}
commit=$(git rev-parse --short=12 "${tag}^{commit}")
build_time=$(git for-each-ref --format='%(taggerdate:iso8601-strict)' "refs/tags/$tag")
if [[ -z "$build_time" ]]; then
  echo "error: annotated tag '$tag' has no tagger timestamp" >&2
  exit 1
fi

mkdir -p "$(dirname -- "$output")"
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.buildTime=$build_time"
"$go_command" build \
  -buildvcs=false \
  -trimpath \
  -ldflags "$ldflags" \
  -o "$output" \
  ./cmd/tun-proxy

printf 'built %s from %s (commit %s)\n' "$output" "$tag" "$commit"
