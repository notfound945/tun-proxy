# tun-proxy

`tun-proxy` 是一个仅支持 macOS、正在持续开发的 Go TUN 代理。它使用 Fake DNS、
Fake IP 路由和用户态网络栈，为每个 TCP 或 UDP 流选择指定的物理出口网卡。

## 快速安装

下载仓库中的独立安装脚本，即可安装 GitHub 上的最新 Release：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/install-release.sh | bash
```

脚本会自动识别 Apple Silicon/Intel Mac，下载对应的 Release 压缩包和
`SHA256SUMS`，完成 SHA-256 校验后，将二进制安装到
`/usr/local/bin/tun-proxy`。它还会创建权限为 `0700` 的
`~/.config/tun-proxy`，并在配置不存在时生成默认配置；已有普通配置文件会被保留。
脚本只安装 CLI 和用户配置，不会自动安装或启动系统服务。

安装指定版本：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/install-release.sh | \
  bash -s -- v1.2.3
```

也可以从 [GitHub Releases](https://github.com/notfound945/tun-proxy/releases)
手动下载压缩包和 `SHA256SUMS`，放在同一目录后安装本地文件。Apple Silicon
使用 `darwin_arm64`，Intel Mac 使用 `darwin_amd64`：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/install-release.sh | \
  bash -s -- "$HOME/Downloads/tun-proxy_1.2.3_darwin_arm64.tar.gz"
```

## 更新

已通过 Release 安装的用户可以下载独立更新脚本，将 CLI 更新到最新版本：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | bash
```

更新到指定版本或使用本地 Release 压缩包：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  bash -s -- v1.2.3

curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  bash -s -- "$HOME/Downloads/tun-proxy_1.2.3_darwin_arm64.tar.gz"
```

CLI 与托管服务使用两份不同的二进制：

- `/usr/local/bin/tun-proxy` 是终端中直接运行的 CLI；
- `/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy` 是 LaunchDaemon 使用的托管副本。

因此，再次运行快速安装脚本只会替换 CLI，不会自动替换已经加载的托管服务。如果服务已经
安装，不应再次执行 `service install`；否则会因为 launchd label 已加载而收到：

```text
launchd service label is already loaded
```

`update-release.sh` 会先完成与快速安装相同的架构识别、下载和 SHA-256 校验，再检测托管
服务：未安装服务时只更新 CLI；检测到完整安装时使用 `service upgrade` 事务性替换托管
二进制，并保持服务原来的运行或停止状态。用户配置和已安装的托管配置默认都会保留。

需要同时把 `~/.config/tun-proxy/config.yaml` 更新到托管配置时：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  env UPDATE_SERVICE_CONFIG=1 bash
```

如果先后看到以下两个错误：

```text
inspect worker storage "/var/run/tun-proxy/worker": ... no such file or directory
launchd service label is already loaded
```

说明托管服务和 launchd job 已经存在，但易失的 worker 运行目录缺失。此时不要重复执行
`service install`，也不需要 `uninstall -purge`；更新脚本中的 `service upgrade` 会重新准备
托管存储。可同时要求更新后启动服务：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  env START_SERVICE=1 bash
```

也可以不用脚本，先更新 CLI，再手动升级服务：

```bash
sudo tun-proxy service upgrade
sudo tun-proxy service status
```

如状态显示 `running=false`，再执行：

```bash
sudo tun-proxy service start
```

## 快速开始

也可以不执行完整安装，单独从二进制生成默认配置：

```sh
tun-proxy config -generate
```

该命令默认写入 `~/.config/tun-proxy/config.yaml`，已有文件时不会覆盖。明确需要覆盖时
使用 `tun-proxy config -generate -force`。

校验配置、安装托管服务并确认服务正在运行：

```sh
tun-proxy config validate
sudo tun-proxy service install
sudo tun-proxy service status
```

CLI 默认读取 `~/.config/tun-proxy/config.yaml`。`service install` 会把该用户配置事务性
安装到托管服务的固定目录并启动服务。安装后可以在 Finder 中显示用户配置，或控制服务：

```sh
tun-proxy config -finder
sudo tun-proxy service stop
sudo tun-proxy service start
sudo tun-proxy service restart
sudo tun-proxy service reload
sudo tun-proxy service logs -follow
```

## 文档

- [本地开发、编译、安装与使用](docs/build-and-install.md)
- [CLI 命令与参数](docs/cli-flags.md)
- [设计与实施计划](docs/plans/PLAN.md)
- [阶段状态与剩余工作](docs/plans/STATUS.md)
- [分阶段验收记录](docs/phases/)
