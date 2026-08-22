# gVisor netstack 与会话

> 本文描述当前双栈 TCP/UDP 数据面。早期“仅 IPv4 TCP MVP、UDP/IPv6 后续实现”的描述已过期。

## 1. TUN 与 LinkEndpoint

```text
utun Read
→ 校验 IPv4/IPv6 头部与实际长度
→ 注入 gVisor channel LinkEndpoint

gVisor 输出 IPv4/IPv6 数据包
→ 有界缓冲池复制
→ 写入 utun
→ macOS 交付给原应用 Socket
```

TUN Pump 同步处理单个数据包，不允许 handler 在返回后持有复用缓冲区。输入、输出路径都有
明确错误传播；取消上下文会关闭 Device，以唤醒阻塞读取并结束 goroutine。

## 2. gVisor 初始化

- 同时注册 IPv4、IPv6、TCP 和 UDP 协议。
- 为截获流量启用 Spoofing 和 Promiscuous mode。
- 在隔离 NIC 上安装 IPv4/IPv6 全路由，宿主机实际捕获范围仍由 macOS 路由决定。
- TCP Forwarder 使用 `sessions.max_tcp_flows` 的有界容量。
- UDP Forwarder 把 gVisor endpoint 交给独立的会话限制器。
- gVisor 依赖只暴露在 `internal/netstack`，其他模块使用标准库地址和 `net.Conn` 抽象。

## 3. TCP Flow

每个新 TCP 流：

1. 读取源/目标地址和端口。
2. 如果目标在 Fake 前缀内，从对应 IPv4/IPv6 Pool 获取域名并持有映射引用；
   default-route 捕获的 literal IP 不需要映射。
3. 由 gVisor TCP Forwarder 建立应用侧 `net.Conn`。
4. 构造只读 Flow Metadata。
5. 执行域名预匹配、出口独立 DNS、CIDR 后匹配并冻结最终决定。
6. 如果最终出口为 reject，立即拒绝并记录指标。
7. 对 direct 出口，依次尝试解析地址并创建绑定物理接口的真实 TCP Socket。
8. 仅在可恢复的接口/路由/超时错误上沿 fallback 链尝试下一出口。
9. 在 gVisor Conn 与真实 Conn 之间双向 relay。
10. 连接结束后释放 generation 和 Fake IP 引用。

## 4. TCP 关闭语义

任一方向 EOF 不等同于立即关闭两端：

```text
客户端发送 EOF
→ 停止读取客户端
→ 对真实出口执行 CloseWrite/shutdown(write)
→ 继续读取服务器响应
```

- 不可恢复错误取消整个 Flow Context。
- 正常半关闭等待另一方向结束或 grace timeout。
- 最终 Close 只执行一次。
- 连接结束后不残留复制 goroutine。
- 已建立 TCP 流固定使用创建时的 generation、规则和出口，不在 reload 或网卡变化时迁移。

## 5. UDP Session

会话键包含：

```text
源 IP + 源端口 + 目标 IP + 目标端口 + 地址族
```

每个会话记录冻结的规则决定、实际出口、解析地址、connected UDP Socket、最后活动时间，
Fake-IP 流量还持有对应映射引用。

- 首个数据报注册会话、执行规则与解析、创建出口 Socket，并在成功后发送首包。
- 后续数据报复用同一目标和出口；一个会话不会因 reload 改写策略。
- 配置总会话上限和单源地址上限，达到容量时快速拒绝。
- 双向 datagram relay 使用固定大小缓冲池。
- 空闲计时器在任一方向活动后重置，超时后关闭两端并释放容量与映射引用。
- 网卡变化后的新会话使用新 generation；既有会话继续使用原 Socket，直至 I/O 错误、取消或空闲超时。

## 6. Generation 与热更新

数据面由不可变 generation 组成：

```text
rules.Engine + per-outbound routes/resolvers + TCP handler + UDP handler
```

reload 先完整构造 next generation 和 Fake DNS 上游，校验成功后原子切换。新流只获取当前
generation；旧 generation 在活跃引用归零前保留，随后把累计指标合并到 base 并回收。
共享的 Fake IP Pools 和 UDP Limiter 不因可变配置 reload 被替换。

## 7. 缓冲、容量与背压

- TUN 使用固定数量、固定最大长度的 BufferPool。
- gVisor channel endpoint 使用 `tun.packet_queue` 限制队列。
- TCP Forwarder 使用并发槽位限制活动流。
- UDP 使用全局和单源会话限制。
- DNS 使用独立并发信号量。
- TCP/UDP relay 使用复用缓冲区，不为慢出口建立无界应用层队列。
- 达到容量时快速失败并记录 rejected/capacity 指标。

## 8. 超时

当前主要超时包括：

- 每出口 TCP/UDP connect timeout；
- Fake DNS 和真实地址解析 query timeout；
- UDP idle timeout；
- TCP relay half-close grace timeout；
- 前台和托管服务 shutdown timeout；
- 服务 start/reload/stop/upgrade 的 CLI 等待超时。

尚未提供独立的通用 TCP idle timeout；长连接由端点、上下文和系统网络错误结束。

## 9. 可观测指标

status socket 当前聚合：

- TCP 总量、活跃、完成、失败、reject 和 fallback 次数；
- UDP 总量、活跃、过期、失败、reject、fallback 及双向数据报数量；
- DNS 查询、Fake IPv4/IPv6、NODATA、转发、失败和容量拒绝；
- Fake IPv4/IPv6 映射、引用、回收和持久化相关统计；
- TUN 收发包/字节、畸形丢弃和读写错误；
- netstack TCP/UDP 接收、拒绝和处理错误；
- reload、网络刷新、goroutine、文件描述符和资源限制快照。
