# 规则引擎、多网卡出口与独立 DNS

> 本文描述当前 Phase 8.5 之后的规则语义。早期 `protocol` / `dst_port` 规则已经删除；
> 当前只支持域名、域名后缀、目标 CIDR 和最终默认规则。

## 1. Flow Metadata

规则引擎接收纯数据，不执行网络 I/O：

```go
type FlowMetadata struct {
    Domain        string
    FakeIP        netip.Addr
    DestinationIP netip.Addr
    SourceIP      netip.Addr
    SourcePort    uint16
}
```

- Fake-IP 流量同时保留 `Domain` 与 Fake 地址。
- default-route 捕获的 literal-IP 流量没有域名，直接以真实目标地址完成 CIDR 匹配。
- 规则层不依赖 gVisor、resolver 或 Socket 类型。

## 2. 当前规则

支持 `domain`、`domain_suffix`、`ip_cidr` 和默认规则：

- 按 YAML 顺序，首个匹配生效。
- 最后一条必须是只包含 `outbound` 的无条件默认规则。
- `domain` 精确匹配规范化域名。
- `domain_suffix` 按 DNS label 边界匹配自身及子域名，不匹配字符串伪后缀。
- 同一规则内多个 domain/suffix/CIDR 分别为 OR；不同字段之间为 AND。
- 一个 CIDR 规则在任一解析地址命中任一前缀时成立。
- IPv4-mapped IPv6、带 host bits 的非规范 CIDR 和重复/未知字段在配置阶段拒绝或规范化处理。
- `protocol` 和 `dst_port` 不受支持，严格 YAML 会拒绝旧字段。
- 一个新流只产生一个最终不可变决定；reload 只影响新流。

## 3. 选择性 Fake IP 与 CIDR 两阶段决策

Fake DNS 只依据显式 `domain` / `domain_suffix` 条件决定是否分配 Fake IP，因为 DNS 阶段
尚无真实地址：

- 纯 `ip_cidr` 规则要覆盖普通真实 IP 或 literal-IP 流量，必须启用
  `capture.default_route: true`。
- 域名 + CIDR 组合规则在选择性模式下仍可生效：域名条件先触发 Fake IP，数据面再解析真实地址。

域名流使用两阶段决策：

```text
按顺序收集满足非 IP 条件的 CIDR 延迟候选
→ 找到首个不含 CIDR 的结论规则
→ 通过可解析候选出口查询真实 A/AAAA
→ 按完整 YAML 顺序执行 CIDR 后匹配
→ 若最终策略出口改变，使用最终出口独立 Resolver 再解析一次
→ 冻结 RuleID 和策略 Outbound，不再重新匹配
```

若结论规则是 reject，但前面存在可能因 CIDR 胜出的 direct 候选，会借用第一个可解析候选完成
地址判断。第二次解析不再次触发规则匹配，避免不同出口 DNS 答案造成策略振荡。

fallback 只影响实际解析或连接路径，不改写已经冻结的 RuleID 和策略 Outbound。

## 4. Outbound

当前内部路由抽象等价于：

```go
type Dialer interface {
    DialContext(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error)
}

type PacketDialer interface {
    DialPacket(ctx context.Context, dst netip.AddrPort) (net.Conn, error)
}
```

当前配置类型：

- `direct`：拥有接口绑定 Resolver、TCP Dialer、UDP PacketDialer 和可选 fallback。
- `reject`：终止规则，不允许配置接口、Resolver 或 fallback。

SOCKS、HTTP CONNECT 和加密隧道尚未实现；未来扩展不得改变规则引擎的无 I/O 边界和单流
不可变决策语义。

## 5. macOS 接口绑定

每个新 Socket 在 `connect(2)` 前按确定地址族设置：

- IPv4：`IP_BOUND_IF`
- IPv6：`IPV6_BOUND_IF`

接口名在每次创建 Socket 时重新解析为系统索引，并确认接口仍为 Up。接口不存在、Down 或
地址族不确定时返回显式错误，不允许静默使用系统默认接口。真实 DNS Socket 与业务 Socket
必须使用相同绑定策略。

## 6. fallback

- fallback 引用和环路在配置编译时校验。
- 只对接口不存在/Down、无路由、网络或主机不可达、地址不可用和超时等可恢复环境错误尝试。
- context 取消、策略 reject 和 DNS NXDOMAIN 等业务错误为终止错误。
- Resolver fallback 与连接 fallback 都保留完整错误链。
- `reject` 是终止节点，不能再 fallback。

## 7. 每出口独立 Resolver

```text
规则选择 wired
→ wired Resolver 的 DNS Socket 绑定 wired
→ 得到 wired 视角的真实地址
→ wired TCP/UDP Socket 同样绑定 wired
```

- 每个 direct 出口创建独立 Resolver 和 TTL 缓存，答案不跨出口共享。
- A 与 AAAA 使用独立 cache key。
- `dns_source: dhcp` 优先采用该接口实时发现的 DHCP DNS，静态列表作为发现失败时的回退。
- `dns_source: static` 始终使用配置列表。
- 支持多个上游、UDP、TC 后 TCP、超时、取消和并发限制。
- 禁止调用已经指向 `127.0.0.1` Fake DNS 的系统默认 Resolver。

## 8. 域名与地址选择

- 原始规范化域名保留到最终出口连接完成。
- TLS 端到端透明，项目不解密、不读取 Host/SNI。
- Resolver 返回全部同族 A/AAAA 地址，连接层按返回顺序逐个尝试并聚合错误。
- direct-IP flow 不执行 DNS，使用 literal destination 完成 CIDR 后匹配和连接。
- Fake IPv6 只在宿主 IPv6 能力门控通过时对外提供，且不提供 NAT64。

## 9. reload 与接口变化

- reload 先构造完整 next generation，成功后原子切换。
- 新流使用最新规则、接口 DNS 和 route；既有 TCP/UDP 会话保持原 generation。
- 运行时周期性重新发现 DHCP DNS；选择性捕获模式可刷新新 generation 和 Fake DNS 上游。
- `capture.default_route: true` 时，outbound 接口/DNS/fallback 拓扑不可热更新，因为它参与已安装旁路。
- split-default 运行中若旁路拓扑需要重建，服务安全停止并回滚，等待显式重启。
