# 测试计划与完成标准

## 1. 单元测试

### 配置

- YAML 未知字段和错误类型。
- Duration、地址、CIDR、端口解析。
- 默认规则位置。
- 未定义 Outbound 引用。
- fallback 循环。
- 不可变字段热更新拒绝。

### Fake IP 与 DNS

- Fake IP 分配、稳定性、回收和耗尽。
- 并发查询同一域名只分配一个地址。
- 双向映射一致性。
- 活跃引用阻止回收。
- 域名标准化和排除规则。
- A、AAAA、非地址查询和 DNS TCP 回退。

### 规则与出口

- 规则顺序和组合条件。
- 默认规则。
- Outbound fallback。
- 接口不可用错误分类。
- DNS 缓存按 Outbound 隔离。

### 系统事务与会话

- 状态文件序列化。
- 启动步骤失败后的反向回滚。
- cleanup 幂等性和不匹配状态拒绝。
- TCP 关闭协调。
- UDP 会话过期。
- 容量限制。
- context 取消后 goroutine 退出。

## 2. 集成测试

- 使用内存 LinkEndpoint 测试 TCP Echo。
- HTTP 和 HTTPS。
- TCP 半关闭。
- 大文件双向传输。
- UDP Echo。
- DNS UDP/TCP。
- 上游 DNS 超时和 TCP 回退。
- 网卡断开及 fallback。
- `SIGINT`、`SIGTERM` 和启动失败回滚。
- Fake IP 映射持久化与重启恢复。

## 3. macOS 实机验证

```bash
dig example.com
route -n get 198.18.0.10
sudo tcpdump -ni utunX
sudo tcpdump -ni en0
sudo tcpdump -ni en5
scutil --dns
```

重点验证：

- Fake IP 流量只进入 utun。
- 真实出口流量只进入规则指定网卡。
- 上游 DNS 与业务连接使用同一网卡。
- HTTP、HTTPS 和大文件下载正常。
- 拔掉网卡后，新流按 fallback 或 reject。
- 正常退出后 DNS 和路由完全恢复。
- 强制终止后可通过 `cleanup` 恢复。

两块网卡若共享同一公网出口，不能只依赖公网 IP 判断，应以各接口 `tcpdump` 或只在特定网络可达的测试服务为准。

## 4. 稳定性测试

- 连续运行 24 小时。
- 周期性建立和关闭 TCP 流。
- 高频短 UDP 会话。
- DNS 并发与缓存压力。
- Fake IP 池接近高水位。
- 睡眠、唤醒、插拔有线网卡和切换 Wi-Fi。
- 多次启动、停止和 cleanup。
- 监控 goroutine、文件描述符、内存和会话数量。

## 5. TCP MVP 完成定义

1. `check` 能在不修改系统的情况下发现配置、接口和权限问题。
2. Fake DNS 能稳定为不同域名分配不同 Fake IP。
3. Fake IP 请求能进入 utun 并由 gVisor 建立 TCP 流。
4. HTTPS 能正常访问且不需要中间人证书。
5. 两条域名规则能让真实连接分别从两块指定网卡发出。
6. 真实 DNS 查询与业务连接使用同一出口。
7. 接口断开后，新连接按配置 fallback 或 reject。
8. 正常退出能完整恢复 DNS、路由和 utun。
9. 异常退出后，`cleanup` 能根据状态日志安全恢复。
10. 单元测试和核心集成测试全部通过。

## 6. Beta 完成定义

- UDP 和 QUIC 按规则选择出口。
- Fake IP 映射可以安全持久化。
- 可变配置支持无中断热更新。
- 睡眠、唤醒和网卡变化后新流能够恢复。
- 达到资源上限时行为可预测且可诊断。
- 24 小时稳定性测试无持续资源泄漏。

