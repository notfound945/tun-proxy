# 规则引擎、多网卡出口与独立 DNS

## 1. Flow Metadata

规则引擎接收纯数据，不执行网络 I/O：

```go
type FlowMetadata struct {
    Domain         string
    FakeIP         netip.Addr
    SourceIP       netip.Addr
    SourcePort     uint16
    DestinationPort uint16
    Protocol       string
}
```

字段可在实现时调整，但规则层不得依赖 gVisor 类型。

## 2. MVP 规则

支持 `domain`、`domain_suffix`、`protocol`、`dst_port` 和默认规则。

- 按 YAML 顺序，首个匹配生效。
- 一个新流只产生一个最终不可变决策；两阶段内部评估不允许会话中途改写。
- 默认规则必须位于最后。
- 热更新只影响新流。
- 结果包含 Outbound 名称和匹配规则标识。

Phase 8.5 已实现真实 IP CIDR 规则，使用两阶段决策：

```text
域名/协议/端口预匹配
→ 使用候选出口解析真实 IP
→ IP/CIDR 后匹配
→ 必要时切换出口并重新解析
```

- `ip_cidr` 可与 domain、domain_suffix、protocol、dst_port 组合，字段间为 AND。
- 一个规则包含多个 CIDR 时为 OR；域名存在多个 A/AAAA 候选时，任一地址命中即匹配。
- 预匹配保留满足非 IP 条件的 CIDR 延迟候选，并以首个不含 CIDR 的规则作为通常的
  解析候选；若该候选为 reject，则借用前面的首个 direct 候选完成解析判断。
- 后匹配仍严格按 YAML 顺序首个生效。若出口改变，使用新出口的独立 Resolver
  重新解析一次，但不再次匹配，防止策略振荡。
- 直接 IP flow 使用 literal destination 直接完成 CIDR 后匹配，不执行 DNS。
- fallback 只改变实际解析/连接路径，不改写已经冻结的规则 ID 与策略出口。

## 3. Outbound 接口

```go
type Outbound interface {
    DialContext(ctx context.Context, network string, dst Destination) (net.Conn, error)
    ListenPacket(ctx context.Context, dst Destination) (net.PacketConn, error)
}
```

初始类型：

- `direct`：绑定指定物理接口建立连接。
- `reject`：拒绝 TCP 或丢弃/拒绝 UDP。

后续可扩展 SOCKS、HTTP CONNECT 或加密隧道，但不能改变规则引擎对 Outbound 的抽象。

## 4. macOS 接口绑定

在 `connect()` 前设置：

- IPv4：`IP_BOUND_IF`
- IPv6：`IPV6_BOUND_IF`；Phase 8.1 已完成出口 socket 与 DNS 上游基础，完整 Fake
  IPv6 数据面在 Phase 8.2–8.3 接入

要求：

- 使用接口索引，不依赖接口名称推断类型。
- 创建连接前确认接口仍存在且 Up。
- 接口无有效路由时返回清晰错误，不静默改走默认接口。
- 用 `tcpdump` 验证真实流量出现在目标接口。

## 5. fallback

- fallback 引用在启动时解析成有向无环链。
- 只在接口不存在、无路由或连接超时等可恢复错误上 fallback。
- DNS NXDOMAIN、证书错误等业务错误不能随意触发 fallback。
- 每次 fallback 记录原出口、目标出口和错误类别。
- `reject` 是终止节点。

## 6. 每出口独立 Resolver

```text
规则选择 wired
→ wired Resolver 的 DNS Socket 绑定 wired
→ 得到真实 IP
→ 业务 Socket 同样绑定 wired
```

Resolver 支持显式上游、UDP、TC 后 TCP 回退、超时、有限重试、独立缓存、查询取消和接口消失快速失败。禁止使用已指向 `127.0.0.1` 的系统 Resolver 查询真实地址。

## 7. 域名与地址选择

- 保留原始域名直到出口连接建立完成。
- TLS 端到端透明，代理不解密 TLS。
- 多个 A 记录由出口排序、逐个尝试并聚合错误。
- DNS 缓存键至少包含域名、查询类型和 Outbound。

## 8. 接口变化

- 新流使用最新接口快照和规则。
- 已建立 TCP 流不迁移。
- 接口消失后新连接走 fallback 或 reject。
- 已建立 UDP 会话在接口消失时关闭。
- 接口恢复后可供新流选择，属于 Beta 目标。
