# CLI 与 YAML 配置

> 本文描述当前 CLI 和配置契约。IPv6、split-default/literal-IP 捕获、Fake IP 持久化、
> 热更新和 LaunchDaemon 权限分离均已实现；阶段编号只用于历史验收记录，不再表示这些能力
> 尚待开发。当前状态以源码和 [`STATUS.md`](STATUS.md) 为准。

## 1. CLI 设计

### 1.1 日常配置与诊断

```bash
tun-proxy interfaces

tun-proxy config -generate
tun-proxy config -finder
tun-proxy config validate
tun-proxy config validate -service -json

tun-proxy explain -domain example.com
tun-proxy explain -domain example.com -resolve -json
tun-proxy diagnose
sudo tun-proxy diagnose -json

tun-proxy version
```

- `config validate` 只做严格 YAML 解析和语义编译，不要求 root，也不读取或修改系统路由、DNS。
- `config validate -service` 额外校验托管服务固定路径契约，但仍是离线操作。
- `explain` 默认离线解释域名阶段、待决 CIDR 阶段、出口、接口、DNS 和 fallback；传入
  `-ip` 可模拟解析结果，`-resolve` 才会通过配置的接口绑定 DNS 发起真实解析。
- `diagnose` 始终只读；普通用户可获得部分结果，使用 `sudo` 才能读取 root-owned state、
  LaunchDaemon 状态和运行时 status socket。

### 1.2 前台运行与恢复

```bash
sudo tun-proxy check -config ./config.yaml
sudo tun-proxy run -config ./config.yaml
sudo tun-proxy status
sudo tun-proxy cleanup
sudo tun-proxy cleanup -clear-dns -config ./config.yaml
sudo tun-proxy cleanup -clear-fake-ip -config ./config.yaml
```

前台 `run` 由一个 root 进程持有系统事务和数据面。程序通过文件锁保证单实例运行。
普通 `cleanup` 只恢复状态日志明确记录、且当前值仍归本程序所有的路由和 DNS 修改。
状态文件丢失时，`-clear-dns` 可以保守地把完整 DNS 列表仍恰好等于配置中
`dns.listen` 地址的已启用网络服务重置为自动/DHCP DNS；`-clear-fake-ip` 用于删除配置
对应的 IPv4/IPv6 Fake IP 快照和 WAL。两个 clear flag 会先尝试普通恢复，共用一次实例锁，
也可以同时使用。

### 1.3 LaunchDaemon 托管模式

```bash
sudo tun-proxy service install
sudo tun-proxy service status
sudo tun-proxy service start
sudo tun-proxy service stop
sudo tun-proxy service restart
sudo tun-proxy service sync-user-config
sudo tun-proxy service reload -timeout 15s
sudo tun-proxy service logs -lines 200
sudo tun-proxy service upgrade
sudo tun-proxy service uninstall
```

托管模式由 root supervisor 持有 utun、53 端口 listener、路由、系统 DNS 和恢复状态，
由专用非 root `_tun-proxy` worker 运行 Fake DNS、Fake IP、规则、resolver、gVisor、TCP/UDP
relay 和 status socket。`check -service` 用于验证这套固定路径、账号和分离所有权。
`service install`、`upgrade`、`sync-user-config`、`reload` 和 `uninstall` 都按事务处理；普通 uninstall 默认保留
配置、映射和日志，只有显式 `-purge` 才删除托管数据。

## 2. YAML 示例

```yaml
version: 1

log:
  level: info
  format: text

system:
  state_file: /var/run/tun-proxy/state.json
  lock_file: /var/run/tun-proxy/tun-proxy.lock
  manage_dns: true

capture:
  # false：只捕获 Fake IP 前缀；true：使用 split-default 捕获普通真实 IP
  # 和 literal-IP 流量。默认 false。
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

# 可选双栈配置。启用时必须同时提供 tun.ipv6_address 和 tun.ipv6_peer，
# 且 point-to-point 地址不能落在此 Fake IPv6 CIDR 内。
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
      - "10.0.0.1:53"
    connect_timeout: 10s
    fallback: wifi

  reject:
    type: reject

rules:
  - domain:
      - example.com
    outbound: wired

  - domain_suffix:
      - company.com
      - internal.example
    outbound: wired

  # 纯 CIDR 规则要观察普通真实 IP/literal-IP 流量，必须启用
  # capture.default_route。带显式域名条件的组合规则可走 Fake IP 路径。
  - ip_cidr:
      - 203.0.113.0/24
      - 2001:db8::/32
    outbound: wired

  - outbound: wifi
```

`capture.default_route: false` 保持选择性 Fake IP 模式：只有需要域名策略的流量进入 Fake IP
前缀，普通解析结果不被全局捕获。设为 `true` 时，程序安装 IPv4 split-default；IPv6 运行时
能力 gate 通过后还会安装 IPv6 split-default。程序不删除系统原有 default route，而是通过
全局 utun `/1`、物理接口 scoped `/1` 和上游 DNS/网关旁路构造可证明无环的捕获拓扑。

配置了 `fake_ipv6` 不等于无条件返回 Fake AAAA。只有宿主存在可用的非链路本地 IPv6 地址、
物理 IPv6 默认路由且 utun IPv6 路径可以安全建立时，IPv6 数据面才会启用；否则 A/IPv4
继续工作，Fake AAAA 返回 NODATA。

## 3. 加载与编译

- 使用 `yaml.Decoder.KnownFields(true)` 拒绝未知字段。
- 拒绝重复或不支持的配置版本。
- 启动前把 duration、地址、前缀和端口编译成强类型运行时配置。
- `rules` 当前只接受 `domain`、`domain_suffix`、`ip_cidr` 和最终默认规则；不暴露进程、
  协议、端口、SNI 或 HTTP Host 条件。
- `rules[].ip_cidr` 只接受 canonical IPv4/IPv6 CIDR；同一规则内重复前缀去重，
  IPv4-mapped IPv6 与带 host bits 的非规范前缀拒绝加载。
- 域名与 CIDR 的组合规则使用两阶段决策：域名阶段决定候选，真实解析地址到达后再完成
  CIDR 判断；最终决定绑定到流和配置 generation，不因出口再次解析或 reload 改变。
- 纯 `ip_cidr` 规则要覆盖普通真实 IP/literal-IP 流量时必须启用
  `capture.default_route`；带显式域名条件的组合 CIDR 规则可由 Fake IP 路径捕获。
- direct 出口的 `dns_source` 支持 `dhcp` 和 `static`。`dhcp` 优先使用接口租约 DNS，
  配置中的 `dns` 是必需回退；`static` 始终使用配置列表。
- 以 `*` 开头的 YAML 字符串必须加引号，例如 `"*.local"`。
- 原始 YAML 结构不得直接进入业务模块；编译后的运行时配置按 generation 发布。

## 4. 校验和启动前检查

三类检查边界不同：

1. `config validate`：离线解析、编译、引用和路径契约检查，不要求 root，不接触宿主网络。
2. `check`：只读的实机 preflight，检查 root、目录/文件安全、接口状态、DHCP DNS、Fake IP
   前缀冲突、DNS listener 可用性、IPv6 capability gate，以及 default-route 旁路和 scoped
   路由计划；不会创建 utun、添加路由或修改系统 DNS。
3. `run` / 托管 supervisor：在 preflight 后按状态先记录、后变更的事务顺序应用 utun、路由
   和系统 DNS，并在任一步失败时反向回滚。托管模式使用 `check -service` 的 root/worker
   所有权模型。

配置编译和实机 preflight共同覆盖：

- 配置版本、TUN IPv4/IPv6 地址、Peer、MTU、队列和 buffer pool。
- Fake IPv4/IPv6 CIDR、容量、持久化路径和现有路由冲突。
- DNS 监听地址、UDP/TCP listener、默认出口和并发限制。
- direct 出口接口、DNS 源、DNS 服务器、fallback 引用及循环。
- 规则引用、默认规则位置、域名、CIDR 和 duration。
- 启用 `capture.default_route` 时，每个 direct 出口的同族物理网关、scoped `/1`、主机旁路
  唯一所有权，以及安装前后的无环出口可证明性。
- state、lock、IPv4/IPv6 mapping 路径的文件类型、权限与所有者。

## 5. 配置热更新

前台模式响应 `SIGHUP`；托管模式推荐使用：

```bash
sudo tun-proxy service reload
sudo tun-proxy service reload -user-config
sudo tun-proxy service reload -config ./config.yaml -timeout 15s
sudo tun-proxy service sync-user-config
```

服务运行时，托管 reload 会先校验并事务性安装配置，再通过 root-only
`/var/run/tun-proxy/control.sock` 发送期望配置摘要，等待 supervisor/worker 的最终结果。CLI 不再
通过 status counters 推断请求结果；运行时拒绝、摘要不一致或超时会回滚已安装配置，并通过同一
control socket 确认旧运行配置恢复。`SIGHUP` 仅保留为手工兼容入口。reload 成功后只影响新建
DNS 查询和 TCP/UDP 流，已有流保留其原 generation、规则决定和出口。`service reload` 无论
是否带配置参数都要求服务处于 `running` 阶段。

`service sync-user-config` 安全读取并校验调用用户的默认配置。服务停止时，它会先卸载仍注册的
launchd job，原子同步配置并保持停止；服务运行时，它会先停止服务，再同步配置并重新启动。
新配置启动失败时会回滚托管配置并尝试恢复旧服务。该路径可以修正 TUN、网口等通常不可热
重载的字段，也可以解除错误配置造成的启动失败。

可热更新的内容包括规则、日志配置、`fake_ip.dns_ttl`/排除项、默认 DNS 出口、上游 DNS、
connect timeout、fallback 和 UDP idle timeout 等。以下字段必须重启：

- state/lock 路径和 `system.manage_dns`；
- `capture.default_route`；
- TUN IPv4/IPv6 地址、Peer、MTU、队列和 buffer pool；
- Fake IPv4/IPv6 CIDR、mapping TTL、容量和持久化路径，以及启用/禁用整块 `fake_ipv6`；
- DNS listen、UDP/TCP 开关和并发上限；
- TCP/UDP 全局与单源容量上限。

当 `capture.default_route: true` 时，整个 outbounds 拓扑也不可热更新，因为接口、DNS 和
fallback 共同决定已经安装的物理 scoped split route 与旁路计划；要改变它们必须重启并
重新证明捕获拓扑。`default_route: false` 时，合法的 outbound 更新可以随新 generation
生效。
