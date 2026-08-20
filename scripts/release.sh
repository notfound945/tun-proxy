#!/bin/bash

set -euo pipefail

repository_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repository_dir"

go_command=${GO:-go}
remote=${REMOTE:-origin}
dist_dir=${DIST_DIR:-$repository_dir/dist}
cgo_enabled=${CGO_ENABLED:-0}
semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'

usage() {
  cat <<'USAGE'
Usage:
  ./scripts/release.sh package [vMAJOR.MINOR.PATCH]
  ./scripts/release.sh publish [vMAJOR.MINOR.PATCH]

Commands:
  package  Run checks and create both macOS archives plus SHA256SUMS in dist/.
  publish  Package, push the annotated tag, and create/update the GitHub Release.

If the tag is omitted, HEAD must point exactly at an annotated SemVer tag.
The source is exported from the tag, so uncommitted working-tree files are not
included in release artifacts.

Optional environment variables:
  GO=go
  CGO_ENABLED=0
  DIST_DIR=/absolute/path
  REMOTE=origin
  GH_REPO=owner/repository
USAGE
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

mode=${1:-package}
case $mode in
  -h|--help)
    usage
    exit 0
    ;;
  package|publish)
    if (( $# > 0 )); then
      shift
    fi
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

if (( $# > 1 )); then
  usage >&2
  exit 2
fi

tag=${1:-}
if [[ -z $tag ]]; then
  tag=$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null) || \
    fail 'HEAD must point exactly at an annotated SemVer tag, or pass a tag explicitly'
fi

[[ $tag =~ $semver_re ]] || fail "tag '$tag' is not a valid semantic version"
git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null || fail "tag '$tag' does not exist locally"
[[ $(git cat-file -t "refs/tags/$tag") == tag ]] || \
  fail "tag '$tag' is lightweight; releases require an annotated tag"

version=${tag#v}
commit=$(git rev-parse --short=12 "${tag}^{commit}")
full_commit=$(git rev-parse "${tag}^{commit}")
build_time=$(git for-each-ref --format='%(taggerdate:iso8601-strict)' "refs/tags/$tag")
[[ -n $build_time ]] || fail "annotated tag '$tag' has no tagger timestamp"

require_command "$go_command"
require_command make
require_command tar
require_command shasum
require_command file

case $dist_dir in
  ''|/) fail 'DIST_DIR must not be empty or /' ;;
esac
if [[ $dist_dir != /* ]]; then
  dist_dir=$repository_dir/$dist_dir
fi
mkdir -p "$dist_dir"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
source_dir=$workdir/source
mkdir -p "$source_dir"
git archive "$tag" | tar -x -C "$source_dir"

printf 'checking source from %s (%s)...\n' "$tag" "$commit"
(
  cd "$source_dir"
  make check
)

archives=()
for arch in arm64 amd64; do
  package_name="tun-proxy_${version}_darwin_${arch}"
  package_dir="$dist_dir/$package_name"
  archive="$dist_dir/$package_name.tar.gz"

  rm -rf "$package_dir"
  rm -f "$archive"
  mkdir -p "$package_dir"

  ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.buildTime=$build_time"
  (
    cd "$source_dir"
    GOOS=darwin GOARCH="$arch" CGO_ENABLED="$cgo_enabled" "$go_command" build \
      -buildvcs=false \
      -trimpath \
      -ldflags "$ldflags" \
      -o "$package_dir/tun-proxy" \
      ./cmd/tun-proxy
  )

  cp "$source_dir/README.md" "$package_dir/"
  cp "$source_dir/configs/example.yaml" "$package_dir/config.example.yaml"

  case $arch in
    arm64) file_arch=arm64 ;;
    amd64) file_arch=x86_64 ;;
  esac
  file "$package_dir/tun-proxy" | grep -F "$file_arch" >/dev/null || \
    fail "built binary does not have the expected $arch architecture"

  case $(uname -m) in
    arm64) host_arch=arm64 ;;
    x86_64) host_arch=amd64 ;;
    *) host_arch=unknown ;;
  esac
  if [[ $arch == "$host_arch" ]]; then
    expected="tun-proxy $version (commit $commit, built $build_time)"
    actual=$("$package_dir/tun-proxy" version)
    [[ $actual == "$expected" ]] || fail "unexpected version output: $actual"
  fi

  COPYFILE_DISABLE=1 tar -czf "$archive" -C "$dist_dir" "$package_name"
  archives+=("$archive")
  printf 'packaged %s\n' "$archive"
done

checksum_file=$dist_dir/SHA256SUMS
rm -f "$checksum_file"
(
  cd "$dist_dir"
  for archive in "${archives[@]}"; do
    LC_ALL=C shasum -a 256 "$(basename "$archive")"
  done > SHA256SUMS
)
printf 'created %s\n' "$checksum_file"
cat "$checksum_file"

if [[ $mode == package ]]; then
  printf 'release package complete for %s\n' "$tag"
  exit 0
fi

require_command gh
gh auth status >/dev/null

printf 'pushing annotated tag %s to %s...\n' "$tag" "$remote"
git push "$remote" "refs/tags/$tag"
remote_commit=$(git ls-remote --tags "$remote" "refs/tags/${tag}^{}" | awk 'NR == 1 { print $1 }')
[[ $remote_commit == "$full_commit" ]] || \
  fail "remote tag '$tag' does not resolve to $full_commit"

assets=("${archives[@]}" "$checksum_file")
if gh release view "$tag" >/dev/null 2>&1; then
  gh release upload "$tag" "${assets[@]}" --clobber
  printf 'updated GitHub Release %s\n' "$tag"
else
  version_without_build=${version%%+*}
  if [[ $version_without_build == *-* ]]; then
    gh release create "$tag" "${assets[@]}" \
      --verify-tag \
      --title "$tag" \
      --notes-from-tag \
      --prerelease
  else
    gh release create "$tag" "${assets[@]}" \
      --verify-tag \
      --title "$tag" \
      --notes-from-tag
  fi
  printf 'published GitHub Release %s\n' "$tag"
fi
