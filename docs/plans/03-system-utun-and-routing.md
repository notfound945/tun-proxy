# macOS 系统状态、utun 与路由

> 本文描述当前实现。最初仅捕获 Fake IPv4 前缀的 MVP 已扩展为可选 split-default、
> 运行时门控的 IPv6 数据面和 LaunchDaemon 权限分离。

## 1. 系统状态事务

任何宿主网络修改都必须有可恢复记录。状态文件包含或关联：

- 配置摘要、启动时间、运行阶段和实际 `utun` 名称；
- 本次修改前的网络服务 DNS 快照；
- 本次新增且由实例拥有的 Fake IP、旁路和 split-default 路由；
- 实例锁和 worker status socket；
- 仅用于精确回滚的原值和所有权信息。

前台模式默认使用 `/var/run/tun-proxy/state.json`。托管模式固定使用
`/var/run/tun-proxy/state.json` 和 `/var/run/tun-proxy/tun-proxy.lock`。状态采用受权限保护的
普通文件、临时文件、同步和原子替换写入；不安全的符号链接、所有者或文件类型必须拒绝。

## 2. 前台启停顺序

前台 `sudo tun-proxy run` 的关键顺序是：

```text
严格加载配置并完成预检
→ 预构建 Fake IP 池、规则、resolver、session 和 gVisor 图
→ 获取单实例锁并创建恢复状态
→ 创建并配置 utun IPv4/可用时的 IPv6
→ 安装 Fake IP 路由、可选旁路和 split-default 路由
→ 启动 TUN/netstack 数据泵
→ 启动 Fake DNS UDP/TCP 监听
→ 最后修改系统 DNS
→ 启动 status socket 并标记 running
```

停止或任一步失败时，按可用状态反向回滚。系统 DNS 必须在 Fake DNS 仍可响应时优先恢复，
随后停止监听器和数据面、删除本实例拥有的路由、关闭 utun、刷新持久化并清理状态和锁。
回滚未完整成功时保留恢复状态，供 `cleanup` 重试。

## 3. 托管服务权限边界

LaunchDaemon 以 root supervisor 启动，但生产数据面不长期以 root 运行：

```text
root supervisor
├── 恢复陈旧状态、获取锁
├── 创建和配置 utun
├── 预绑定 UDP/TCP 53 端口
├── 安装/恢复系统 DNS 与路由
├── 保存 root 拥有的恢复状态
└── 把 utun、DNS listener 和控制通道交给 worker

_tun-proxy worker
├── Fake DNS 与 Fake IP 持久化
├── gVisor netstack
├── 规则、resolver、TCP/UDP relay
└── worker 拥有的 status socket 和运行指标
```

supervisor 只有在 worker 完成 prepare/commit 并确认 running 后才把服务标记为就绪。reload 和
shutdown 通过有版本、长度限制、请求 ID 和摘要校验的内部协议执行。worker 退出或握手失败时，
supervisor 负责恢复全部宿主状态。

## 4. utun 配置

- 创建点对点 `utunX`。
- 默认 IPv4 地址为 `10.255.0.2`，Peer 为 `10.255.0.1`。
- 配置 `fake_ipv6` 时必须同时配置 TUN IPv6 地址和 Peer，并且点对点地址不能落入 Fake IPv6 前缀。
- IPv6 只有在宿主机存在可用物理 IPv6 默认路由等安全条件时才启用；否则 Fake AAAA 返回 NODATA。
- 默认 MTU 为 1400，包队列和缓冲池都有显式上限。
- TUN 数据泵校验 IPv4/IPv6 头部和长度，丢弃畸形包并记录指标。
- 关闭 Device 必须唤醒阻塞读循环并终止输入、输出 goroutine。

## 5. 路由策略

### 5.1 选择性 Fake IP 模式

`capture.default_route: false` 时安装：

```text
fake_ip.cidr   → utunX
fake_ipv6.cidr → utunX（仅 IPv6 能力门控通过时）
```

只有获得 Fake IP 的显式域名规则流量进入 TUN；普通真实 IP 和 literal-IP 保持原系统路径。
启动前检查前缀冲突，不覆盖未知或不属于本实例的路由。

### 5.2 split-default 模式

`capture.default_route: true` 时额外安装：

```text
0.0.0.0/1   → utunX
128.0.0.0/1 → utunX
::/1        → utunX（仅 IPv6 启用时）
8000::/1    → utunX（仅 IPv6 启用时）
```

为避免出口 Socket、上游 DNS 或网关重新进入 TUN，启动前按每个 direct 接口规划作用域旁路。
旁路必须能证明使用目标接口的网关/路由；存在冲突、缺失或歧义时拒绝启动。运行中网络变化会重新
发现 DHCP DNS 和接口状态；若 split-default 旁路拓扑发生需要重建的变化，进程安全停止并回滚，
要求重启后重新规划。

## 6. 系统工具调用

系统层只使用绝对路径和参数数组调用 macOS 工具，不经过 shell，主要包括：

- `/sbin/ifconfig`
- `/sbin/route`
- `/usr/sbin/networksetup`
- 必要的只读 `/usr/sbin/scutil`

接口名、地址、CIDR、网络服务名和文件路径必须先校验；命令设置上下文超时，并限制错误输出。
所有修改都必须在 `internal/system` 中有对应状态记录和恢复动作。

## 7. 接口枚举与网络刷新

`interfaces` 输出接口名称、系统索引、地址、Flags 和 Up/Running 状态。不得假设 `en0`
永远是 Wi-Fi，配置必须使用本机实际设备名。

运行时周期性重新发现接口 DHCP DNS。新 generation 使用更新后的 resolver 和 route；既有 TCP
流不迁移。选择性捕获模式可以原子刷新网络数据面，split-default 模式则额外要求旁路拓扑保持一致。

## 8. cleanup

普通 `cleanup` 根据状态文件精确恢复 DNS 和路由，并验证状态文件、锁、status socket 的
所有者、权限和原进程状态。恢复失败时保留状态文件，并输出精确错误。

状态文件缺失时还提供两种显式兜底：

- `-clear-dns`：仅当某个已启用网络服务的完整 DNS 列表仍恰好等于配置中的单个 Fake DNS
  地址时，才重置为自动/DHCP DNS；不覆盖手动、混合或已被外部修改的列表。
- `-clear-fake-ip`：删除配置的 IPv4/IPv6 映射快照及 WAL。

两种 clear 操作都会先尝试记录状态恢复，并共用一次实例锁；仍有实例启动或运行时必须拒绝。
