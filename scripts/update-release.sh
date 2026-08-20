#!/usr/bin/env bash

set -euo pipefail

repository=${TUN_PROXY_REPOSITORY:-notfound945/tun-proxy}
installer_ref=${TUN_PROXY_INSTALLER_REF:-master}
prefix=${PREFIX:-/usr/local}
bindir=${BINDIR:-${prefix}/bin}
target=${bindir}/tun-proxy
config_dir=${CONFIG_DIR:-${HOME}/.config/tun-proxy}
config_path=${CONFIG_PATH:-${config_dir}/config.yaml}
install_command=${INSTALL:-/usr/bin/install}
sudo_value=${SUDO-sudo}
update_service_config=${UPDATE_SERVICE_CONFIG:-0}
start_service=${START_SERVICE:-0}

service_binary=/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy
service_config='/Library/Application Support/tun-proxy/config.yaml'
service_plist=/Library/LaunchDaemons/cn.notfound945.tun-proxy.plist

usage() {
  cat <<'USAGE'
用法:
  update-release.sh                 更新到 GitHub 上的最新 Release
  update-release.sh v1.2.3          更新到指定 Release
  update-release.sh /path/file.tgz  使用已下载的 Release 压缩包更新

更新过程先替换 /usr/local/bin/tun-proxy。检测到完整的托管服务安装后，
还会执行事务性的 "tun-proxy service upgrade"，不会再次执行 service install。
默认保留用户配置和当前托管配置，并保持服务原来的运行/停止状态。

可选环境变量:
  PREFIX=/usr/local
  BINDIR=$PREFIX/bin
  CONFIG_DIR=$HOME/.config/tun-proxy
  CONFIG_PATH=$CONFIG_DIR/config.yaml
  UPDATE_SERVICE_CONFIG=1  同时使用用户配置更新托管配置
  START_SERVICE=1          更新完成后明确启动托管服务
  SUDO=sudo
  INSTALL=/usr/bin/install
  INSTALL_RELEASE_SCRIPT=/path/install-release.sh
  TUN_PROXY_REPOSITORY=owner/repository
  TUN_PROXY_INSTALLER_REF=master
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
  fail '请以普通用户运行更新脚本，不要使用 sudo 执行整个脚本。'
fi
[[ $(uname -s) == Darwin ]] || fail 'Release 更新脚本仅支持 macOS。'
[[ $update_service_config == 0 || $update_service_config == 1 ]] || \
  fail 'UPDATE_SERVICE_CONFIG 只能是 0 或 1。'
[[ $start_service == 0 || $start_service == 1 ]] || fail 'START_SERVICE 只能是 0 或 1。'
[[ -n $target && $target == /* ]] || fail 'BINDIR 必须是绝对路径。'
[[ -n $config_path && $config_path == /* ]] || fail 'CONFIG_PATH 必须是绝对路径。'

run_privileged() {
  if [[ -n $sudo_value ]]; then
    local sudo_command
    read -r -a sudo_command <<<"$sudo_value"
    "${sudo_command[@]}" "$@"
  else
    "$@"
  fi
}

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

installer=${INSTALL_RELEASE_SCRIPT:-}
if [[ -z $installer ]]; then
  script_source=${BASH_SOURCE[0]:-}
  if [[ -n $script_source && -f $script_source ]]; then
    sibling_installer=$(CDPATH= cd -- "$(dirname -- "$script_source")" && pwd)/install-release.sh
    if [[ -f $sibling_installer ]]; then
      installer=$sibling_installer
    fi
  fi
fi
if [[ -z $installer ]]; then
  require_command curl
  installer=$workdir/install-release.sh
  printf '下载安装程序...\n'
  curl -fL --retry 3 --retry-delay 1 \
    -o "$installer" \
    "https://raw.githubusercontent.com/${repository}/${installer_ref}/scripts/install-release.sh"
fi
[[ -f $installer && ! -L $installer ]] || fail "安装程序不是普通文件: $installer"

old_version=未安装
if [[ -x $target ]]; then
  old_version=$("$target" version 2>/dev/null || printf '未知版本')
fi

printf '当前 CLI: %s\n' "$old_version"
run_installer() {
  TUN_PROXY_REPOSITORY="$repository" \
  PREFIX="$prefix" \
  BINDIR="$bindir" \
  CONFIG_DIR="$config_dir" \
  CONFIG_PATH="$config_path" \
  INSTALL="$install_command" \
  SUDO="$sudo_value" \
  FORCE_CONFIG=0 \
    /bin/bash "$installer" "$@"
}
if (( $# == 1 )); then
  run_installer "$1"
else
  run_installer
fi

[[ -x $target ]] || fail "更新后找不到 tun-proxy: $target"
new_version=$("$target" version)
printf '更新后 CLI: %s\n' "$new_version"

path_exists() {
  [[ -e $1 || -L $1 ]]
}

service_identity_artifacts=0
for path in "$service_binary" "$service_plist"; do
  if path_exists "$path"; then
    service_identity_artifacts=$((service_identity_artifacts + 1))
  fi
done

if (( service_identity_artifacts == 0 )); then
  printf '未检测到托管服务；本次只更新 CLI，后续首次安装可运行：\n'
  printf '  sudo tun-proxy service install\n'
  exit 0
fi
if (( service_identity_artifacts == 1 )) || ! path_exists "$service_config"; then
  fail '检测到不完整的托管服务安装；CLI 已更新，但为避免覆盖异常状态，没有自动升级服务。请先检查 /Library/LaunchDaemons、/Library/PrivilegedHelperTools 和托管配置。'
fi

printf '检测到已安装的托管服务，开始事务升级...\n'
upgrade_args=(service upgrade -binary "$target")
if [[ $update_service_config == 1 ]]; then
  [[ -f $config_path && ! -L $config_path ]] || fail "用户配置不是普通文件: $config_path"
  "$target" config validate -service -config "$config_path"
  upgrade_args+=(-config "$config_path")
fi
run_privileged "$target" "${upgrade_args[@]}"

if [[ $start_service == 1 ]]; then
  run_privileged "$target" service start
fi

printf '更新后的托管服务状态：\n'
run_privileged "$target" service status
printf '更新完成。若状态显示 running=false，可运行：sudo tun-proxy service start\n'
