# 范围、架构与技术选型

## 1. 目标数据路径

```text
应用 DNS 查询
    ↓
127.0.0.1:53 Fake DNS
    ↓ 返回 198.18.0.0/15 中的 Fake IP
应用连接 Fake IP
    ↓
macOS 路由表
    ↓
utun
    ↓
gVisor netstack
    ↓ 还原 TCP 字节流或 UDP 会话
Fake IP → 域名
    ↓
规则引擎
    ├── 指定 Wi-Fi 网卡
    ├── 指定有线网卡
    └── Reject
    ↓
绑定指定接口的真实 Socket
    ↓
目标服务器
```

## 2. MVP 范围

### 2.1 必须支持

- 创建并读写 macOS `utun`。
- 为 Fake IP 网段安装路由。
- 在 `127.0.0.1:53` 同时提供 UDP/TCP Fake DNS。
- 为不同域名分配不同的 Fake IPv4。
- 维护域名与 Fake IP 的双向映射。
- 使用 gVisor netstack 处理 TCP。
- 支持精确域名、域名后缀、协议、目标端口和默认规则。
- 使用 `IP_BOUND_IF` 将出口 Socket 绑定到配置的 macOS 网卡。
- 为每个出口配置独立的上游 DNS。
- 支持出口不可用时的 `fallback` 或 `reject`。
- 保存并恢复系统 DNS、路由和运行状态。
- 处理 `SIGINT`、`SIGTERM` 和启动失败回滚。
- 提供接口枚举、配置检查、启动和清理命令。

### 2.2 明确不支持

- 图形界面；项目只提供 CLI，不规划 UI。
- Apple NetworkExtension。
- IPv6 Fake IP。
- 全局默认路由接管。
- 直接 IP 连接的透明分流。
- 按进程或应用分流。
- HTTP Host、TLS SNI、QUIC SNI 嗅探。
- 已建立 TCP 连接跨网卡迁移。
- 自研 TCP/IP 协议栈。
- 二层 TAP、ARP 或以太网桥接。

## 3. 技术选型

| 模块 | 方案 |
|---|---|
| Go 版本 | 项目创建时的当前稳定版本 |
| TUN | `golang.zx2c4.com/wireguard/tun`；若 Spike 发现不适配，再实现最小 Darwin utun 封装 |
| 用户态 TCP/IP 栈 | `gvisor.dev/gvisor/pkg/tcpip/...` |
| YAML | `go.yaml.in/yaml/v3` |
| DNS | `github.com/miekg/dns` |
| Darwin 系统调用 | `golang.org/x/sys/unix` |
| IP/CIDR | 标准库 `net/netip` |
| 日志 | 标准库 `log/slog` |
| 路由和接口配置 | MVP 由 Go 以绝对路径调用 macOS 自带工具 |
| 配置热更新 | MVP 后通过 `SIGHUP` 原子替换规则 |

## 4. 代码目录规划

```text
tun-proxy/
├── cmd/
│   └── tun-proxy/
│       └── main.go
├── configs/
│   └── example.yaml
├── internal/
│   ├── app/                 # 生命周期编排
│   ├── config/              # YAML、校验、运行时配置编译
│   ├── daemon/              # 信号、PID、文件锁
│   ├── system/
│   │   ├── dns_darwin.go    # 系统 DNS 快照、修改和恢复
│   │   ├── route_darwin.go  # 路由安装和恢复
│   │   ├── state.go         # 恢复日志
│   │   └── privilege.go     # root 权限检查
│   ├── interfaceinfo/       # 网卡枚举和状态
│   ├── tun/                 # utun 创建和数据泵
│   ├── netstack/            # gVisor 适配、TCP/UDP Forwarder
│   ├── fakedns/             # 本地 DNS 服务
│   ├── fakeip/              # Fake IP 地址池和双向映射
│   ├── rules/               # 规则编译和匹配
│   ├── resolver/            # 每出口独立 DNS Resolver
│   ├── outbound/            # 网卡绑定 Dialer/PacketConn
│   ├── session/             # TCP/UDP 会话和生命周期
│   └── metrics/             # 运行统计
├── scripts/
├── go.mod
├── go.sum
├── README.md
└── docs/
    ├── PLAN.md
    └── plan/
```

## 5. 依赖边界

- `app` 负责生命周期编排，不实现协议细节。
- `config` 只输出经过完整校验的不可变运行时配置。
- `system` 是系统变更的唯一入口，其他模块不能直接执行系统命令。
- `netstack` 不直接选择接口，只通过抽象的 `Outbound` 建立出口。
- `rules` 是纯逻辑模块，不执行 DNS 或网络 I/O。
- `resolver` 与 `outbound` 共享接口绑定策略，但不共享可变连接状态。
- 所有 gVisor 类型限制在 `internal/netstack` 中，防止依赖扩散。

