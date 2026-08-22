# `config.yaml` 完整配置参考

本文档逐项说明 `tun-proxy` 当前支持的全部 YAML 配置、默认值、校验约束、运行时行为和热重载限制。

> 配置格式当前只支持 `version: 1`。解析器采用严格模式：字段名拼错或出现未支持字段时会直接拒绝，而不是静默忽略。

## 1. 配置文件位置

CLI 默认读取当前调用用户的配置：

```text
~/.config/tun-proxy/config.yaml
```

可生成一份默认配置：

```sh
tun-proxy config -generate
```

仓库中的参考配置位于：

```text
configs/example.yaml
```

安装 LaunchDaemon 后，`service install` 会把用户配置复制到固定的托管路径：

```text
/Library/Application Support/tun-proxy/config.yaml
```

用户配置与托管配置是两份独立文件。修改用户配置后，可使用以下命令校验、同步并热重载：

```sh
sudo tun-proxy service reload -user-config
```

也可以让大多数命令读取指定文件：

```sh
tun-proxy config validate -config /path/to/config.yaml
sudo tun-proxy check -config /path/to/config.yaml
sudo tun-proxy run -config /path/to/config.yaml
```

## 2. 完整配置示例

下面的示例展示了当前全部配置字段。`fake_ipv6` 和 `tun.ipv6_*` 是可选能力；如果主机没有安全可用的 IPv6 数据路径，运行时会保留配置，但不会返回 Fake AAAA。

```yaml
version: 1

log:
  level: info
  format: text

system:
  state_file: /var/run/tun-proxy/state.json
  lock_file: /var/run/tun-proxy/tun-proxy.lock
  manage_dns: true
  # 兼容字段，只能省略或设为 true；不建议新配置继续写入。
  restore_on_exit: true

capture:
  # false 时纯 ip_cidr 规则无法接管普通真实 IP/literal IP 流量；
  # domain/domain_suffix + ip_cidr 组合规则仍可通过 Fake IP 进入 TUN。
  default_route: false

tun:
  address: 10.255.0.2
  peer: 10.255.0.1
  ipv6_address: fd00:7475:6e70:ffff::2
  ipv6_peer: fd00:7475:6e70:ffff::1
  mtu: 1400
  packet_queue: 1024
  buffer_pool: 128

fake_ip:
  cidr: 198.18.0.0/15
  dns_ttl: 1m
  mapping_ttl: 24h
  max_mappings: 65536
  persistence_file: /var/lib/tun-proxy/fake-ip.yaml
  exclude:
    - localhost
    - "*.local"
    - "*.lan"

fake_ipv6:
  cidr: fd00:7475:6e70::/96
  max_mappings: 65536
  persistence_file: /var/lib/tun-proxy/fake-ipv6.yaml

dns:
  listen: 127.0.0.1:53
  udp: true
  tcp: true
  default_outbound: wifi
  max_concurrent: 256

sessions:
  max_tcp_flows: 1024
  udp_idle_timeout: 2m
  max_udp_sessions: 4096
  max_udp_sessions_per_source: 256

outbounds:
  wifi:
    type: direct
    interface: en0
    dns_source: dhcp
    dns:
      - "1.1.1.1:53"
      - "8.8.8.8:53"
    connect_timeout: 10s
    fallback: reject

  wired:
    type: direct
    interface: en5
    dns_source: static
    dns:
      - "9.9.9.9:53"
    connect_timeout: 10s
    fallback: wifi

  reject:
    type: reject

rules:
  - domain_suffix:
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

  - domain:
      - example.com
    outbound: wired

  - domain_suffix:
      - video.example
    outbound: wifi

  # 纯 CIDR 示例：若要接管普通真实 IP/literal IP 流量，必须把
  # capture.default_route 改为 true。
  - ip_cidr:
      - 203.0.113.0/24
      - 2001:db8::/32
    outbound: reject

  # 必须且只能存在一条无条件默认规则，并且必须放在最后。
  - outbound: wifi
```

## 3. 全局语法与校验规则

- 只支持一个 YAML document；不能用 `---` 在同一文件内追加第二份配置。
- 配置文件必须是普通文件，最大为 1 MiB。
- 未知字段、重复字段、错误的 YAML 类型都会被拒绝。
- 时间使用 Go duration 格式，例如 `500ms`、`10s`、`2m`、`24h`；当前所有 duration 字段都必须大于零。
- 路径必须是经过清理的绝对路径，例如 `/var/run/tun-proxy/state.json`。不能使用 `~`、相对路径、尾随 `/` 或包含 `..` 的非规范路径。
- 域名会去除首尾空白和末尾的点，并统一转为小写。
- 配置解析只验证格式和配置关系；网卡是否存在、路由是否可用、目录权限是否正确等主机条件由 `tun-proxy check` 检查。
- 多个数值字段把 `0` 当作“使用默认值”，因此不能通过填写 `0` 来禁用该限制。

离线校验：

```sh
tun-proxy config validate
```

包含网卡、路由、文件权限和 DNS 监听端口检查的实时预检：

```sh
sudo tun-proxy check
```

如果配置将用于 LaunchDaemon，可额外检查托管路径契约：

```sh
tun-proxy config validate -service
sudo tun-proxy check -service
```

托管服务要求：

- `dns.udp` 和 `dns.tcp` 必须同时为 `true`；
- `system.state_file` 必须为 `/var/run/tun-proxy/state.json`；
- `system.lock_file` 必须为 `/var/run/tun-proxy/tun-proxy.lock`；
- `fake_ip.persistence_file` 必须为 `/var/lib/tun-proxy/fake-ip.yaml`；
- 配置 `fake_ipv6` 时，`fake_ipv6.persistence_file` 必须为 `/var/lib/tun-proxy/fake-ipv6.yaml`。

## 4. 顶层字段

| 字段 | 类型 | 必填 | 默认值 | 热重载 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `version` | integer | 是 | 无 | 否 | 配置格式版本，当前必须为 `1`。 |
| `log` | object | 否 | 见下文 | 部分 | 日志级别和输出格式。 |
| `system` | object | 否 | 见下文 | 否 | 恢复状态、实例锁和系统 DNS 管理。 |
| `capture` | object | 否 | 见下文 | 否 | 是否捕获普通默认路由流量。 |
| `tun` | object | 否 | 见下文 | 否 | TUN 地址和资源限制。 |
| `fake_ip` | object | 否 | 见下文 | 部分 | IPv4 Fake IP 地址池和持久化。 |
| `fake_ipv6` | object | 否 | 禁用 | 否 | 可选的 IPv6 Fake IP 地址池。 |
| `dns` | object | 部分 | 见下文 | 部分 | 本地 Fake DNS 监听和默认真实 DNS 出口。 |
| `sessions` | object | 否 | 见下文 | 部分 | TCP/UDP 会话上限和超时。 |
| `outbounds` | map | 是 | 无 | 有条件 | 物理出口、DNS 来源和 fallback。至少配置一个。 |
| `rules` | list | 是 | 无 | 是 | 按顺序匹配的流量规则，必须包含最后一条默认规则。 |

## 5. `log`

```yaml
log:
  level: info
  format: text
```

| 字段 | 类型 | 默认值 | 可选值 | 热重载 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `log.level` | string | `info` | `debug`、`info`、`warn`、`error` | 是 | 日志级别，不区分大小写。 |
| `log.format` | string | `text` | `text`、`json` | 是 | 日志输出格式，不区分大小写。 |

## 6. `system`

```yaml
system:
  state_file: /var/run/tun-proxy/state.json
  lock_file: /var/run/tun-proxy/tun-proxy.lock
  manage_dns: true
```

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
| --- | --- | --- | --- | --- |
| `system.state_file` | string | `/var/run/tun-proxy/state.json` | 否 | 记录程序已修改的 DNS、路由等主机状态，用于正常退出和 `cleanup` 恢复。必须是 clean absolute path。 |
| `system.lock_file` | string | `/var/run/tun-proxy/tun-proxy.lock` | 否 | 防止多个实例同时修改主机网络。必须是 clean absolute path，且不能与 `state_file` 相同。 |
| `system.manage_dns` | boolean | `true` | 否 | 是否把活动 macOS 网络服务的系统 DNS 改为 `dns.listen` 的本地地址。 |
| `system.restore_on_exit` | boolean | `true` | 不适用 | 兼容旧配置的字段。只能省略或设为 `true`；设为 `false` 会被拒绝，因为主机网络回滚是强制行为。运行时配置不保存该字段。 |

### `manage_dns` 的行为

- `true`：启动本地 DNS，并把活动系统网络服务的 DNS 指向本地监听地址；退出时按状态文件恢复原 DNS。
- `false`：本地 DNS 仍会启动，但程序不会修改系统 DNS。应用必须自行把 DNS 请求发送到 `dns.listen`，否则选择性 Fake DNS 不会参与解析。

恢复操作带有所有权保护：如果退出时发现当前 DNS 已被用户或其他程序改成不同值，`tun-proxy` 不会盲目覆盖，而会保留恢复状态供诊断或后续清理。

## 7. `capture`

```yaml
capture:
  default_route: false
```

| 字段 | 类型 | 默认值 | 热重载 | 说明 |
| --- | --- | --- | --- | --- |
| `capture.default_route` | boolean | `false` | 否 | 是否安装分割默认路由，把普通真实 IP 流量也捕获到 TUN。 |

> **与 `ip_cidr` 的关系：**不是所有包含 `ip_cidr` 的规则都要求开启此项。纯
> `ip_cidr` 规则要接管普通真实 IP 或 literal-IP 流量时，必须设为 `true`，否则这些流量
> 根本不会进入 TUN，规则引擎也就看不到它们。`domain` / `domain_suffix` 与 `ip_cidr` 的
> 组合规则在 `false` 时仍可生效，因为显式域名条件会触发 Fake IP，使流量先进入 TUN，
> 随后再用解析出的真实地址判断 CIDR。

### `default_route: false`

这是默认且风险较低的模式：

- 显式域名规则获得 Fake IP；Fake IP 前缀路由进入 TUN。
- 未被域名规则选中的普通域名获得上游 DNS 返回的真实 IP。
- 使用真实 IP 的普通流量继续走 macOS 原默认路由，不进入 TUN。
- literal-IP 流量同样不进入 TUN。
- 因此，仅包含 `ip_cidr` 的规则无法接管上述流量；但带显式域名条件的组合 CIDR 规则
  仍会通过 Fake IP 路径进入 TUN 并完成 CIDR 后匹配。

### `default_route: true`

启用后，程序会通过 IPv4 `0.0.0.0/1`、`128.0.0.0/1` 捕获普通默认路由流量；IPv6 数据路径可用时也会安装 `::/1`、`8000::/1`。

安装捕获路由前会先验证并记录：

- 每个 `direct` outbound 所用物理网卡的作用域默认路由和网关；
- 物理网关绕行路由；
- 所有实际使用的上游 DNS 地址的物理网卡绕行路由；
- 出口网卡、网关和 DNS 路由归属是否符合配置。

任何证明失败都会拒绝启动，而不是冒险安装默认捕获路由。启用该选项后，全部 `outbounds` 都不能热重载，因为出口变化会使已安装的绕行和作用域路由失效。

## 8. `tun`

```yaml
tun:
  address: 10.255.0.2
  peer: 10.255.0.1
  mtu: 1400
  packet_queue: 1024
  buffer_pool: 128
```

| 字段 | 类型 | 默认值 | 热重载 | 约束与说明 |
| --- | --- | --- | --- | --- |
| `tun.address` | string | `10.255.0.2` | 否 | TUN 本端 IPv4。必须是非 loopback、非 multicast、非 unspecified 的 IPv4 单播地址。 |
| `tun.peer` | string | `10.255.0.1` | 否 | TUN 对端 IPv4，约束同上，且不能等于 `tun.address`。 |
| `tun.ipv6_address` | string | 未配置 | 否 | 可选 TUN 本端 IPv6。必须是非 unspecified、非 multicast、非 IPv4-mapped 的 IPv6 地址；必须与 `ipv6_peer` 同时配置，并要求存在 `fake_ipv6`。 |
| `tun.ipv6_peer` | string | 未配置 | 否 | 可选 TUN 对端 IPv6，地址约束同上，且不能等于 `ipv6_address`。 |
| `tun.mtu` | integer | `1400` | 否 | 范围 `576`–`9000`。 |
| `tun.packet_queue` | integer | `1024` | 否 | TUN 读包队列容量，范围 `64`–`65536`。 |
| `tun.buffer_pool` | integer | `128` | 否 | 复用数据包缓冲区的池容量，范围 `8`–`4096`。 |

额外约束：

- `tun.address` 和 `tun.peer` 不能落入 `fake_ip.cidr`。
- `tun.ipv6_address` 和 `tun.ipv6_peer` 不能落入 `fake_ipv6.cidr`。
- `tun.ipv6_address` 与 `tun.ipv6_peer` 必须成对出现；只配置一个会被拒绝。

## 9. `fake_ip`

```yaml
fake_ip:
  cidr: 198.18.0.0/15
  dns_ttl: 1m
  mapping_ttl: 24h
  max_mappings: 65536
  persistence_file: /var/lib/tun-proxy/fake-ip.yaml
  exclude:
    - localhost
    - "*.local"
    - "*.lan"
```

| 字段 | 类型 | 默认值 | 热重载 | 约束与说明 |
| --- | --- | --- | --- | --- |
| `fake_ip.cidr` | CIDR string | `198.18.0.0/15` | 否 | IPv4 Fake IP 地址池。输入会规范化为网络前缀。 |
| `fake_ip.dns_ttl` | duration | `1m` | 是 | Fake A/AAAA DNS 答案的 TTL，必须大于零。实际 DNS 秒值最小为 1 秒。 |
| `fake_ip.mapping_ttl` | duration | `24h` | 否 | 域名与 Fake IP 映射的保留时间，必须大于 `dns_ttl`。IPv4/IPv6 池共享该值。 |
| `fake_ip.max_mappings` | integer | `65536` | 否 | IPv4 最大映射数，范围为 `1` 到地址池可用容量。池较小时默认值会自动下调。 |
| `fake_ip.persistence_file` | string | `/var/lib/tun-proxy/fake-ip.yaml` | 否 | IPv4 映射持久化文件，必须是 clean absolute path。WAL 使用关联路径。 |
| `fake_ip.exclude` | string list | `localhost`、`*.local`、`*.lan` | 是 | 永远不分配 Fake IP、始终转发到真实上游 DNS 的域名模式。 |

地址池会保留前 10 个地址和最后一个地址，因此可用容量为总地址数减 11。过小、没有可用容量的前缀会被拒绝。

### `exclude` 匹配方式

精确域名：

```yaml
exclude:
  - localhost
  - printer.lan
```

后缀域名：

```yaml
exclude:
  - "*.local"
  - "*.internal.example"
```

`*.local` 同时匹配 `local` 和其子域名，例如 `printer.local`。模式按域名标签边界匹配，不会错误匹配 `notlocal`。

如果完全省略 `exclude`，程序会使用默认的三项排除列表。若要显式取消所有默认排除项，必须写成：

```yaml
fake_ip:
  exclude: []
```

清理持久化的 IPv4/IPv6 映射及 WAL：

```sh
sudo tun-proxy cleanup -clear-fake-ip \
  -config ~/.config/tun-proxy/config.yaml
```

`cleanup` 需要通过配置读取 persistence 路径；执行前应先停止前台实例或托管服务。它会先恢复记录的系统状态，并确认实例锁空闲后再删除 Fake IP 数据。

## 10. `fake_ipv6`

`fake_ipv6` 整个区块可选。启用时必须同时配置 `tun.ipv6_address` 和 `tun.ipv6_peer`：

```yaml
tun:
  ipv6_address: fd00:7475:6e70:ffff::2
  ipv6_peer: fd00:7475:6e70:ffff::1

fake_ipv6:
  cidr: fd00:7475:6e70::/96
  max_mappings: 65536
  persistence_file: /var/lib/tun-proxy/fake-ipv6.yaml
```

| 字段 | 类型 | 默认值 | 必填 | 热重载 | 约束与说明 |
| --- | --- | --- | --- | --- | --- |
| `fake_ipv6.cidr` | CIDR string | 无 | 区块存在时必填 | 否 | 必须是 IPv6 ULA/private 前缀，不能是 IPv4-mapped IPv6。 |
| `fake_ipv6.max_mappings` | integer | `65536` | 否 | 否 | 范围为 `1` 到前缀可用容量；较小前缀会自动下调默认值。 |
| `fake_ipv6.persistence_file` | string | `/var/lib/tun-proxy/fake-ipv6.yaml` | 否 | 否 | 必须是 clean absolute path，且不能与 IPv4 persistence 路径相同。 |

Fake IPv6 使用 `fake_ip.dns_ttl` 和 `fake_ip.mapping_ttl`，没有单独的 TTL 配置。

配置 `fake_ipv6` 不等于运行时一定启用 Fake AAAA。程序启动时还会确认：

- 至少一个已配置的 `direct` 出口网卡具有可用、非 link-local 的 IPv6 地址；
- 系统存在 IPv6 默认路由；
- IPv6 默认路由使用的是已配置且具备 IPv6 能力的出口网卡。

条件不满足时，IPv6 池保持已配置状态，但本次进程运行期间 Fake AAAA 返回 NODATA；网络条件恢复后需要重启进程重新检测。

## 11. `dns`

```yaml
dns:
  listen: 127.0.0.1:53
  udp: true
  tcp: true
  default_outbound: wifi
  max_concurrent: 256
```

| 字段 | 类型 | 默认值 | 必填 | 热重载 | 约束与说明 |
| --- | --- | --- | --- | --- | --- |
| `dns.listen` | IP:port string | `127.0.0.1:53` | 否 | 否 | 只允许 IPv4 loopback 地址，端口范围 `1`–`65535`。 |
| `dns.udp` | boolean | `true` | 否 | 否 | 是否监听 UDP DNS。 |
| `dns.tcp` | boolean | `true` | 否 | 否 | 是否监听 TCP DNS。 |
| `dns.default_outbound` | string | 无 | 是 | 是 | 普通域名真实 DNS 查询使用的出口，必须引用一个 `direct` outbound。 |
| `dns.max_concurrent` | integer | `256` | 否 | 否 | 同时处理的 DNS 请求数，范围 `1`–`65536`。超过容量时返回 SERVFAIL。 |

`dns.udp` 与 `dns.tcp` 至少启用一个；LaunchDaemon 托管模式要求两者都启用。

### 选择性 Fake DNS 流程

对于 A/AAAA 请求，处理顺序为：

1. 如果域名命中 `fake_ip.exclude`，转发给 `dns.default_outbound` 的真实上游 DNS。
2. 如果域名命中任意非默认规则中的显式 `domain` / `domain_suffix` 条件，分配 Fake IP。
3. 其他普通域名转发给 `dns.default_outbound`，返回真实 IP。
4. A/AAAA 以外的记录类型始终转发真实上游 DNS。

DNS 请求尚未获得已解析目标 IP，因此在决定是否分配 Fake IP 时：

- 会考虑规则的 `domain` 和 `domain_suffix`；
- 不考虑 `ip_cidr`。

例如，纯 `ip_cidr` 规则不会触发 Fake IP；包含显式域名条件的规则会先按域名条件决定是否分配 Fake IP，流量进入 TUN 并完成解析后再判断 CIDR。

如果同一条规则同时配置 `domain` 和 `domain_suffix`，DNS 阶段也要求两个域名条件同时满足后才分配 Fake IP。

## 12. `sessions`

```yaml
sessions:
  max_tcp_flows: 1024
  udp_idle_timeout: 2m
  max_udp_sessions: 4096
  max_udp_sessions_per_source: 256
```

| 字段 | 类型 | 默认值 | 热重载 | 约束与说明 |
| --- | --- | --- | --- | --- |
| `sessions.max_tcp_flows` | integer | `1024` | 否 | 最大并发 TCP flow，范围 `1`–`1000000`。 |
| `sessions.udp_idle_timeout` | duration | `2m` | 是 | UDP session 空闲回收时间，必须大于零。 |
| `sessions.max_udp_sessions` | integer | `4096` | 否 | 全局最大 UDP session 数，范围 `1`–`1000000`。 |
| `sessions.max_udp_sessions_per_source` | integer | `256` | 否 | 单一源地址允许的最大 UDP session 数，范围 `1` 到 `max_udp_sessions`。 |

## 13. `outbounds`

`outbounds` 是以自定义名称为键的 map：

```yaml
outbounds:
  wifi:
    type: direct
    interface: en0
    dns_source: dhcp
    dns: ["1.1.1.1:53"]
    connect_timeout: 10s
    fallback: reject

  reject:
    type: reject
```

出口名称：

- 只能包含英文字母、数字、`_` 和 `-`；
- 不能以 `_` 或 `-` 开头；
- 名称区分大小写，引用必须完全一致。

### `type: direct`

`direct` 表示通过指定物理网卡建立连接并执行独立 DNS 解析。

| 字段 | 类型 | 默认值 | 必填 | 约束与说明 |
| --- | --- | --- | --- | --- |
| `outbounds.<name>.type` | string | 无 | 是 | `direct` 或 `reject`，值不区分大小写。 |
| `outbounds.<name>.interface` | string | 无 | `direct` 必填 | macOS 网卡名，例如 `en0`。最长 15 字符，只允许字母、数字和 `_`。 |
| `outbounds.<name>.dns_source` | string | `dhcp` | 否 | `dhcp` 或 `static`，值不区分大小写。 |
| `outbounds.<name>.dns` | string list | 无 | `direct` 必填 | 至少一个带端口的 DNS IP 地址。即使使用 `dhcp` 也必须配置，作为发现失败时的 fallback。 |
| `outbounds.<name>.connect_timeout` | duration | `10s` | 否 | 建立出口连接的超时，必须大于零。 |
| `outbounds.<name>.fallback` | string | 无 | 否 | 当前出口遇到可恢复错误时尝试的下一个 outbound。 |

DNS 地址必须显式写端口：

```yaml
dns:
  - "1.1.1.1:53"
  - "[2606:4700:4700::1111]:53"
```

不允许：

- loopback 地址；
- unspecified 地址；
- IPv4-mapped IPv6 地址；
- 端口 `0`；
- 只有 IP、没有端口的值。

### `dns_source: dhcp`

运行时优先发现 `interface` 对应网络服务的 DHCP DNS：

- 发现到非空 DHCP DNS 列表时，使用发现结果；
- 未发现或发现过程失败时，使用 YAML 中必填的 `dns` 列表；
- 启动日志会显示每个 direct outbound 的有效 DNS 来源与服务器列表。

### `dns_source: static`

始终使用 YAML 的 `dns` 列表，不读取该出口的 DHCP DNS。

### `type: reject`

`reject` 表示明确拒绝命中的流量：

```yaml
outbounds:
  reject:
    type: reject
```

`reject` outbound 不能配置以下字段：

- `interface`
- `dns_source`
- `dns`
- `connect_timeout`
- `fallback`

`dns.default_outbound` 不能引用 `reject`，但规则的 `outbound` 可以引用它。

### fallback 规则

- 必须引用已定义的 outbound；
- 不能引用自身；
- 整个 fallback 链不能形成环；
- fallback 只处理被判定为可恢复的网络或解析错误；
- 明确的业务 DNS 错误、不可恢复错误和 `reject` 不会无条件继续 fallback。

### outbounds 的热重载

- `capture.default_route: false` 时，可以热重载整个 `outbounds` map，包括网卡、DNS 来源、DNS 地址、超时和 fallback。
- `capture.default_route: true` 时，任何 outbound 变化都会被拒绝，必须重启服务才能重新规划物理网关和绕行路由。

## 14. `rules`

规则按 YAML 顺序匹配，第一条完整匹配的规则生效：

```yaml
rules:
  - domain_suffix:
      - example.com
    ip_cidr:
      - 203.0.113.0/24
    outbound: wifi

  - outbound: wired
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `rules[].domain` | string list | 否 | 精确域名列表。 |
| `rules[].domain_suffix` | string list | 否 | 域名后缀列表，同时匹配后缀本身和子域名。 |
| `rules[].ip_cidr` | CIDR list | 否 | IPv4/IPv6 目标网段列表。必须写规范网络前缀；纯 CIDR 规则要覆盖普通真实 IP/literal IP 流量时需要 `capture.default_route: true`。 |
| `rules[].outbound` | string | 是 | 必须引用一个已定义的 outbound。 |

`protocol` 和 `dst_port` 已移除；它们现在属于未知字段，配置校验会直接拒绝。

### 组合逻辑

同一字段列表内部是 **OR**。例如：

```yaml
ip_cidr: [203.0.113.0/24, 2001:db8::/32]
```

表示任意一个解析地址落入其中任意网段即可满足 CIDR 条件。

同一条规则的不同字段之间是 **AND**：

```yaml
- domain_suffix: [example.com]
  ip_cidr: [203.0.113.0/24]
  outbound: wifi
```

表示域名属于 `example.com`，并且解析后的真实目标地址落入 `203.0.113.0/24`。

如果一条规则同时配置 `domain` 和 `domain_suffix`，两个条件也必须同时满足。通常没有必要同时配置，除非确实需要取二者交集。

### 域名匹配

```yaml
- domain:
    - api.example.com
```

只匹配 `api.example.com`。

```yaml
- domain_suffix:
    - example.com
```

匹配：

- `example.com`
- `api.example.com`
- `a.b.example.com`

不匹配 `notexample.com`。匹配使用域名标签边界。

`domain_suffix` 的值直接写 `example.com`，不要写成 `*.example.com`；`*.` 形式只用于 `fake_ip.exclude`。

域名会规范化为小写并移除末尾的点，因此 `Example.COM.` 等价于 `example.com`。IP literal 不能写入域名字段。

### CIDR 匹配

```yaml
- ip_cidr:
    - 203.0.113.0/24
    - 2001:db8::/32
  outbound: wired
```

前缀必须是规范形式。例如应写 `192.0.2.0/24`，不能写 `192.0.2.1/24`。

域名流量命中含 `ip_cidr` 的候选规则时，程序会先通过候选出口解析域名；只要解析结果中的任意地址落入任意配置前缀，该 CIDR 条件就成立。如果最终规则选择了另一个 direct outbound，程序会通过最终出口重新解析，确保 DNS 缓存和 socket 绑定继续按出口隔离。

运行时是否能执行 CIDR 匹配，首先取决于流量是否进入 TUN：

- 纯 `ip_cidr` 规则不会触发 Fake IP。普通域名的真实 IP 流量和 literal-IP 流量只有在
  `capture.default_route: true` 时才会被默认捕获路由送入 TUN。
- `domain` / `domain_suffix` + `ip_cidr` 组合规则会由显式域名条件触发 Fake IP，因此即使
  `capture.default_route: false`，仍可在流量进入 TUN 后按真实解析地址判断 CIDR。

`tun-proxy explain -ip ...` 只模拟规则决策；它可以在 `default_route: false` 的配置下显示
纯 CIDR 命中，但这不代表对应的真实 IP 流量在实际运行时会被系统路由送入 TUN。

### 默认规则

必须恰好有一条不包含任何匹配条件的默认规则，并且必须放在最后：

```yaml
rules:
  - outbound: wifi
```

以下配置无效：

```yaml
rules:
  - outbound: wifi
  - domain: [example.com]
    outbound: wired
```

因为默认规则会提前吞掉全部流量，而且配置校验要求它必须是最后一条。

### 验证规则决策

离线检查域名规则：

```sh
tun-proxy explain \
  -domain downloads.example.com
```

模拟 CIDR 解析结果：

```sh
tun-proxy explain \
  -domain example.com \
  -ip 203.0.113.10
```

通过配置中的出口 DNS 实际解析：

```sh
tun-proxy explain \
  -domain example.com \
  -resolve \
  -family ipv4
```

## 15. DNS、Fake IP 与流量路径总览

### 命中显式域名规则

```text
应用查询域名
  -> 本地 DNS 判断 domain/domain_suffix 需要策略接管
  -> 返回 Fake A/AAAA
  -> Fake IP 前缀路由进入 TUN
  -> 根据完整规则（域名、CIDR）选择 outbound
  -> 通过目标物理网卡连接
```

### 普通域名，且 `capture.default_route: false`

```text
应用查询域名
  -> 本地 DNS 通过 dns.default_outbound 查询 DHCP/static DNS
  -> 返回真实 IP
  -> 连接按 macOS 默认路由直接从默认网卡发出
  -> 不进入 TUN
```

### 普通域名，且 `capture.default_route: true`

```text
应用查询域名
  -> 本地 DNS 返回真实 IP
  -> /1 分割默认路由把连接送入 TUN
  -> ip_cidr 规则可参与选择
  -> 按规则对应 outbound 发出
```

因此：

- 只想代理明确域名、普通流量保持系统默认路径时，使用 `capture.default_route: false`。
- 需要让纯 CIDR 规则覆盖普通真实 IP 或 literal-IP 流量时，使用
  `capture.default_route: true`，并先执行 `sudo tun-proxy check`。
- 只使用 `domain` / `domain_suffix` + `ip_cidr` 组合规则时，不必为 CIDR 匹配单独开启
  `capture.default_route`；显式域名条件已经提供 Fake IP 捕获路径。

## 16. 热重载矩阵

执行：

```sh
sudo tun-proxy service reload -user-config
```

### 可热重载

- `log.level`
- `log.format`
- `fake_ip.dns_ttl`
- `fake_ip.exclude`
- `dns.default_outbound`
- `sessions.udp_idle_timeout`
- 全部 `rules`
- 全部 `outbounds`，但仅限当前 `capture.default_route: false`

### 不可热重载

- `version`
- `system.state_file`
- `system.lock_file`
- `system.manage_dns`
- `capture.default_route`
- 全部 `tun.*`
- `fake_ip.cidr`
- `fake_ip.mapping_ttl`
- `fake_ip.max_mappings`
- `fake_ip.persistence_file`
- 启用或禁用整个 `fake_ipv6`
- 全部 `fake_ipv6.*`
- `dns.listen`
- `dns.udp`
- `dns.tcp`
- `dns.max_concurrent`
- `sessions.max_tcp_flows`
- `sessions.max_udp_sessions`
- `sessions.max_udp_sessions_per_source`
- 当前 `capture.default_route: true` 时的全部 `outbounds`

不可热重载的字段需要停止并重新启动前台实例。托管服务需要同步新配置并重启时，可以使用事务升级命令：

```sh
sudo tun-proxy service upgrade \
  -config ~/.config/tun-proxy/config.yaml
```

`service reload` 会在提交托管配置前拒绝不可热重载的变化，不会把无法应用的新配置留在托管路径。

## 17. 常见配置错误

### 普通域名也被返回 Fake IP

检查是否有过宽的 `domain_suffix` 规则。只有命中显式 `domain` / `domain_suffix` 的域名才应获得 Fake IP；如某个域名必须始终返回真实地址，可加入 `fake_ip.exclude`。

### DHCP DNS 没有生效

确认：

```yaml
dns_source: dhcp
```

并查看启动日志中的 `effective DNS`。如果接口没有发现到 DHCP DNS，程序会使用该 outbound 的 YAML `dns` 列表。

### 纯 CIDR 规则没有命中普通流量

默认 `capture.default_route: false` 时，普通真实 IP 和 literal-IP 流量不进入 TUN，因此纯
`ip_cidr` 规则看不到这些流量。需要捕获这类流量时启用：

```yaml
capture:
  default_route: true
```

启用前运行：

```sh
sudo tun-proxy check
```

### 配置 IPv6 后仍没有 Fake AAAA

查看日志或 `tun-proxy status` 中的 IPv6 fallback reason。常见原因包括出口网卡没有非 link-local IPv6 地址、系统没有 IPv6 默认路由，或默认路由不属于已配置出口。网络条件修复后需要重启服务。

### `service reload` 拒绝配置

先运行：

```sh
tun-proxy config validate
```

如果 YAML 有效但 reload 报某字段 `cannot be reloaded`，说明修改涉及主机网络、监听器、地址池或固定容量。可使用 `service upgrade -config ...` 事务性同步配置并重启，而不是继续尝试热重载。

### `config validate` 成功但 `check` 失败

`config validate` 只验证配置内容；`check` 还会检查真实主机环境。常见失败包括：

- `interface` 不存在或未启用；
- persistence/state/lock 父目录不存在、权限或所有者不安全；
- Fake IP 前缀与现有路由冲突；
- `dns.listen` 端口被占用；
- `capture.default_route: true` 时无法证明物理网关或上游 DNS 绕行路由。
