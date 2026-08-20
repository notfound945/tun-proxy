# gVisor netstack 与会话

## 1. LinkEndpoint

```text
utun Read
→ 判断 IP 版本
→ 注入 gVisor NIC

gVisor 输出数据包
→ 写入 utun
→ macOS 交付给原应用 Socket
```

MVP 只接受 IPv4。Phase 8.3 已让 TUN 数据泵和隔离的 gVisor 适配层按实际版本接受
IPv4/IPv6，并通过内存 TCP/UDP 测试；在主机 IPv6 地址和事务路由接通前，Fake DNS
仍不会返回 AAAA，因而不会形成半实现的系统流量路径。

## 2. gVisor 初始化

- 启用 IPv4/IPv6、TCP 和 UDP；IPv6 属于 Phase 8.3。
- 为 Fake IP 流量开启 Spoofing 和 Promiscuous。
- 配置能够接受整个 Fake IP 池的路由。
- gVisor 依赖只暴露在 `internal/netstack`。
- 固定版本，并为 Forwarder、Endpoint 和 Route 封装项目接口。

## 3. TCP Forwarder

每个新 TCP 流：

1. 读取源地址、源端口、目标 Fake IP 和目标端口。
2. 通过 Fake IP 反向查询域名。
3. 创建并完成 gVisor TCP Endpoint。
4. 构建只读 Flow Metadata。
5. 调用规则引擎选择 Outbound。
6. 使用选中出口的 Resolver 获取真实地址。
7. 创建绑定对应物理接口的真实 TCP Socket。
8. 在 gVisor `net.Conn` 与真实 `net.Conn` 间双向复制。
9. 处理半关闭、超时、取消和错误传播。
10. 连接结束后释放会话和 Fake IP 引用。

## 4. TCP 关闭语义

任一方向 EOF 不能简单等同于立即关闭两端：

```text
客户端发送 EOF
→ 停止读取客户端
→ 对真实出口执行 CloseWrite/shutdown(write)
→ 继续读取服务器响应
```

要求：

- 不可恢复错误取消整个 Flow Context。
- 正常半关闭等待另一方向结束或超时。
- 最终 Close 只执行一次。
- 连接结束后不残留复制 goroutine。

## 5. UDP Forwarder

UDP 在 TCP MVP 后实现。会话键：

```text
源 IP + 源端口 + Fake IP + 目标端口 + 地址族
```

每个会话记录域名、选定出口、真实目标、出口 PacketConn、最后活动时间和 Fake IP 引用。

要求：

- 首个数据报创建会话并匹配一次规则。
- 后续数据报复用出口和解析结果。
- 设置空闲超时、最大会话数和每源限制。
- 响应通过 gVisor 重新封装并写回 utun。
- 接口失效时关闭相关 UDP 会话。

## 6. 缓冲和背压

- utun 读写使用批处理能力。
- 包缓冲区通过有上限的池复用。
- TCP 字节流复制使用固定大小缓冲区池。
- 不允许因慢出口无限缓存应用侧数据。
- 所有内部 channel 都有明确容量和满载策略。
- 达到连接或会话上限时快速拒绝并记录原因。

## 7. 超时

至少支持 TCP connect timeout、DNS query timeout、UDP idle timeout 和 graceful shutdown timeout。TCP idle timeout 在 Beta 阶段加入。所有超时从运行时配置读取。

## 8. 可观测指标

- 活跃和累计 TCP 流。
- 活跃和累计 UDP 会话。
- 每出口连接成功、失败和超时。
- utun 收发包、字节和丢弃。
- netstack 错误分类。
- goroutine 和缓冲池高水位。
