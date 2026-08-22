# Fake DNS 与 Fake IP

## 1. DNS 数据流

```text
应用查询域名
    ↓
系统 DNS 指向 127.0.0.1
    ↓
是否命中显式 domain / domain_suffix 规则？
    ├─ 是 → 分配 Fake IP → 保存域名映射 → 流量因 Fake IP 路由进入 utun
    └─ 否 → dns.default_outbound 的 DHCP/static DNS 返回真实 IP → 默认网卡直接出站
```

Fake IP 只是规则域名的本地代号，不能直接发送到公网。`fake_ip.exclude` 的优先级高于
域名规则，命中后始终通过真实上游 DNS 返回真实地址。

## 2. Fake DNS 服务

- 同时监听 `127.0.0.1:53` UDP 和 TCP。
- DNS 监听成功后才允许修改系统 DNS。
- 命中显式 `domain` / `domain_suffix` 规则的 A 查询返回 Fake IPv4。
- 对规则域名，未配置 `fake_ipv6` 时 AAAA 返回 NODATA；配置完整双栈链路后，A 与 AAAA
  分别从独立的 IPv4/IPv6 地址池分配。
- 未被域名规则选中的普通 A/AAAA 查询，以及所有非 A/AAAA 查询，都通过
  `dns.default_outbound` 的显式上游 DNS 转发并保留真实答案。
- DNS 阶段没有协议、端口和已解析 IP 元数据，因此纯 `protocol`、`dst_port`、`ip_cidr`
  规则不会触发 Fake IP。`capture.default_route: false` 时，这些普通真实 IP 流量绕过 TUN；
  要让此类规则覆盖普通流量，必须启用 `capture.default_route: true`。
- 禁止使用系统默认 Resolver 作为上游，否则会产生递归循环。
- 上游查询具有超时、有限重试、UDP 截断后的 TCP 回退和容量限制。

## 3. 排除域名

默认建议排除：

- `localhost`
- `*.local`
- 用户配置的局域网域名

排除域名通过真实上游 DNS 返回真实答案，不分配 Fake IP，并且该行为优先于域名规则。
`capture.default_route: false` 时，这些真实地址不会进入 TUN。

## 4. 地址池规则

- 默认池为 `198.18.0.0/15`。
- 保留内部使用地址以及池边界地址。
- 同一域名在映射有效期内返回稳定 Fake IP。
- 不同活跃域名不能共享同一个 Fake IP。
- 映射支持双向 O(1) 查询。
- 地址池耗尽时返回明确 DNS 错误并记录指标，不能复用活跃地址。

## 5. 生命周期

区分：

- DNS TTL：应用和系统缓存 Fake IP 的时间。
- Mapping TTL：代理内部保留双向映射的时间。

Mapping TTL 必须显著长于 DNS TTL。每个活跃 TCP/UDP 会话持有映射引用，引用未归零时禁止回收。

```text
映射超过 Mapping TTL
AND 活跃引用数为 0
AND 不在持久化保护窗口内
→ 允许回收
```

## 6. 持久化

MVP 可以先使用内存映射，Beta 增加持久化：

- 临时文件写入完整快照。
- `fsync` 后原子替换。
- 文件由 root 所有并限制权限。
- 启动时验证 CIDR、版本和重复地址。
- 损坏文件应隔离并重建，不得造成错误复用。

持久化用于避免应用仍缓存旧 Fake IP，而程序重启后已把该地址分配给另一域名。

## 7. 并发与容量

- 地址分配和双向映射更新必须原子完成。
- 并发查询同一域名只能产生一个 Fake IP。
- 配置最大映射数和高水位告警。
- 暴露池容量、已使用数、活跃数、回收数和耗尽次数指标。

## 8. 后续扩展

- IPv6 Fake IP 池与 Fake AAAA（Phase 8.3；需要物理出口具备可用 IPv6 才能访问公网
  IPv6 目标，不提供 NAT64）。
- DNS 缓存和负缓存。
- 更完整的 CNAME、HTTPS/SVCB 处理。
- DNSSEC 兼容策略。
- DoH/DoT 绕过检测或显式策略。
