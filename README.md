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
二进制，并保持服务原来的运行或停止状态。升级前已就绪的服务会重启并验证新版本；原本未运行
或未就绪的服务会保持 stopped/unloaded，不会在升级过程中尝试启动。用户配置和已安装的托管
配置默认都会保留。

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
安装到托管服务的固定目录并立即启动服务，但默认不会设置开机自启。如需允许开机自启，使用：

```sh
sudo tun-proxy service install -start-at-boot=true
```

安装后可以在 Finder 中显示用户配置，或控制服务：

```sh
tun-proxy config -finder
sudo tun-proxy service stop
sudo tun-proxy service start
sudo tun-proxy service restart
sudo tun-proxy service reload
sudo tun-proxy service logs -follow
```

`service stop` 会禁用并卸载 launchd job，避免 `KeepAlive` 在停止后继续重启进程；安装文件和
配置仍会保留，执行 `service start` 即可重新 enable、bootstrap 并启动。日志文件不会因停止而
清空，但停止成功后不应继续产生新的服务日志。

## 清理残留 DNS 与 Fake IP 持久化映射

普通 `cleanup` 会根据状态文件精确恢复 tun-proxy 记录的原 DNS 和路由：

```sh
sudo tun-proxy cleanup
```

如果状态文件已经丢失，但某个已启用的 macOS 网络服务仍然只使用配置中的
`dns.listen` 地址，可以使用保守兜底清理，将它重置为自动/DHCP DNS：

```sh
sudo tun-proxy cleanup -clear-dns \
  -config ~/.config/tun-proxy/config.yaml
```

`-clear-dns` 只有在服务的完整 DNS 列表恰好等于 `dns.listen` 地址时才会修改该服务；
手动 DNS、混合 DNS 列表以及已经被其他程序修改的 DNS 都不会被覆盖。

停止前台实例或托管服务后，可以通过配置文件定位并删除 IPv4/IPv6 Fake IP 快照及 WAL：

```sh
sudo tun-proxy cleanup -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
```

两个清理项也可以同时执行：

```sh
sudo tun-proxy cleanup -clear-dns -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
```

两个 clear flag 都会先执行系统状态恢复，并共用一次实例锁；如果仍有实例正在启动或运行，
命令会拒绝执行兜底 DNS 重置或删除 Fake IP 数据。

## 快速配置

### 1. 查看当前网口

配置出口前先查看这台 Mac 实际存在的网口：

```sh
tun-proxy interfaces
```

不同 Mac 的网口名称可能不同，不要直接照搬示例中的 `en0`、`en7`。每个
`outbounds.*.interface` 都应填写当前存在且可用的网口。

### 2. 打开用户配置

默认用户配置位于：

```text
~/.config/tun-proxy/config.yaml
```

安装脚本会在文件不存在时生成默认配置。也可以手动生成或在 Finder 中显示配置：

```sh
tun-proxy config -generate
tun-proxy config -finder
```

已有配置时，`config -generate` 不会覆盖；确实需要恢复内置模板时使用
`tun-proxy config -generate -force`。

### 3. 配置出口和规则

下面是双网口分流示例。请先把 `interface` 修改为当前 Mac 的真实网口：

```yaml
dns:
  default_outbound: wired

outbounds:
  wifi:
    type: direct
    interface: en0
    dns_source: dhcp
    dns:
      - "1.1.1.1:53"
    fallback: reject

  wired:
    type: direct
    interface: en7
    dns_source: dhcp
    dns:
      - "9.9.9.9:53"
    fallback: reject

  reject:
    type: reject

rules:
  - domain_suffix:
      - cursor.sh
      - claude.ai
      - claude.com
      - usefathom.com
      - anthropic.com
      - claudeusercontent.com
      - claudemcpcontent.com
      - claudemcpclient.com
      - intercomcdn.com
      - snapcraft.io
      - intercom.io
      - datadoghq.com
    outbound: wifi

  - outbound: wired
```

配置时注意：

- `outbounds` 中的 `wifi`、`wired` 是自定义出口名称，不要求和系统网口同名。
- `interface` 才是实际网口，例如 `en0`、`en7`。
- `dns_source: dhcp` 优先使用该网口的 DHCP DNS，配置的 `dns` 是必要的回退地址；
  使用 `static` 时始终使用配置中的 DNS。
- `rules` 按 YAML 顺序匹配，首条匹配规则生效，最后必须保留一条仅包含 `outbound` 的
  默认规则。
- `domain` 只匹配完整域名；`domain_suffix: claude.ai` 同时匹配 `claude.ai` 及
  `downloads.claude.ai` 等子域名。
- 命中任意显式 `domain` / `domain_suffix` 规则的域名会获得 Fake IP；未被域名规则选中的
  普通域名通过 `dns.default_outbound` 的 DNS（`dns_source: dhcp` 时优先使用 DHCP DNS）
  返回真实 IP。`fake_ip.exclude` 始终优先使用真实 DNS。
- 规则还可以使用 `ip_cidr`，并与 `domain` / `domain_suffix` 组合；同一规则中的不同条件需要
  同时满足。DNS 阶段尚未获得真实目标 IP，因此纯 `ip_cidr` 规则不会单独触发 Fake IP。
- `capture.default_route: false` 时，拿到真实 IP 的普通流量和 literal-IP 流量不会进入
  TUN；如需让纯 `ip_cidr` 规则覆盖这类流量，必须启用 `capture.default_route: true`。
  但 `domain` / `domain_suffix` 与 `ip_cidr` 的组合规则不要求开启：显式域名条件会先让
  该域名获得 Fake IP，流量进入 TUN 后再判断真实解析地址是否命中 CIDR。
- 规则不再支持 `protocol` 和 `dst_port`；配置中保留这两个字段会被严格 YAML 校验拒绝。

### 4. 校验并确认规则

修改后先做离线校验：

```sh
tun-proxy config validate
```

该命令检查 YAML、字段约束、出口引用、fallback 循环和规则结构，但不会确认当前网口是否
在线。可以使用 `explain` 验证指定流量会命中哪个出口：

```sh
tun-proxy explain \
  -domain downloads.claude.ai
```

### 5. 应用配置

首次安装服务时，`service install` 会把用户配置复制到托管目录并启动服务：

```sh
sudo tun-proxy service install
```

服务安装后，CLI 用户配置和 LaunchDaemon 托管配置是两份独立文件。再次修改
`~/.config/tun-proxy/config.yaml` 后，使用下面的命令完成再次校验、事务同步和热重载：

```sh
sudo tun-proxy service reload -user-config
```

`-user-config` 会根据 `SUDO_USER` 自动选择调用 `sudo` 的用户配置，无需填写完整路径。
需要应用其他配置文件时使用：

```sh
sudo tun-proxy service reload \
  -config "/path/to/config.yaml"
```

同步目标固定为 `/Library/Application Support/tun-proxy/config.yaml`。不可热重载的修改会
在同步前被拒绝；运行时拒绝、超时或配置摘要不一致时，会回滚托管配置并恢复原有运行
配置。不带 `-config` 或 `-user-config` 的 `service reload` 只重读现有托管配置，不会自动
复制用户配置。

## 文档

- [本地开发、编译、安装与使用](docs/build-and-install.md)
- [CLI 命令与参数](docs/cli-flags.md)
- [`config.yaml` 完整配置参考](docs/config-reference.md)
- [设计与实施计划](docs/plans/PLAN.md)
- [阶段状态与剩余工作](docs/plans/STATUS.md)
- [分阶段验收记录](docs/phases/)
