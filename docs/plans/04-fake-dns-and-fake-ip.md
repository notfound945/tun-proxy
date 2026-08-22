# Fake DNS 与 Fake IP

> 本文描述当前选择性 Fake DNS、双栈地址池和持久化实现。早期“仅内存 IPv4、IPv6 后续实现”
> 的描述已经不再适用。

## 1. DNS 数据流

```text
应用查询域名
    ↓
系统 DNS 指向 127.0.0.1
    ↓
是否命中显式 domain / domain_suffix 规则？
    ├─ 是 → 按查询族分配 Fake IP → 持久化映射 → 流量经 Fake 前缀路由进入 utun
    └─ 否 → dns.default_outbound 的 DHCP/static DNS 返回真实答案
```

Fake IP 是规则域名的本地身份，不能直接发送到公网。`fake_ip.exclude` 优先于域名规则，
命中后始终通过真实上游 DNS 返回真实地址。

## 2. Fake DNS 服务

- 配置监听地址必须是非零 IPv4 loopback；托管服务要求同时启用 UDP 和 TCP 监听。
- DNS listener 成功启动后才允许修改系统 DNS；停止时先恢复系统 DNS，再关闭 listener。
- 命中显式 `domain` / `domain_suffix` 规则的 A 查询返回 Fake IPv4。
- 配置 `fake_ipv6` 且运行时 IPv6 能力门控通过时，规则域名的 AAAA 查询返回 Fake IPv6。
- 未配置 `fake_ipv6` 或能力门控未通过时，规则域名的 AAAA 返回 NODATA，不形成半实现 IPv6 路径。
- 未被域名规则选中的普通 A/AAAA 查询，以及非 A/AAAA 查询，通过
  `dns.default_outbound` 的显式上游 DNS 转发并保留真实答案。
- DNS 阶段没有真实目标 IP，因此纯 `ip_cidr` 规则不会触发 Fake IP。
- `capture.default_route: false` 时，普通真实 IP 和 literal-IP 绕过 TUN；要让纯 CIDR 规则覆盖
  这些流量必须启用 `capture.default_route: true`。域名 + CIDR 组合规则仍可通过 Fake IP 进入 TUN。
- 禁止使用已经指向本地 Fake DNS 的系统 Resolver 查询真实地址，防止递归。
- 上游支持查询超时、有限并发、多个显式服务器、UDP 截断后的 TCP 回退和请求取消。

## 3. 排除域名

默认建议排除：

- `localhost`
- `*.local`
- 用户配置的局域网域名

域名在匹配前会规范化，后缀按 DNS label 边界判断。排除规则返回真实上游答案且不分配 Fake IP，
优先级高于显式域名分流规则。

## 4. IPv4/IPv6 地址池规则

- 默认 IPv4 池为 `198.18.0.0/15`；IPv6 池由 `fake_ipv6.cidr` 显式配置。
- IPv4 与 IPv6 使用独立 Pool、映射文件、容量和统计。
- 保留池首部内部地址和边界地址，TUN 点对点地址不得落入 Fake 前缀。
- 同一域名在映射有效期内返回稳定的同族 Fake IP。
- 不同活跃域名不能共享同一个 Fake IP。
- 映射支持域名到地址、地址到域名的并发 O(1) 查询。
- 地址池耗尽时返回明确 DNS 错误并增加耗尽/失败指标，不复用仍活跃的地址。
- Fake IPv6 不提供 NAT64；访问公网 IPv6 目标要求对应物理出口具备可用 IPv6。

## 5. 生命周期与会话引用

区分：

- DNS TTL：应用和系统缓存 Fake IP 的时间。
- Mapping TTL：代理内部保留双向映射的时间。
- persistence protection window：重启加载后至少保护旧映射的时间，避免客户端仍持有旧 DNS 答案。

Mapping TTL 必须显著长于 DNS TTL。每个 Fake-IP TCP/UDP 会话获取映射引用，结束时释放；
普通 literal-IP 流量不持有 Fake IP 引用。

```text
映射超过 Mapping TTL
AND 活跃引用数为 0
AND 不在持久化保护窗口内
→ 允许回收
```

## 6. 当前持久化实现

Fake IPv4/IPv6 映射都已经持久化：

- 启动时加载快照和 WAL，并校验版本、地址族、CIDR、容量、重复域名/地址和过期时间。
- 分配、替换和删除更新先写 WAL；达到条件后压缩为完整快照。
- 完整快照通过受限临时文件、同步和原子替换提交。
- 加载后的映射应用保护窗口，避免 DNS TTL 内错误复用旧地址。
- 损坏或不兼容的持久化文件会被隔离，服务以干净地址池继续启动并记录告警。
- 前台模式通常由 root 写入；托管模式由固定 `_tun-proxy` worker 在受限数据目录写入。
- `cleanup -clear-fake-ip` 可以显式删除 IPv4/IPv6 快照和对应 WAL，但运行实例存在时拒绝。

## 7. 并发与容量

- 地址分配和双向索引更新原子完成。
- 并发查询同一域名只产生一个同族 Fake IP。
- DNS 使用固定并发信号量；达到上限快速返回失败并记录 capacity reject。
- 地址池配置最大映射数，UDP/TCP 会话另有独立容量限制。
- status socket 暴露池容量、当前映射、活跃引用、回收、分配失败、DNS Fake/forward/NODATA 等指标。

## 8. 仍未实现的扩展

- 完整的本地 DNS 正缓存和负缓存；当前真实地址缓存位于每个出口 Resolver 中。
- 更完整的 HTTPS/SVCB 策略和复杂 DNSSEC 语义处理。
- DoH/DoT 识别或显式阻断策略；应用绕过系统 DNS 时，选择性 Fake DNS 无法单独恢复域名上下文。
- NAT64 或 DNS64。
