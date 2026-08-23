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
| `self-update` | 从 GitHub 下载并安装最新 Release | 不需要；必须以普通用户运行 |
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

## Release 自动更新

```sh
tun-proxy self-update
```

命令会使用 `/usr/bin/curl -fsSL` 下载仓库 `master` 分支中的
`scripts/update-release.sh`，确认下载成功且内容非空后交给 `/bin/bash` 执行，其更新效果与：

```sh
curl -fsSL \
  https://raw.githubusercontent.com/notfound945/tun-proxy/master/scripts/update-release.sh | bash
```

一致。脚本会先查询 GitHub 的最新 Release；当前 CLI 版本相同时会提示已是最新版本并
直接退出，不再下载安装器、Release 压缩包或升级托管服务。必须以普通用户运行，不要使用
`sudo tun-proxy self-update`；更新脚本会在替换系统文件时自行请求权限。`UPDATE_SERVICE_CONFIG`、
`START_SERVICE`、`PREFIX` 等更新脚本环境变量会原样继承，例如：

```sh
START_SERVICE=1 tun-proxy self-update
```

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
  -domain api.cursor.sh
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
sudo tun-proxy status -fake-ip
sudo tun-proxy status -fake-ip -json
sudo tun-proxy status -state /var/run/tun-proxy/state.json
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-state PATH` | `/var/run/tun-proxy/state.json` | 要读取的运行恢复状态。 |
| `-json` | `false` | 以 JSON 输出完整运行快照。 |
| `-fake-ip` | `false` | 按需包含实时 IPv4/IPv6 Fake IP 映射。 |

进程存活且状态中包含 status socket 时，该命令还会报告实时 DNS、TCP、UDP、Fake IP、
重载、资源和 TUN 指标。`-fake-ip` 会额外输出每条映射的地址、域名和过期时间；与
`-json` 组合时，映射位于 `fake_ip_mappings.ipv4` 和 `fake_ip_mappings.ipv6`。映射列表
只从正在运行的进程读取，服务停止或没有可用 status socket 时该 flag 会报错。普通
`status` 不请求映射列表，避免映射数量较大时增加状态查询开销。

## `cleanup`

```sh
sudo tun-proxy cleanup
sudo tun-proxy cleanup \
  -state /var/run/tun-proxy/state.json \
  -lock /var/run/tun-proxy/tun-proxy.lock
sudo tun-proxy cleanup -clear-dns \
  -config ~/.config/tun-proxy/config.yaml
sudo tun-proxy cleanup -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
sudo tun-proxy cleanup -clear-dns -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config PATH` | `~/.config/tun-proxy/config.yaml` | 使用 clear flag 时读取 `dns.listen`、Fake IP 持久化路径、状态路径和锁路径的配置文件。 |
| `-state PATH` | `/var/run/tun-proxy/state.json` | 要恢复的已记录系统状态；显式传入时覆盖配置值。 |
| `-lock PATH` | `/var/run/tun-proxy/tun-proxy.lock` | 备用的陈旧进程锁路径；显式传入时覆盖配置值。 |
| `-timeout DURATION` | `30s` | cleanup 的最大执行时间。 |
| `-clear-dns` | `false` | 将完整 DNS 列表仍恰好等于配置中 `dns.listen` 地址的已启用网络服务重置为自动/DHCP DNS。 |
| `-clear-fake-ip` | `false` | 删除配置的 IPv4/IPv6 Fake IP 快照和对应 `.wal`。 |

异常退出后如残留已记录状态，可使用普通 `cleanup` 精确恢复原 DNS 和路由。状态文件缺失时，
`-clear-dns` 提供保守兜底：它也会检查当前不活跃但仍启用的网络服务，但只修改完整 DNS 列表
仍为单个 `dns.listen` 地址的服务，不覆盖手动 DNS、混合 DNS 列表或外部程序已经修改的值。

`-clear-dns` 与 `-clear-fake-ip` 可以单独或同时使用。两者都会先恢复可用的记录状态，然后
共用一次实例锁执行额外清理；实例仍在启动或运行时会拒绝操作。正常的前台退出和托管服务
停止会自行恢复各自事务修改的系统状态，但不会自动删除 Fake IP 映射。

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

### 托管服务操作互斥

所有会修改托管服务或托管配置的命令——`install`、`start`、`stop`、`restart`、
`sync-user-config`、`reload`、`upgrade` 和 `uninstall`——都会在完整事务期间持有独立的跨进程
operation lock：`/var/run/tun-proxy.service-operation.lock`。使用默认托管 state/lock 路径的
`cleanup` 也使用同一把锁；显式指定独立 state/lock 的 standalone cleanup 不会获取它。

锁使用非阻塞 `flock`。已有其他写操作执行时，命令会立即返回“service operation is already in
progress”，并在 metadata 可用时附带操作类型、operation ID、PID 和开始时间。`service status`、
`service logs` 等只读命令不获取该排他锁。锁文件会保留，但只有实际持有的内核锁表示操作仍在
进行；持锁进程退出或崩溃后内核会自动释放锁。

### `service start`

```sh
sudo tun-proxy service start
```

启动已安装的服务并等待就绪，最长等待 20 秒。该命令没有参数。启动失败（包括等待就绪超时）时，
错误信息会提示运行 `sudo tun-proxy service logs` 查看托管服务的 stdout/stderr 日志。

### `service stop`

```sh
sudo tun-proxy service stop
```

干净停止服务，禁用对应的 launchd label，并将 job 从当前 system domain 卸载，从而阻止
`KeepAlive` 在退出后再次拉起进程。安装文件和配置会保留；之后执行 `service start` 会重新
enable、bootstrap 并等待服务就绪。停止成功后，`service status` 应显示 `loaded=false`、
`running=false`。该命令没有参数。停止失败时，错误信息会提示运行
`sudo tun-proxy service logs` 查看托管服务日志。日志文件中的历史内容不会被自动清空，但停止
成功后不应继续追加新的服务日志。

### `service restart`

```sh
sudo tun-proxy service restart
```

先干净停止服务，再启动并检查是否就绪。该命令没有参数。

### `service sync-user-config`

```sh
sudo tun-proxy service sync-user-config
```

根据 `SUDO_USER` 选择调用者的 `~/.config/tun-proxy/config.yaml`，完成安全读取、YAML 与字段
约束校验后，原子替换 `/Library/Application Support/tun-proxy/config.yaml`。该命令没有参数。

- 服务未运行：禁用并卸载仍注册的 launchd job，原子同步配置并保持停止；随后可执行
  `sudo tun-proxy service start`。
- 服务正在运行：先干净停止服务，再同步配置、重新启动并等待就绪。新配置启动失败时会回滚
  托管配置，并尝试重新启动旧配置。

该命令适用于 TUN、监听地址、地址池、容量和 default-route 拓扑等需要完整重启的修改，也可用于
修复导致服务无法启动的错误网口。操作失败时，错误信息会提示运行
`sudo tun-proxy service logs` 查看托管服务日志。

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
| `-config PATH` | 不同步 | 校验并安装指定配置，然后进行热重载。 |
| `-user-config` | `false` | 使用调用 `sudo` 的用户默认配置进行热重载，行为同 `-config`。 |
| `-timeout DURATION` | `15s` | 等待 supervisor/worker 最终结果的正数时长。 |

`service reload` 始终要求服务处于 `running` 阶段。不带 `-config` 或 `-user-config` 时，只重新
读取已经安装在 `/Library/Application Support/tun-proxy/config.yaml` 的托管配置。

`-user-config` 会根据 `SUDO_USER` 选择调用者的
`~/.config/tun-proxy/config.yaml`，等价于显式传入该路径，但不能和 `-config` 同时使用。
使用任一配置参数时，CLI 会安全读取并校验指定文件，继续检查不可热重载字段和 direct 出口
网口，并原子同步托管配置。随后 CLI 连接 root-only `/var/run/tun-proxy/control.sock`，发送期望
配置摘要，并等待 supervisor 返回 worker 的最终成功或失败结果；status reload counters 仅用于
观测，不再用于关联当前 CLI 请求。运行时拒绝、确认超时或摘要不一致时，CLI 恢复旧托管配置，
重新计算旧配置摘要，并通过同一 control socket 确认运行时恢复。`SIGHUP` 仅保留为手工兼容入口。
服务未运行或尚未进入 `running` 阶段时不会复制配置；同步完整用户配置应使用
`service sync-user-config`。

服务必须已经安装。操作失败时，错误信息会提示运行 `sudo tun-proxy service logs` 查看托管服务日志。

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
`-start-at-boot=true` 或 `false` 可切换。升级后的运行处理取决于升级开始前的状态：

- 升级前服务处于 `running` 且已就绪：启动新版本并等待其就绪；新版本启动失败时回滚文件，
  然后尝试恢复旧版本服务。
- 升级前服务未运行或未就绪：只替换安装文件，不启动新版本，也不执行 readiness 检查；
  升级完成后保持 stopped/unloaded。之后可显式执行 `sudo tun-proxy service start`，或由后续
  launchd 启动流程按保留的策略加载。

因此，网卡暂时不存在、上游当前不可用等配置或运行环境问题不会阻止一个原本未就绪的服务
完成版本升级。若升级操作本身失败，可运行 `sudo tun-proxy service logs` 查看托管日志。

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
