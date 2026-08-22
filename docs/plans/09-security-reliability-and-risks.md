# 安全、可靠性与风险控制

> 本文已按 Phase 8.7b 权限分离、双栈和 split-default 当前实现更新。早期“整个托管服务长期以
> root 运行、IPv6/default route 后续实现”的描述不再适用。

## 1. 权限模型

项目有两种执行模型：

### 1.1 前台模式

`sudo tun-proxy run` 仍以 root 运行，适合开发、诊断和直接运行。它需要创建 utun、绑定
53 端口、修改系统 DNS/路由和写入默认系统状态路径。

### 1.2 托管模式

system LaunchDaemon 使用最小权限分离：

- root supervisor 负责恢复陈旧状态、获取锁、创建 utun、预绑定 DNS listener、修改/恢复
  系统 DNS 与路由、管理 root 状态和监督 worker。
- `_tun-proxy` worker 从首条业务指令起保持非 root、清空附加组，运行 Fake DNS、Fake IP
  持久化、规则、resolver、gVisor netstack 和 TCP/UDP relay。
- 特权文件描述符通过受控继承移交；supervisor/worker 使用版本化、限长、带请求 ID 和配置摘要
  校验的内部协议完成 prepare、commit、reload 和 shutdown。
- worker 只写固定的 worker runtime/data 路径和 status socket，不拥有系统配置、plist、helper
  binary、状态文件或实例锁。

共同约束：

- 只读取严格校验的本地 YAML；不支持插件、脚本钩子或配置内任意命令。
- 固定托管路径验证文件类型、所有者、权限和符号链接安全。
- 日志不记录业务数据正文、TCP/UDP payload 或完整 DNS 原始报文。

## 2. 系统命令安全

- 只调用固定绝对路径。
- 使用参数数组，不经过 shell。
- 校验接口名、CIDR、地址、端口、网络服务名和文件路径。
- 所有命令设置上下文超时。
- 记录退出状态和受限长度的标准错误。
- 不依赖调用者 `HOME` 保存托管运行状态。
- 托管服务配置只能引用固定 state、lock、Fake IPv4/IPv6 和 status 路径，避免 root 任意路径写入。

## 3. 文件与 IPC 安全

- 配置、binary、plist、日志、状态、锁、映射和 socket 都有固定类型、owner 和 mode 约束。
- 创建/替换配置和 binary 使用同目录临时文件、同步、原子 rename 和失败回滚。
- 日志读取拒绝符号链接和非普通文件，tail 窗口与最大行数有上限。
- Unix status socket 位于 worker 专属目录；查询和 stale cleanup 验证允许的所有者。
- privilege-separation frame 有协议版本、类型白名单、最大长度和 payload 校验。
- reload 配置内容与声明摘要必须一致，避免 supervisor 和 worker 使用不同配置。

## 4. 恢复安全

- 修改系统前创建恢复状态，后续每个已完成步骤及时持久化。
- 状态文件使用原子写入；失败时不丢弃仍需恢复的信息。
- 正常退出、启动失败和 worker 崩溃都由拥有宿主事务的进程执行反向回滚。
- `cleanup` 只恢复状态文件明确记录且仍满足所有权/当前值约束的对象。
- 恢复失败时保留状态文件，不执行宽泛路由或 DNS 删除。
- DNS 故障或退出时优先恢复系统 DNS，再停止 Fake DNS。
- `-clear-dns` 只在完整 DNS 列表仍是单个配置 listener 时重置自动 DNS。
- `-clear-fake-ip` 只删除配置指定的快照和 WAL，并受同一实例锁保护。

## 5. 网络与资源安全

- Fake DNS 只监听 IPv4 loopback；托管服务要求 UDP/TCP 同时启用。
- DNS 和真实出口都有超时、取消和并发限制。
- 限制 TCP flow、UDP 总会话、单源 UDP、Fake IP 映射、DNS 并发、TUN 队列和缓冲池。
- 达到上限时快速失败，不建立无界队列或无限分配。
- 不实施 TLS 中间人、Host/SNI 嗅探或 payload 检查。
- IPv6 在宿主默认路由和物理能力不足时安全禁用，Fake AAAA 返回 NODATA。
- split-default 启动前必须证明出口 DNS 和网关具有正确接口旁路。
- 运行中旁路拓扑发生不兼容变化时安全停止并回滚，不静默改走错误出口。

## 6. 主要风险

### R1：指定接口绑定不符合预期

控制：每个 Socket 建立前重新解析接口并设置 `IP_BOUND_IF` / `IPV6_BOUND_IF`；接口不存在或
Down 时返回可分类错误。自动测试覆盖错误分类，严格验收使用双接口 `tcpdump` 证明真实路径。

### R2：DNS 自递归或跨出口污染

控制：真实解析只使用显式/DHCP 发现的接口绑定 Resolver，禁止调用系统默认 Resolver；每出口
拥有独立 A/AAAA TTL 缓存，答案不跨出口共享。

### R3：系统 DNS、路由或 utun 残留

控制：原子恢复状态、实例锁、启动前 stale recovery、正常/错误反向回滚、LaunchDaemon crash
恢复和幂等 cleanup。无状态文件时仅提供保守的显式 clear 操作。

### R4：Fake IP 错误复用

控制：Mapping TTL 长于 DNS TTL、活跃会话引用、重启保护窗口、快照 + WAL、损坏文件隔离和
地址池容量限制。

### R5：gVisor API 或行为变化

控制：固定依赖版本、仅在 `internal/netstack` 适配、内存 TCP/UDP 集成测试、race test 和升级
验收清单。

### R6：UDP 会话或 goroutine 泄漏

控制：空闲超时、总量/单源限制、双向 relay 取消、关闭等待和状态指标；24 小时 soak 仍是发布门禁。

### R7：Fake 前缀或 split-default 路由冲突

控制：启动前检查现有路由和前缀冲突，只操作本实例拥有的路由；default-route 模式额外规划并
验证接口作用域旁路，拓扑变化时停止而非猜测。

### R8：IPv6 旁路或半实现数据面

控制：只有配置完整 Fake IPv6/TUN IPv6 且运行时能力门控通过时才返回 Fake AAAA、安装 IPv6
路由和接受 IPv6 捕获；否则返回 NODATA。原生公网 IPv6 仍需在具备 IPv6 的物理环境继续验收。

### R9：DoH/DoT 绕过域名策略

控制：文档明确该限制。split-default 可以捕获其 IP 流量并应用纯 CIDR 规则，但当前不识别 DoH/DoT，
也无法从加密 DNS 恢复原始域名；没有显式端点 CIDR 策略时，域名级规则可能被绕过。

### R10：权限分离边界退化

控制：托管预检验证 worker 账户、目录 owner/mode、固定配置路径和 status socket；握手校验 PID、
UID/GID、配置摘要和协议状态。安装、升级和卸载保持事务性，不能以“root 后台进程”等价替代
least-privilege worker。

## 7. 发布前检查

- 依赖许可证和固定版本已审查。
- 配置示例不包含用户真实 DNS、网卡或内部域名。
- README 明确说明 `sudo`、托管权限边界以及会修改 DNS 和路由。
- 前台与托管 cleanup、crash recovery、stop/start 和 upgrade 已完成对应自动/实机验收。
- 日志默认不输出敏感业务正文。
- 二进制包含版本、commit 和构建时间。
- `go test ./...`、race、vet、lint 和目标架构 build 按发布流程执行。
- 在目标 macOS 与 CPU 架构完成 Fake DNS、TCP、UDP、路由、回滚和双接口抓包验证。
- 完成严格 24 小时 soak、QUIC 客户端验收及具备条件时的原生 IPv6 转发验收。
