# tun-proxy 编译、安装与使用

本文档说明从源码检查、编译，到安装系统 CLI 和 macOS LaunchDaemon 的完整流程。
全部命令都应在仓库根目录执行。完整命令与参数参考见
[cli-flags.md](cli-flags.md)。

## 1. 环境要求

- macOS。生产环境的 TUN、路由、DNS、launchd 和接口绑定代码仅支持 Darwin。
- 与 `go.mod` 要求兼容的 Go 工具链。
- Xcode Command Line Tools 提供的 `make`、编译工具和常用系统命令。
- 可通过 `sudo` 安装系统二进制和管理服务的管理员账号。
- 至少一个可用的物理出口接口，以及能够通过该接口访问的 DNS 服务器。

检查本机环境：

```sh
sw_vers
uname -m
go version
make --version
```

## 2. Makefile 常用目标

查看所有常用目标：

```sh
make help
```

主要目标如下：

| 命令 | 用途 |
| --- | --- |
| `make` 或 `make build` | 编译版本号为 `local` 的 `./bin/tun-proxy` |
| `make build-release` | 从当前 annotated SemVer tag 编译发布版本 |
| `make deps` | 下载 Go 模块依赖 |
| `make test` | 运行单元测试 |
| `make test-race` | 运行竞态测试 |
| `make vet` | 运行 `go vet` |
| `make fmt` | 格式化 Go 源码 |
| `make check` | 依次执行格式检查、测试和 `go vet` |
| `make install` | 先执行 `build-release`，再安装发布版 CLI 和用户配置 |
| `make system-proxy-check` | 只读检查 tun-proxy 是否仍在接管系统网络 |
| `make system-proxy-clean` | 停止服务并恢复已记录的 DNS 和路由状态 |
| `make clean` | 删除本地编译产物 |

Makefile 主要提供源码开发、检查和编译目标；`make install` 是唯一的系统安装目标。
两个 `system-proxy-*` 目标用于本机网络接管状态的检查与安全恢复，内部仍统一调用
`tun-proxy` CLI，不会直接删除未知路由或重置未记录的 DNS 设置。

## 3. 下载依赖并检查源码

安装前运行：

```sh
make deps
make check
```

竞态测试耗时更长，建议在发布或重要变更后额外执行：

```sh
make test-race
```

## 4. 编译

```sh
make build
```

产物位于：

```text
./bin/tun-proxy
```

确认二进制和帮助系统可用：

```sh
./bin/tun-proxy -version
./bin/tun-proxy help
./bin/tun-proxy help service install
```

`make build` 始终生成版本号为 `local` 的开发二进制。`version`、`-version` 和
`--version` 都可以查看版本信息；本地构建的 Commit 和构建时间显示为 `unknown`，
不影响运行行为。

在当前 `HEAD` 已打 annotated SemVer tag（例如 `v1.2.3`）时，可以构建发布版本：

```sh
make build-release
./bin/tun-proxy -version
```

发布构建会去掉 tag 的 `v` 前缀并注入版本号，同时注入 tag 指向的 12 位 Commit 和
annotated tag 的时间。当前提交没有精确 tag、tag 不符合 SemVer，或使用轻量 tag 时，
构建会直接失败，避免生成版本来源不明确的发布二进制。

### 本地打包并发布 GitHub Release

Release 不再由 GitHub Actions 打包或发布。原 workflow 保留在
`.github/workflows/release.yml.disabled` 作为历史实现；由于文件不以 `.yml` 或 `.yaml`
结尾，GitHub 不会加载或执行它。

本地发布脚本要求 annotated SemVer tag。先提交变更、创建 tag，并把分支提交推送到远端：

```sh
git tag -a v1.2.3 -m "Release v1.2.3"
git push origin HEAD
```

只在本地检查和打包，不发布：

```sh
./scripts/release.sh package
```

脚本会从 tag 导出干净源码，执行 `make check`，以 `CGO_ENABLED=0` 分别交叉编译 Apple
Silicon 和 Intel macOS 二进制，并在 `dist/` 生成：

```text
tun-proxy_1.2.3_darwin_arm64.tar.gz
tun-proxy_1.2.3_darwin_amd64.tar.gz
SHA256SUMS
```

确认产物后，本地打包并发布 GitHub Release：

```sh
gh auth login
./scripts/release.sh publish
```

`publish` 会重新执行完整检查和打包，随后推送 annotated tag，并通过 `gh` 创建 GitHub
Release；如果对应 Release 已存在，则覆盖更新上述资产。tag 参数默认取当前 `HEAD` 的精确
tag，也可以显式指定，例如 `./scripts/release.sh package v1.2.3`。预发布版本可以使用
`v1.2.3-rc.1`，发布时会自动标记为 prerelease。轻量 tag、`v1.2` 等非完整语义化版本会被
拒绝，annotated tag 的说明会作为 Release notes。

## 5. 快速安装到系统

运行快速安装脚本：

```sh
./scripts/install.sh
```

脚本等价于：

```sh
make install
```

`make install` 会先执行 `make build-release`，再把生成的发布二进制安装到系统路径。
因此当前 `HEAD` 必须精确指向 annotated SemVer tag；没有有效 tag 时安装会在修改系统文件
之前失败。请以普通用户运行，不要执行 `sudo make install`。安装过程仅在写入
`/usr/local/bin` 时调用 `sudo`。

默认安装结果：

| 用途 | 路径 | 权限 |
| --- | --- | --- |
| 系统 CLI | `/usr/local/bin/tun-proxy` | `0755` |
| 用户配置目录 | `$HOME/.config/tun-proxy` | `0700` |
| 用户配置 | `$HOME/.config/tun-proxy/config.yaml` | `0600` |

在当前用户环境中，配置路径展开为：

```text
/Users/hailinpan/.config/tun-proxy/config.yaml
```

默认配置已编译进 `tun-proxy` 二进制，系统安装不需要从当前仓库复制配置。如果用户配置
已经存在，安装程序默认保留，不会覆盖。需要用内置默认配置重新生成时执行：

```sh
make install FORCE_CONFIG=1
```

也可以覆盖安装前缀或配置目录，例如：

```sh
make install PREFIX=/opt/tun-proxy CONFIG_DIR="$HOME/.config/tun-proxy"
```

## 6. 准备配置

不执行完整安装时，也可以直接使用已编译的二进制生成配置：

```sh
./bin/tun-proxy config -generate
```

如果 `tun-proxy` 已加入 `PATH`：

```sh
tun-proxy config -generate
```

默认写入 `$HOME/.config/tun-proxy/config.yaml`。目标已存在时命令会拒绝覆盖；确认需要
恢复为内置默认配置时使用：

```sh
tun-proxy config -generate -force
```

也可以指定其他目标：

```sh
tun-proxy config -generate -config /tmp/tun-proxy.yaml
```

生成命令会创建权限为 `0700` 的配置目录和权限为 `0600` 的配置文件，并在写入前使用
当前配置解析器校验内置模板。

安装或生成后编辑用户配置：

```sh
open -a TextEdit "$HOME/.config/tun-proxy/config.yaml"
```

或者直接在 Finder 中显示该文件：

```sh
tun-proxy config -finder
```

至少检查以下字段：

- `outbounds.*.interface`：使用当前 Mac 上实际存在的接口名称。
- `outbounds.*.dns_source`：只能设置为 `dhcp` 或 `static`；省略时默认为 `dhcp`。
  `dhcp` 优先使用所选接口当前 DHCP 租约中的 DNS，未获取到可用地址时回退到配置的
  `dns`；`static` 忽略 DHCP DNS，始终使用配置的 `dns`。
- `outbounds.*.dns`：无论选择 `dhcp` 还是 `static` 都必须至少填写一个 DNS 服务器。
  地址按配置顺序使用，并且必须能够通过所选接口访问。`127.0.0.1`、`::1` 等本机
  回环地址不会被接受为上游，避免 Fake DNS 递归查询自身。
- `outbounds.*.fallback`：确保 fallback 目标存在且不会形成循环。
- `dns.default_outbound`：选择一个已存在的 direct outbound。
- `rules`：替换示例域名并为每条规则选择预期出口。
- 最后一条规则：保留只包含 `outbound` 的默认规则。
- `capture.default_route`：除非已完成目标网络测试，否则保持为 `false`。

查看当前接口：

```sh
tun-proxy interfaces
```

接口名称不能在不同 Mac 之间直接复用，不要假设示例中的 `en0` 或 `en7` 一定存在。

例如，`en7` 设置了 `dns_source: dhcp`，配置 DNS 是 `9.9.9.9:53`，当前 DHCP 租约下发
`192.168.100.51`、`192.168.96.51` 时，运行中的 `en7` outbound 会依次使用这两个
DHCP DNS；只有 DHCP 没有提供可用 DNS 时才回退到 `9.9.9.9:53`。改为
`dns_source: static` 后，则始终使用 `9.9.9.9:53`，不会探测或采用 `en7` 的 DHCP DNS。
网络续租或切换后，tun-proxy 会重新读取使用 `dhcp` 模式的网卡 DNS。启用
`capture.default_route` 时，如果新的 DNS 需要不同的物理旁路路由，进程会先安全回滚并
要求重启，以免 DNS 请求被重新捕获进 TUN。

## 7. 校验和预检

CLI 默认读取：

```text
~/.config/tun-proxy/config.yaml
```

离线校验不会修改当前主机，也不需要 root：

```sh
tun-proxy config validate
tun-proxy config validate -service
tun-proxy config validate -service -json
```

`-service` 会额外检查托管服务要求的固定状态、锁、映射和 DNS 设置。

首次安装服务前执行实时预检：

```sh
sudo tun-proxy check
```

`check` 不会创建 utun、安装路由、修改 DNS 或启动服务。如果服务已经运行，应使用
`service status` 和 `diagnose`，不要用 DNS 监听端口是否空闲来判断现有服务。

## 8. 安装并启动托管服务

首次安装可以直接使用系统 CLI：

```sh
sudo tun-proxy service install
```

通过 `sudo` 执行时，默认配置路径仍会解析到发起调用用户的
`~/.config/tun-proxy/config.yaml`，不会错误读取 `/var/root`。

`service install` 会把用户配置复制到固定的托管目录，并安装 LaunchDaemon。托管服务
不会直接从用户 home 读取运行配置。主要固定路径如下：

| 用途 | 托管路径 |
| --- | --- |
| 特权可执行文件 | `/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy` |
| 托管配置 | `/Library/Application Support/tun-proxy/config.yaml` |
| LaunchDaemon plist | `/Library/LaunchDaemons/cn.notfound945.tun-proxy.plist` |
| 标准输出日志 | `/Library/Logs/tun-proxy/stdout.log` |
| 标准错误日志 | `/Library/Logs/tun-proxy/stderr.log` |
| 运行状态 | `/var/run/tun-proxy/state.json` |

如需先安装但不启动：

```sh
sudo tun-proxy service install -start=false
sudo tun-proxy check -service
sudo tun-proxy service start
```

仅在首次安装或完整卸载后使用 `service install`。已经安装 LaunchDaemon 时使用升级命令。

## 9. 验证和日常控制

```sh
sudo tun-proxy service status
sudo tun-proxy service status -json
sudo tun-proxy status -json
sudo tun-proxy diagnose
```

常用服务命令：

```sh
sudo tun-proxy service start
sudo tun-proxy service stop
sudo tun-proxy service restart
sudo tun-proxy service reload
sudo tun-proxy service logs -lines 100
sudo tun-proxy service logs -follow -stream both
```

`service stop` 会回滚运行期间的系统状态但保留安装；`service reload` 仅接受可热重载的
配置变化；日志跟随可通过 `Ctrl-C` 退出，不会停止服务。

## 10. 更新二进制和配置

### Release 安装更新

普通用户通过 Release 安装后，可以让 `curl` 将脚本直接交给 Bash，不需要用 `-o`
保存到当前目录：

```sh
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | bash

curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  bash -s -- v1.2.3
```

在仓库源码目录内开发或测试脚本时，也可以直接运行
`./scripts/update-release.sh [version-or-archive]`。

脚本会复用 Release 安装器完成架构识别、下载、SHA-256 和版本校验，然后更新系统 CLI。
如果同时检测到托管 binary、配置和 plist，会继续调用 `service upgrade` 更新 LaunchDaemon
使用的独立 binary；没有安装托管服务时不会自动创建服务。
CLI 和托管服务是两份不同文件。再次执行 `install-release.sh` 只更新
`/usr/local/bin/tun-proxy`，不会替换
`/Library/PrivilegedHelperTools/cn.notfound945.tun-proxy`。已经安装或加载服务时应使用
`service upgrade`，不要再次执行 `service install`。

默认更新保留用户配置、托管配置和服务原来的运行/停止状态。明确需要同步用户配置或启动
原本停止的服务时使用：

```sh
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  env UPDATE_SERVICE_CONFIG=1 bash

curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | \
  env START_SERVICE=1 bash
```

如果 `service status` 报告 `/var/run/tun-proxy/worker` 不存在，而 `service install` 又报告
launchd label 已加载，表示现有服务的易失 worker 运行目录丢失。不要重复安装或执行 purge；
直接执行上面的 `env START_SERVICE=1 bash` 更新命令，由事务升级重新准备存储并启动服务。

脚本也可以单独下载执行；`INSTALL_RELEASE_SCRIPT` 可指定本地安装器，未指定且同目录中没有
安装器时，会从 `TUN_PROXY_REPOSITORY` 的 `TUN_PROXY_INSTALLER_REF` 下载。

### 源码构建更新

更新源码后先检查并编译：

```sh
make check
make build
```

修改用户配置后先校验：

```sh
tun-proxy config validate -service
```

事务升级托管二进制和配置：

```sh
sudo tun-proxy service upgrade \
  -binary ./bin/tun-proxy \
  -config "$HOME/.config/tun-proxy/config.yaml"
```

升级失败时会自动回滚到先前版本。不要直接覆盖 LaunchDaemon 正在使用的托管二进制。

## 11. 卸载

卸载 LaunchDaemon，但保留托管配置、映射和日志：

```sh
sudo tun-proxy service uninstall
```

同时永久删除托管配置、映射和日志：

```sh
sudo tun-proxy service uninstall -purge
```

删除 `/usr/local/bin/tun-proxy`，但保留用户配置：

```sh
sudo rm -f /usr/local/bin/tun-proxy
```

`$HOME/.config/tun-proxy/config.yaml` 不会被上述命令删除。如需删除，应在确认不再需要后
自行备份并处理。

## 12. 故障排查

怀疑系统仍被 tun-proxy 接管时，先执行只读检查：

```sh
make system-proxy-check
```

该目标会编译当前源码，然后显示托管服务状态、运行恢复状态，并使用
`$HOME/.config/tun-proxy/config.yaml` 执行只读诊断。它不会修改系统网络。

确认需要停止代理并恢复网络时执行：

```sh
make system-proxy-clean
```

该目标先通过 `service stop` 正常停止托管服务并回滚运行时修改，再通过 `cleanup`
恢复异常退出可能留下的已记录 DNS 和路由状态，最后重新显示状态。它不会卸载
LaunchDaemon，也不会删除用户配置、日志或 Fake IP 映射。如果状态文件记录的非托管进程
仍然存活，安全检查会拒绝清理，避免破坏正在运行的进程。

安装或启动失败时，先收集：

```sh
tun-proxy config validate -service
tun-proxy interfaces
sudo tun-proxy service status -json
sudo tun-proxy diagnose -json
sudo tun-proxy service logs -lines 200 -stream both
```

常见原因包括：配置中的接口不存在、绑定接口的 DNS 服务器不可达、其他进程占用
`127.0.0.1:53`、Fake IP 路由冲突、托管存储路径偏离固定值，或 `/etc/hosts` 中存在
使域名绕过 Fake DNS 的条目。
