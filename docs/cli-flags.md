# tun-proxy CLI 命令与参数

本文档是 `tun-proxy` 公开命令行接口的完整参考。私有入口 `_service-run` 和
`_service-worker` 由 launchd 管理，不属于面向用户的接口。

## 使用约定

以下示例假设 `tun-proxy` 已加入 `PATH`。如果直接在本仓库中运行，请将它替换为
`./bin/tun-proxy`。

- `PATH`、`NAME`、`ADDRESS` 和 `PORT` 表示由用户提供的值。
- 布尔参数通过直接写出参数名启用，例如 `-json`。
- 如果某个布尔参数默认为 true，可使用显式值关闭，例如 `-start=false`。
- 时长使用 Go duration 格式，例如 `500ms`、`15s` 或 `2m`。
- 所有命令都会拒绝未预期的位置参数。
- 内置帮助可使用 `tun-proxy help`、`tun-proxy help COMMAND` 或
  `tun-proxy help service COMMAND` 查看。

大多数查询命令和离线配置命令不需要 root。创建 utun、修改路由或 DNS、恢复系统状态、
管理 LaunchDaemon 的命令应通过 `sudo` 运行。

## 命令概览

| 命令 | 用途 | Root 要求 |
| --- | --- | --- |
| `interfaces` | 列出网络接口 | 不需要 |
| `config -generate` | 从二进制内置模板生成默认配置 | 不需要 |
| `config -finder` | 在 Finder 中显示配置文件 | 不需要 |
| `config validate` | 在不修改主机的情况下校验配置 | 不需要 |
| `check` | 校验配置和当前主机前置条件 | 完整检查需要 |
| `explain` | 解释模拟流量的策略选择 | 通常不需要；访问受保护资源时除外 |
| `diagnose` | 收集只读健康报告 | 可选；使用 `sudo` 可读取完整托管运行数据 |
| `run` | 在前台运行代理 | 需要 |
| `status` | 读取运行状态和指标 | 查询托管服务时通常需要 |
| `cleanup` | 恢复已记录的系统状态 | 需要 |
| `service ...` | 管理 LaunchDaemon | 需要 |
| `version` / `-version` / `--version` | 输出构建信息 | 不需要 |
| `help ...` | 显示内置命令帮助 | 不需要 |

## 版本信息

以下三种写法等价：

```sh
tun-proxy version
tun-proxy -version
tun-proxy --version
```

输出包含版本号、Commit 和构建时间。`make build` 生成的开发版本号为 `local`；
`make build-release` 会先运行 `golangci-lint`，再从当前 annotated SemVer tag 注入发布版本号。

## 全局帮助

```sh
tun-proxy help
tun-proxy -h
tun-proxy --help
tun-proxy help config
tun-proxy help config validate
tun-proxy help explain
tun-proxy help service
tun-proxy help service reload
```

`help` 接收命令路径而不是命令参数。它不会检查或修改主机状态。

## `interfaces`

```sh
tun-proxy interfaces
```

列出当前每个接口的索引、状态标记、MTU 和已分配地址。该命令没有参数。

## `config -generate` 与 `config -finder`

```sh
tun-proxy config -generate
tun-proxy config -generate -force
tun-proxy config -generate -config /tmp/tun-proxy.yaml
tun-proxy config -finder
tun-proxy config -finder -config "$HOME/.config/tun-proxy/config.yaml"
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-generate` | `false` | 将编译进二进制的默认配置写入目标文件。 |
| `-finder` | `false` | 在 Finder 中显示目标文件。 |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 要生成或显示的配置文件。通过 `sudo` 执行时优先使用 `SUDO_USER` 的 home。 |
| `-force` | `false` | 仅与 `-generate` 一起使用；覆盖已有普通文件。 |

`-generate` 会先使用当前配置解析器校验内置模板，再以 `0700` 创建配置目录、以 `0600`
原子安装配置文件。默认拒绝覆盖已有文件，也会拒绝符号链接和非普通目标。

`-finder` 会将路径解析为绝对路径，并调用 `/usr/bin/open -R`，让 Finder 选中文件。
它会拒绝不存在的路径、目录和其他非普通文件，不会编辑或覆盖配置。

`-generate` 与 `-finder` 必须二选一。生成完成后可使用 `tun-proxy config validate`
校验配置。

## `config validate`

```sh
tun-proxy config validate
tun-proxy config validate -service
tun-proxy config validate -json
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 要校验的 YAML 配置。 |
| `-service` | `false` | 同时强制校验托管服务的固定路径约束。 |
| `-json` | `false` | 以 JSON 输出校验结果。 |

这是离线语法和语义校验。它会输出配置摘要和源文件摘要值，但不会检查实时接口、路由、
DNS 监听端口可用性或存储目录所有权。这些实时条件请使用 `check` 检查。

## `check`

```sh
sudo tun-proxy check
sudo tun-proxy check -service
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 要加载的 YAML 配置。 |
| `-service` | `false` | 校验已安装的权限分离布局、worker 存储和托管服务前置条件。 |

`check` 会在不启动代理的情况下执行实时预检，包括权限、接口、路由、路径和 DNS
监听端口可用性。

## `explain`

在不产生 DNS 流量的情况下解释域名决策：

```sh
tun-proxy explain \
  -domain api.cursor.sh \
  -protocol tcp \
  -port 443
```

使用一个或多个显式地址模拟解析后的 CIDR 策略：

```sh
tun-proxy explain \
  -domain example.com \
  -ip 203.0.113.10 \
  -ip 2001:db8::10 \
  -json
```

通过配置中绑定接口的上游 DNS 执行解析：

```sh
tun-proxy explain \
  -domain example.com \
  -resolve \
  -family ipv4 \
  -timeout 10s
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 用于评估的 YAML 配置。 |
| `-domain NAME` | 空 | 目标域名；除非至少提供一个 `-ip`，否则必须指定。 |
| `-ip ADDRESS` | 无 | 已解析或直接使用的 IP 地址；可重复指定多个地址。 |
| `-protocol tcp\|udp` | `tcp` | 流量协议。 |
| `-port PORT` | `443` | 目标端口，范围为 1–65535。 |
| `-family ipv4\|ipv6` | `ipv4` | `-resolve` 使用的地址族。 |
| `-resolve` | `false` | 查询配置中绑定接口的上游 DNS。 |
| `-timeout DURATION` | `10s` | 大于零的 DNS 解析超时时间。 |
| `-json` | `false` | 以 JSON 输出完整解释结果。 |

未指定 `-ip` 或 `-resolve` 时，解释过程完全离线，CIDR 决策可能保持待定状态。
`-resolve` 必须与 `-domain` 一起使用，且不能与 `-ip` 同时使用。

## `diagnose`

```sh
tun-proxy diagnose
sudo tun-proxy diagnose -json
sudo tun-proxy diagnose \
  -config "/Library/Application Support/tun-proxy/config.yaml" \
  -state /var/run/tun-proxy/state.json \
  -hosts /etc/hosts
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 要检查的配置。通过 `sudo` 执行时优先使用 `SUDO_USER` 的 home。 |
| `-state PATH` | `/var/run/tun-proxy/state.json` | 要检查的运行恢复状态。 |
| `-hosts PATH` | `/etc/hosts` | 扫描是否存在绕过策略条目的 hosts 文件。 |
| `-json` | `false` | 以 JSON 输出完整报告。 |

诊断过程只读。当当前用户无权读取 root 所有的文件时，命令会保留其他可获得的部分结果。
使用 `sudo` 可获得完整的托管服务报告。

## `run`

```sh
sudo tun-proxy run
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 前台进程使用的 YAML 配置。 |

该命令执行预检、创建 utun 数据路径，并在前台持续运行直到被中断。按 `Ctrl-C`
可干净退出。

## `status`

```sh
sudo tun-proxy status
sudo tun-proxy status -json
sudo tun-proxy status -state /var/run/tun-proxy/state.json
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-state PATH` | `/var/run/tun-proxy/state.json` | 要读取的运行恢复状态。 |
| `-json` | `false` | 以 JSON 输出完整运行快照。 |

进程存活且状态中包含 status socket 时，该命令还会报告实时 DNS、TCP、UDP、Fake IP、
重载、资源和 TUN 指标。

## `cleanup`

```sh
sudo tun-proxy cleanup
sudo tun-proxy cleanup \
  -state /var/run/tun-proxy/state.json \
  -lock /var/run/tun-proxy/tun-proxy.lock
sudo tun-proxy cleanup -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 清理 Fake IP 时用于读取持久化、状态和锁路径的配置文件。 |
| `-state PATH` | `/var/run/tun-proxy/state.json` | 要恢复的已记录系统状态；显式传入时覆盖配置值。 |
| `-lock PATH` | `/var/run/tun-proxy/tun-proxy.lock` | 备用的陈旧进程锁路径；显式传入时覆盖配置值。 |
| `-timeout DURATION` | `30s` | cleanup 的最大执行时间。 |
| `-clear-fake-ip` | `false` | 删除配置的 IPv4/IPv6 Fake IP 快照和对应 `.wal`。 |

异常退出后如残留已记录状态，可使用 `cleanup` 恢复。`-clear-fake-ip` 会先恢复系统状态，
然后持有实例锁再删除映射；实例仍在启动或运行时会拒绝清理。正常的前台退出和托管服务停止
会自行恢复各自事务修改的系统状态，但不会自动删除 Fake IP 映射。

## 托管服务

托管服务使用固定的安装、配置、运行、映射和日志路径。所有生命周期命令都应通过
`sudo` 运行。

### `service install`

```sh
sudo tun-proxy service install
sudo tun-proxy service install \
  -config "$HOME/.config/tun-proxy/config.yaml" \
  -binary ./bin/tun-proxy
sudo tun-proxy service install -start=false
sudo tun-proxy service install -start-at-boot=true
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 要安装的配置。通过 `sudo` 执行时优先使用 `SUDO_USER` 的 home。 |
| `-binary PATH` | 当前可执行文件 | 要安装的二进制文件。 |
| `-start BOOL` | `true` | 安装后启动服务；使用 `-start=false` 可只安装不启动。 |
| `-start-at-boot BOOL` | `false` | 是否允许 macOS 开机加载时自动启动服务。 |

安装过程是事务性的。使用默认值 `-start=true` 时，安装成功后还会等待服务就绪，但默认
不会配置开机自启。`-start-at-boot=true` 同时启用 launchd 的异常退出自动重启；默认关闭
时不会写入隐式触发开机运行的 `KeepAlive`。

### `service start`

```sh
sudo tun-proxy service start
```

启动已安装的服务并等待就绪。该命令没有参数。

### `service stop`

```sh
sudo tun-proxy service stop
```

干净停止服务并保留安装状态，之后可以通过 `service start` 再次启动。该命令没有参数。

### `service restart`

```sh
sudo tun-proxy service restart
```

先干净停止服务，再启动并检查是否就绪。该命令没有参数。

### `service reload`

```sh
sudo tun-proxy service reload
sudo tun-proxy service reload -timeout 30s
sudo tun-proxy service reload -user-config
sudo tun-proxy service reload \
  -config "/path/to/config.yaml"
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | 不同步 | 校验并事务性安装指定配置，然后热重载。 |
| `-user-config` | `false` | 使用调用 `sudo` 的用户默认配置并热重载。 |
| `-timeout DURATION` | `15s` | 等待运行时确认的正数时长。 |

重载前会检查托管服务是否正在运行；未运行时会提示完整的
`sudo tun-proxy service start` 命令。不带 `-config` 或 `-user-config` 时，只重新读取
已经安装在 `/Library/Application Support/tun-proxy/config.yaml` 的托管配置。

`-user-config` 会根据 `SUDO_USER` 选择调用者的
`~/.config/tun-proxy/config.yaml`，等价于显式传入该路径，但不能和 `-config` 同时使用。
使用任一配置参数时，CLI 会安全读取并校验指定文件，预检不可热重载字段和配置中的网口，
然后将校验过的同一份字节内容原子同步到托管配置路径，发送重载信号并等待 worker 以
配置摘要确认应用成功。如果预检失败，托管配置不会改变；如果运行时拒绝、确认超时或
摘要不一致，CLI 会恢复旧托管配置，并再次触发重载以恢复旧运行配置。服务必须已经安装
且正在运行。

### `service status`

```sh
sudo tun-proxy service status
sudo tun-proxy service status -json
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-json` | `false` | 以 JSON 输出托管安装、launchd 和运行状态。 |

### `service logs`

```sh
sudo tun-proxy service logs
sudo tun-proxy service logs -lines 200
sudo tun-proxy service logs -n 200 -stream stderr
sudo tun-proxy service logs -follow -stream both
sudo tun-proxy service logs -f -stream stdout
sudo tun-proxy service logs -clear
sudo tun-proxy service logs -clear -stream stderr
sudo tun-proxy service logs -clear -follow
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-lines N` | `100` | 输出末尾行数，范围为 0–10,000。 |
| `-n N` | `100` | `-lines` 的别名。 |
| `-follow` | `false` | 持续跟随新增日志内容。 |
| `-f` | `false` | `-follow` 的别名。 |
| `-clear` | `false` | 清空所选日志；与 `-follow` 组合时，清空后继续等待新日志。 |
| `-stream stdout\|stderr\|both` | `both` | 选择要读取、跟随或清理的托管日志流。 |

跟随模式会持续运行直到被中断。`-clear` 会安全截断所选普通日志文件，不删除文件，
因此正在运行的 launchd 服务可以继续写入；不存在的日志视为已经清空。该命令只操作
固定的托管日志路径，并拒绝符号链接和非普通文件。

### `service upgrade`

```sh
sudo tun-proxy service upgrade
sudo tun-proxy service upgrade -binary ./bin/tun-proxy
sudo tun-proxy service upgrade \
  -binary ./bin/tun-proxy \
  -config "$HOME/.config/tun-proxy/config.yaml"
sudo tun-proxy service upgrade -start-at-boot=false
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-binary PATH` | 当前可执行文件 | 替换用二进制文件。 |
| `-config PATH` | 不替换 | 可选的替换配置。 |
| `-start-at-boot BOOL` | 保留当前设置 | 可选的开机自启策略变更。 |

升级会以事务方式替换指定文件。默认保留已安装 plist 的开机自启策略；显式传入
`-start-at-boot=true` 或 `false` 可切换。如果新服务未能就绪，则自动回滚。

### `service uninstall`

```sh
sudo tun-proxy service uninstall
sudo tun-proxy service uninstall -purge
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-purge` | `false` | 同时删除已安装配置、映射和日志。 |

不指定 `-purge` 时，卸载会保留托管数据。`-purge` 会永久删除这些托管文件，
仅应在确定不再需要时使用。

## `version`

```sh
tun-proxy version
```

输出版本、源码提交和构建时间。该命令没有参数。
