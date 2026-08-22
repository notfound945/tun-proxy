# CLI 与 YAML 配置

## 1. CLI 设计

```bash
tun-proxy interfaces
sudo tun-proxy check -config ./config.yaml
sudo tun-proxy run -config ./config.yaml
sudo tun-proxy status
sudo tun-proxy cleanup
sudo tun-proxy cleanup -clear-dns -config ./config.yaml
sudo tun-proxy cleanup -clear-fake-ip -config ./config.yaml
tun-proxy version
```

程序通过文件锁保证单实例运行。普通 `cleanup` 只恢复由状态日志明确记录、且当前值仍归本程序
所有的路由和 DNS 修改。状态文件丢失时，`-clear-dns` 可以保守地把完整 DNS 列表仍恰好等于
配置中 `dns.listen` 地址的已启用网络服务重置为自动/DHCP DNS；`-clear-fake-ip` 用于删除配置
对应的 Fake IP 快照和 WAL。两个 clear flag 共用一次实例锁，也可以同时使用。

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

# Optional Phase 8.4 literal-IP capture. Defaults to false.
capture:
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
  persistence_file: /var/lib/tun-proxy/fake-ip.yaml
  exclude:
    - localhost
    - "*.local"
    - "*.lan"

# Optional Phase 8.3 dual-stack mode. Both tun.ipv6_* fields are required when
# fake_ipv6 is present, and the point-to-point pair must be outside this CIDR.
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
    dns:
      - "1.1.1.1:53"
      - "8.8.8.8:53"
    connect_timeout: 10s
    fallback: reject

  wired:
    type: direct
    interface: en5
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

  - domain_suffix:
      - video.example
    outbound: wifi

  # Pure CIDR rules need capture.default_route: true to observe ordinary
  # real-IP/literal-IP traffic. Domain + CIDR rules can use the Fake-IP path.
  - ip_cidr:
      - 203.0.113.0/24
      - 2001:db8::/32
    outbound: wired

  - outbound: wifi
```

## 3. 加载与编译

- 使用 `yaml.Decoder.KnownFields(true)` 拒绝未知字段。
- 拒绝重复或不支持的配置版本。
- 启动前把 duration、地址、前缀和端口编译成强类型运行时配置。
- `rules[].ip_cidr` 只接受 canonical IPv4/IPv6 CIDR；同一规则内重复前缀去重，
  IPv4-mapped IPv6 与带 host bits 的非规范前缀拒绝加载。
- 纯 `ip_cidr` 规则要覆盖普通真实 IP/literal-IP 流量时必须启用
  `capture.default_route`；带显式域名条件的组合 CIDR 规则可由 Fake IP 路径捕获。
- 以 `*` 开头的 YAML 字符串必须加引号，例如 `"*.local"`。
- 原始 YAML 结构不得直接进入业务模块。
- 编译后的运行时配置应尽量不可变，并通过原子指针供新流读取。

## 4. 启动前校验

`check` 与 `run` 执行同一套校验：

- 配置版本、TUN 地址、Peer 和 MTU。
- Fake IP CIDR 与现有路由冲突。
- DNS 监听地址和端口占用。
- Outbound 接口是否存在并可用。
- DNS 服务器地址与端口。
- 规则引用和 fallback 循环。
- 默认规则位置。
- 启用 `capture.default_route` 时，每个 direct 出口 DNS 的 scoped 物理网关、主机旁路
  唯一所有权及地址族一致性。
- 域名、CIDR 和 Duration。
- 状态、锁和持久化目录。
- 当前用户权限。

`check` 只执行只读检查，不得创建接口、添加路由或修改 DNS。

## 5. 配置热更新

Beta 阶段通过 `SIGHUP` 执行：

```text
读取新 YAML
→ 严格解析
→ 完整校验
→ 编译规则和出口配置
→ 原子替换
```

允许热更新规则、fallback、上游 DNS、日志级别和超时。TUN IPv4/IPv6 地址、Fake IP/Fake IPv6
CIDR、地址池容量与持久化路径、MTU、DNS 监听地址、状态文件位置及默认路由捕获开关需要重启。
启用默认路由捕获后，Outbound 接口、DNS 与 fallback 拓扑也需要重启，以确保已记录旁路仍与
出口计划一致。热更新只影响
新流。`fake_ipv6` 整块可省略，此时 `tun.ipv6_address` 与 `tun.ipv6_peer` 也必须省略，AAAA
继续返回 NODATA。启用时程序先配置 utun IPv6、安装并验证 Fake IPv6 路由、启动双栈包泵，
随后才启动会返回 Fake AAAA 的本地 DNS，避免暴露没有数据路径的地址。
