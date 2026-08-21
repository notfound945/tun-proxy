#!/usr/bin/env bash

set -euo pipefail

repository=${TUN_PROXY_REPOSITORY:-notfound945/tun-proxy}
prefix=${PREFIX:-/usr/local}
bindir=${BINDIR:-${prefix}/bin}
target=${bindir}/tun-proxy
config_dir=${CONFIG_DIR:-${HOME}/.config/tun-proxy}
config_path=${CONFIG_PATH:-${config_dir}/config.yaml}
install_command=${INSTALL:-/usr/bin/install}
sudo_value=${SUDO-sudo}
force_config=${FORCE_CONFIG:-0}
print_next_steps=${PRINT_NEXT_STEPS:-1}
semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'

usage() {
  cat <<'USAGE'
用法:
  install-release.sh                 安装 GitHub 上的最新 Release
  install-release.sh v1.2.3          安装指定 Release
  install-release.sh /path/file.tgz  安装已下载的 Release 压缩包

可选环境变量:
  PREFIX=/usr/local
  BINDIR=$PREFIX/bin
  CONFIG_DIR=$HOME/.config/tun-proxy
  CONFIG_PATH=$CONFIG_DIR/config.yaml
  FORCE_CONFIG=1
  SUDO=sudo
  TUN_PROXY_REPOSITORY=owner/repository
USAGE
}

fail() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少必需命令: $1"
}

if (( $# == 1 )) && [[ $1 == -h || $1 == --help ]]; then
  usage
  exit 0
fi
if (( $# > 1 )); then
  usage >&2
  exit 2
fi

if [[ $(id -u) -eq 0 ]]; then
  fail '请以普通用户运行安装脚本，不要使用 sudo 执行整个脚本。'
fi

[[ $(uname -s) == Darwin ]] || fail 'Release 安装脚本仅支持 macOS。'

case $(uname -m) in
  arm64) release_arch=arm64 ;;
  x86_64) release_arch=amd64 ;;
  *) fail "不支持的 Mac 架构: $(uname -m)" ;;
esac

[[ $force_config == 0 || $force_config == 1 ]] || fail 'FORCE_CONFIG 只能是 0 或 1。'
[[ $print_next_steps == 0 || $print_next_steps == 1 ]] || \
  fail 'PRINT_NEXT_STEPS 只能是 0 或 1。'
[[ -n $prefix && $prefix == /* ]] || fail 'PREFIX 必须是绝对路径。'
[[ -n $bindir && $bindir == /* ]] || fail 'BINDIR 必须是绝对路径。'
[[ -n $config_dir && $config_dir == /* ]] || fail 'CONFIG_DIR 必须是绝对路径。'
[[ -n $config_path && $config_path == /* ]] || fail 'CONFIG_PATH 必须是绝对路径。'

if [[ -L $config_dir || ( -e $config_dir && ! -d $config_dir ) ]]; then
  fail "配置目录不是普通目录: $config_dir"
fi
if [[ -L $config_path || ( -e $config_path && ! -f $config_path ) ]]; then
  fail "配置路径不是普通文件: $config_path"
fi

require_command tar
require_command shasum
[[ -x $install_command ]] || fail "找不到可执行的 install: $install_command"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

source_value=${1:-}
checksum_file=
if [[ -n $source_value && -f $source_value ]]; then
  archive=$(cd "$(dirname "$source_value")" && pwd)/$(basename "$source_value")
  asset=$(basename "$archive")
  checksum_candidate=$(dirname "$archive")/SHA256SUMS
  if [[ -f $checksum_candidate ]]; then
    checksum_file=$checksum_candidate
  else
    printf '警告: 压缩包旁没有 SHA256SUMS，将跳过完整性校验: %s\n' "$archive" >&2
  fi
else
  require_command curl

  if [[ -z $source_value ]]; then
    latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
      "https://github.com/${repository}/releases/latest")
    latest_url=${latest_url%/}
    tag=${latest_url##*/}
  else
    tag=$source_value
  fi

  [[ $tag =~ $semver_re ]] || fail "无效的 Release 版本: $tag"
  version=${tag#v}
  asset="tun-proxy_${version}_darwin_${release_arch}.tar.gz"
  archive=$workdir/$asset
  checksum_file=$workdir/SHA256SUMS
  download_base="https://github.com/${repository}/releases/download/${tag}"

  printf '下载 tun-proxy %s (%s)...\n' "$tag" "$release_arch"
  curl -fL --retry 3 --retry-delay 1 -o "$archive" "$download_base/$asset"
  curl -fL --retry 3 --retry-delay 1 -o "$checksum_file" "$download_base/SHA256SUMS"
fi

asset_re='^tun-proxy_([^/]+)_darwin_(arm64|amd64)\.tar\.gz$'
[[ $asset =~ $asset_re ]] || fail "压缩包名称不符合 Release 约定: $asset"
archive_version=${BASH_REMATCH[1]}
archive_arch=${BASH_REMATCH[2]}
[[ v$archive_version =~ $semver_re ]] || fail "压缩包版本不是有效的语义化版本: $archive_version"
[[ $archive_arch == "$release_arch" ]] || \
  fail "压缩包架构为 $archive_arch，当前 Mac 需要 $release_arch"

if [[ -n $checksum_file ]]; then
  expected_checksum=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$checksum_file")
  [[ $expected_checksum =~ ^[0-9A-Fa-f]{64}$ ]] || \
    fail "SHA256SUMS 中没有 $asset 的唯一有效校验值"
  actual_checksum=$(shasum -a 256 "$archive" | awk '{ print $1 }')
  expected_checksum=$(printf '%s' "$expected_checksum" | tr '[:upper:]' '[:lower:]')
  actual_checksum=$(printf '%s' "$actual_checksum" | tr '[:upper:]' '[:lower:]')
  [[ $actual_checksum == "$expected_checksum" ]] || fail "SHA-256 校验失败: $asset"
  printf 'SHA-256 校验通过: %s\n' "$asset"
fi

package_dir="tun-proxy_${archive_version}_darwin_${archive_arch}"
member="$package_dir/tun-proxy"
member_count=$(tar -tzf "$archive" | awk -v member="$member" '$0 == member { count++ } END { print count + 0 }')
[[ $member_count -eq 1 ]] || fail '压缩包中没有唯一的预期 tun-proxy 二进制。'

tar -xzf "$archive" -C "$workdir" "$member"
binary=$workdir/$member
[[ -f $binary && ! -L $binary ]] || fail '解压得到的 tun-proxy 不是普通文件。'
chmod 0755 "$binary"
version_output=$("$binary" version)
printf '%s\n' "$version_output"
case $version_output in
  "tun-proxy $archive_version ("*) ;;
  *) fail "二进制版本与压缩包不一致: 期望 $archive_version" ;;
esac

run_privileged() {
  if [[ -n $sudo_value ]]; then
    local sudo_command
    read -r -a sudo_command <<<"$sudo_value"
    "${sudo_command[@]}" "$@"
  else
    "$@"
  fi
}

run_privileged "$install_command" -d -m 0755 "$bindir"
run_privileged "$install_command" -m 0755 "$binary" "$target"
"$install_command" -d -m 0700 "$config_dir"

if [[ -f $config_path && $force_config != 1 ]]; then
  chmod 0600 "$config_path"
  printf '保留现有配置: %s\n' "$config_path"
elif [[ $force_config == 1 ]]; then
  "$target" config -generate -force -config "$config_path"
else
  "$target" config -generate -config "$config_path"
fi

printf '安装完成: %s\n' "$target"
printf '用户配置: %s\n' "$config_path"
if [[ $print_next_steps == 1 ]]; then
  printf '下一步可运行：\n'
  printf '  tun-proxy config validate\n'
  printf '  sudo tun-proxy service install\n'
  printf '  sudo tun-proxy service start  # 服务未运行时\n'
fi
