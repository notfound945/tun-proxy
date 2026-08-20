# macOS 系统状态、utun 与路由

## 1. 系统状态事务

任何系统修改前记录：

- 当前网络服务及其 DNS 配置。
- 计划新增的 Fake IP 路由。
- 实际创建的 `utun` 名称。
- 进程 PID、启动时间和配置摘要。

状态文件建议使用 `/var/run/tun-proxy/state.json`，采用临时文件、`fsync` 和原子替换写入。

## 2. 启停顺序

```text
检查 root 和单实例锁
→ 加载并校验 YAML
→ 保存系统状态
→ 创建并配置 utun
→ 启动 netstack
→ 启动 Fake DNS 监听器
→ 安装 Fake IP 路由
→ 最后修改系统 DNS
→ 标记运行成功
```

停止时按以下顺序：

```text
停止接收新流
→ 先恢复系统 DNS
→ 关闭活动会话
→ 删除本程序添加的路由
→ 关闭 utun
→ 删除状态文件和锁
```

启动中途失败时按已完成步骤的反向顺序回滚。

## 3. utun 配置

- 创建点对点 `utunX`。
- IPv4 地址使用 `10.255.0.2`，Peer 使用 `10.255.0.1`。
- 初始 MTU 为 1400。
- Phase 0 验证 `wireguard/tun` 的包偏移和批量 I/O 行为。
- 使用有上限的缓冲池，避免逐包持续分配。
- 关闭 Device 必须能唤醒阻塞读循环并结束数据泵 goroutine。

## 4. 路由策略

MVP 只安装：

```text
198.18.0.0/15 → utunX
```

因此 Fake IP 流量进入代理，真实出口和 IP literal 保持原路径。启动前检查该网段是否已被现有路由使用，冲突时拒绝启动或要求显式更换地址池。

## 5. 系统工具调用

MVP 可由 Go 使用 `exec.CommandContext` 调用：

- `/sbin/ifconfig`
- `/sbin/route`
- `/usr/sbin/networksetup`
- 必要时只读调用 `/usr/sbin/scutil`

必须使用绝对路径和参数数组，不通过 shell；所有修改都要有状态记录和恢复动作。

## 6. 接口枚举

`interfaces` 输出接口名称、系统索引、地址、Flags 和 Up/Running 状态。不得假设 `en0` 永远是 Wi-Fi，配置必须使用实际设备名。

## 7. cleanup

清理前验证状态文件所有者和权限、原进程状态、路由当前值及网络服务存在性。恢复失败时保留状态文件，并输出精确的人工恢复建议。

