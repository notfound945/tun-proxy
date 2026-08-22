# 范围、架构与技术选型

> 本文已按当前实现更新。早期 MVP 的历史边界和分阶段验收记录保留在
> [实施阶段](07-implementation-phases.md) 与 [phase 文档](../phases/) 中；当前能力以源码和
> [STATUS.md](STATUS.md) 为准。

## 1. 当前数据路径

### 1.1 选择性 Fake IP 捕获

`capture.default_route: false` 时，只为命中显式 `domain` / `domain_suffix` 规则的域名
分配 Fake IP：

```text
应用 DNS 查询
    ↓
127.0.0.1:53 Fake DNS
    ├── 命中显式域名规则 → 返回 Fake IPv4/IPv6
    └── 未命中或被 exclude → 通过 dns.default_outbound 返回真实答案

应用连接 Fake IP
    ↓
macOS Fake IP 前缀路由
    ↓
utun
    ↓
gVisor netstack 还原 TCP 流或 UDP 会话
    ↓
Fake IP → 原始域名
    ↓
域名预匹配 → 出口独立 DNS → CIDR 后匹配
    ↓
绑定指定物理接口的真实 Socket
    ↓
目标服务器
```

### 1.2 split-default 捕获

`capture.default_route: true` 时，除 Fake IP 前缀外，还通过 IPv4
`0.0.0.0/1`、`128.0.0.0/1` 和运行时允许时的 IPv6 `::/1`、`8000::/1`
接管普通真实 IP 与 literal-IP 流量。程序会为每个 direct 出口的 DNS 和网关规划
接口作用域旁路；旁路拓扑无法证明安全时拒绝启动，运行中拓扑变化且无法原子刷新时安全停止并回滚。

## 2. 当前支持范围

### 2.1 已实现

- 创建并读写 macOS `utun`，使用有界数据泵接收和发送 IPv4/IPv6 数据包。
- 安装 Fake IPv4 路由，以及通过运行时能力门控的 Fake IPv6 路由。
- 可选安装 split-default 路由，透明捕获普通真实 IP 和 literal-IP 流量。
- 在 IPv4 loopback 端口 53 同时提供 UDP/TCP Fake DNS。
- 只对显式域名规则分配 Fake IP，其他查询使用默认出口的独立上游 DNS 返回真实答案。
- 持久化 Fake IPv4/IPv6 双向映射；损坏快照会被隔离，WAL 与快照支持安全恢复。
- 使用 gVisor netstack 处理 TCP 和 UDP。
- 支持 `domain`、`domain_suffix`、`ip_cidr` 和最终默认规则；规则按顺序首个匹配。
- 对域名 + CIDR 规则执行两阶段解析与匹配，并冻结每个流的最终决定。
- 使用 `IP_BOUND_IF` / `IPV6_BOUND_IF` 将 DNS 和业务 Socket 绑定到同一物理网卡。
- 支持每出口 DHCP/static DNS、独立缓存、`fallback` 与 `reject`。
- 保存并事务恢复系统 DNS、路由、TUN、运行状态和实例锁。
- 支持前台 root 运行，以及 root supervisor + `_tun-proxy` 非 root worker 的 LaunchDaemon 托管模式。
- 支持原子配置热更新；旧 generation 服务既有流，新 generation 服务新流。
- 提供配置生成/校验、预检、规则解释、诊断、状态、清理、服务生命周期、日志和事务升级 CLI。
- 提供运行时 DNS、TCP、UDP、Fake IP、TUN、reload、network 和资源指标。

### 2.2 当前明确不支持

- 图形界面；项目只提供 CLI，不规划 UI。
- Apple NetworkExtension。
- 按进程或应用确定性分流。Phase 8.6 证明 `libproc` 轮询在进程退出和跨进程 UDP
  `SO_REUSEPORT` 下不能提供可靠归属，因此不对外暴露进程规则。
- HTTP Host、TLS SNI、QUIC SNI 嗅探或 TLS 中间人。
- 已建立 TCP 连接跨网卡迁移；网卡或规则变化只影响新流。
- 自研 TCP/IP 协议栈。
- 二层 TAP、ARP 或以太网桥接。
- SOCKS、HTTP CONNECT、远程加密隧道等代理出口；当前仅有 `direct` 和 `reject`。
- NAT64；Fake IPv6 公网访问要求物理出口本身具备可用 IPv6。

## 3. 技术选型

| 模块 | 当前方案 |
|---|---|
| Go | `go.mod` 固定项目要求的 Go 工具链版本 |
| TUN | `golang.zx2c4.com/wireguard/tun` 的 Darwin utun 实现 |
| 用户态 TCP/IP 栈 | `gvisor.dev/gvisor/pkg/tcpip/...`，依赖限制在 `internal/netstack` |
| YAML | `go.yaml.in/yaml/v3`，启用严格未知字段校验 |
| DNS | `github.com/miekg/dns` |
| Darwin 系统调用 | `golang.org/x/sys/unix` |
| IP/CIDR | 标准库 `net/netip` |
| 日志 | 标准库 `log/slog`，支持 text/json |
| 路由和系统 DNS | Go 以绝对路径调用 macOS 系统工具，不经过 shell |
| 配置热更新 | SIGHUP / 托管 IPC 驱动的 prepare + atomic commit |
| 托管服务 | system LaunchDaemon、固定系统路径、root supervisor + 非 root worker |
| 运行状态 | 原子状态文件、实例锁、受权限保护的 Unix status socket |

## 4. 当前代码目录

```text
tun-proxy/
├── cmd/tun-proxy/           # CLI、诊断和托管服务命令
├── configs/                 # 示例和开发配置
├── internal/
│   ├── app/                 # 前台/托管生命周期、数据面 generation、网络刷新
│   ├── config/              # 严格 YAML、语义编译、reload 约束
│   ├── daemon/              # 单实例锁
│   ├── defaultconfig/       # 内置默认配置
│   ├── domainname/          # 域名规范化和后缀边界
│   ├── fakedns/             # UDP/TCP Fake DNS 与真实查询转发
│   ├── fakeip/              # IPv4/IPv6 地址池、引用和持久化
│   ├── interfaceinfo/       # 网卡枚举
│   ├── launchservice/       # LaunchDaemon 安装、升级和固定布局
│   ├── netstack/            # gVisor TCP/UDP 适配
│   ├── outbound/            # 接口绑定 TCP/UDP Socket
│   ├── privsep/             # supervisor/worker 握手、IPC 和描述符移交
│   ├── procattrib/          # Phase 8.6 诊断性进程归属实验，不参与生产规则
│   ├── resolver/            # 每出口独立 DNS Resolver
│   ├── rules/               # 纯逻辑规则引擎
│   ├── session/             # TCP/UDP 决策、fallback 和 relay
│   ├── status/              # Unix socket 实时状态与资源指标
│   ├── system/              # DNS、路由、状态和事务化系统操作
│   └── tun/                 # utun 设备、缓冲池和包泵
├── docs/phases/             # 分阶段设计、实验和验收记录
├── docs/plans/              # 当前架构约束与总体计划
└── scripts/                 # 构建、安装、更新和 soak 脚本
```

## 5. 依赖边界

- `app` 负责生命周期和组件编排，不实现 DNS、规则或协议细节。
- `config` 输出完整校验后的运行时配置；不可热替换字段由 `ValidateReload` 统一约束。
- `system` 是宿主 DNS、路由和恢复状态变更的唯一入口。
- `launchservice` 管理安装布局和 launchd 生命周期；`privsep` 只负责可信 supervisor/worker 协议。
- root supervisor 拥有系统变更和特权描述符，非 root worker 拥有 Fake DNS、规则、netstack、resolver 和 relay。
- `netstack` 不选择出口，只把纯 Go flow 元数据和连接交给 `session`。
- `rules` 不执行 DNS 或网络 I/O；两阶段解析与 fallback 由 `session` 编排。
- `resolver` 与 `outbound` 使用相同接口绑定策略，但每个出口保持独立缓存和连接状态。
- 所有 gVisor 类型限制在 `internal/netstack`，避免第三方 API 扩散。
