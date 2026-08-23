# 测试计划与当前验收状态

> 本文按当前双栈 TCP/UDP、split-default、CIDR 两阶段决策和 LaunchDaemon 权限分离实现
> 更新。早期“TCP MVP 完成后再做 UDP/IPv6/持久化/权限分离”的阶段目标已经实现；历史过程
> 和逐次实机证据保留在 `docs/phases/`，当前结论与剩余发布门禁以
> [`STATUS.md`](STATUS.md) 为准。

## 1. 自动化门禁

每次发布候选至少执行：

```bash
go test -race ./...
go vet ./...
go build ./...
```

当前自动化覆盖按模块包括：

### 配置、规则与 CLI

- strict YAML、未知字段、错误类型、版本、duration、地址、canonical CIDR 和端口解析。
- 默认规则位置、未定义 outbound、fallback 循环和 reload 不可变字段拒绝。
- `domain`、`domain_suffix`、IPv4/IPv6 `ip_cidr`、literal IP 与两阶段解析后 CIDR 决策。
- 流级 generation 固定：已有流不因 reload 或出口再次解析改变最终规则决定。
- `config validate`、`explain`、`diagnose`、`status`、`cleanup`、分层 help 和 service 子命令。
- managed reload 的成功/失败确认、配置安装回滚、日志 tail/follow/clear、大小上限和
  symlink/非普通文件拒绝。

### Fake DNS 与 Fake IP

- IPv4/IPv6 Fake IP 分配、同域稳定性、双向映射、容量、高水位回收和活跃引用保护。
- 并发查询同一域名只分配一个地址；snapshot + WAL 持久化、重启恢复和损坏文件隔离。
- 选择性 Fake DNS、域名标准化、排除规则、A/AAAA、非地址查询、UDP/TCP DNS。
- IPv6 capability gate 关闭时 Fake AAAA 返回 NODATA，避免暴露没有可用数据路径的地址。

### netstack、会话与出口

- 内存 LinkEndpoint 上的 IPv4/IPv6 TCP、256 KiB TCP Echo、半关闭和双向 relay。
- UDP 数据报边界、同五元组复用、空闲过期、总容量和单源容量限制。
- TCP/UDP literal-IP 不触发 DNS；域名流使用接口绑定 resolver 和 socket。
- direct/reject、可恢复错误 fallback、不可恢复错误不切换，以及 fallback 循环防护。
- context 取消、复制 goroutine 退出、共享 limiter 和多 generation 会话生命周期。

### macOS 系统事务与权限分离

- utun IPv4/IPv6 配置、Fake 前缀路由、状态文件和严格反向 rollback。
- opt-in IPv4/IPv6 split-default、物理接口 scoped `/1`、DNS/网关旁路、冲突拒绝和
  literal TCP/UDP 捕获。
- cleanup 幂等、当前所有权核对、接口消失处理，以及不删除外部替换的路由/DNS。
- 网络变化后的计划重验、可恢复 refresh 与不安全拓扑下的受控停止。
- root supervisor 与非 root worker 的协议、凭据、FD 交接、ready/commit/reload/shutdown、
  worker 故障后的系统恢复。
- LaunchDaemon install/start/stop/restart/upgrade/uninstall 的事务、回滚、保留与 purge 语义。

## 2. macOS 实机验收方法

常用观察命令：

```bash
dig example.com
route -n get 198.18.0.10
route -n get 203.0.113.10
sudo tcpdump -ni utunX
sudo tcpdump -ni en0
sudo tcpdump -ni en5
scutil --dns
sudo tun-proxy status -json
sudo tun-proxy service status -json
```

实机验收重点：

- 选择性模式下 Fake IP 流量进入 utun，普通非策略域名维持真实地址路径。
- `capture.default_route: true` 时，普通真实 IP 和 literal IP 由 split-default 捕获；绑定的
  direct socket 只从规则指定物理接口发出，不回流 utun。
- IPv4/IPv6、TCP/UDP、域名与 CIDR 组合规则都保持“首个最终决定绑定整个流”。
- 上游 DNS 与业务连接使用同一 outbound 接口；fallback 只在可恢复故障时发生。
- 两块网卡共享同一公网出口时，以每个接口的 `tcpdump` 或接口独占可达服务为证据，不能只
  比较公网 IP。
- 正常停止、启动失败、worker 故障和 crash cleanup 都完整恢复系统 DNS、路由、utun、
  listener、state、lock 和 status socket。
- 托管模式中 supervisor 保持 root，worker 以专用 `_tun-proxy` UID/GID 运行；Fake IP
  persistence 归 worker，系统状态和安装产物归 root。

## 3. 已完成的关键实机证据

当前源码对应的已记录结果包括：

- Fake DNS、Fake IPv4 路由、gVisor TCP、HTTPS、10 MiB 下载、超时 fallback、reject 和
  干净恢复已通过；TCP 明文 HTTP 和双接口成功抓包证据仍待补齐。
- UDP/53、40 个并发短会话、1,139 字节 DNSSEC 响应和退出恢复已通过；UDP/QUIC 共用的
  数据面已实现，但 HTTP/3 客户端级验收仍待执行。
- Fake IP 持久化、SIGHUP 原子 reload、运行时指标、容量限制、root reload/restart 和网络
  恢复 smoke 已通过；严格 24 小时 soak 仍是发布门禁。
- 双栈配置、Fake IPv6 与运行时 capability gate 已通过自动化和受限宿主验收；当前已有
  IPv6 实现，但原验收机器缺少可用物理 IPv6 网络，native IPv6 转发证据待补。
- opt-in split-default 的双栈路由规划、literal TCP/UDP、双接口抓包、冲突注入、正常退出和
  crash cleanup 已通过严格验收。
- 解析后 IPv4/IPv6 CIDR 两阶段决策、IPv4 双接口路由、literal IP、reload generation 和
  恢复已通过。
- LaunchDaemon 事务安装、启动、停止、升级、crash recovery、保留式 uninstall 和 root
  supervisor + `_tun-proxy` worker 权限分离已通过；受控 DNS replay 为 20/20 Fake IP
  响应且 failure counter 无新增。
- Phase 9 CLI 路由、help、validate、explain、diagnose、service manager、reload 确认、cleanup
  安全和日志边界已有自动化覆盖。

详细日期、命令、抓包和状态计数见 `docs/phases/phase4-tcp-mvp.md`、
`phase6-udp.md`、`phase7-stability.md` 及各 `phase8-*` / `phase9-*` 文档。

## 4. 稳定性测试

严格 soak 使用：

```bash
sudo ./scripts/phase7-soak.sh 86400 60
```

正式验收要求：

- 连续运行 24 小时并周期性执行 DNS、TCP/HTTPS 和 UDP 工作负载。
- 覆盖高频短会话、Fake IP 池压力、睡眠/唤醒、插拔有线网卡和切换 Wi-Fi。
- 观察 goroutine、文件描述符、Go heap、活跃 TCP/UDP 会话、容量拒绝、reload 和 network
  refresh 指标。
- 业务流量稳定后，不得出现 goroutine、FD、内存或活跃会话的持续单调增长。
- 最终停止后，系统 DNS、路由、utun、listener、state、lock 和 status socket 无残留。

五分钟 pre-soak 已通过；24 小时严格 soak 尚未完成，因此不能把短时 smoke 视为发布门禁
完成。

## 5. 剩余发布门禁

截至 `STATUS.md` 的当前记录，仍需完成：

1. **24 小时稳定性**：完成严格 Phase 7 soak，并确认无持续资源泄漏和恢复残留。
2. **TCP 实机证据**：补充明文 HTTP，以及两块物理接口分别成功时 DNS/TCP 同路的抓包。
3. **QUIC/HTTP/3**：使用支持 HTTP/3 的客户端，确认 UDP/443 只从规则出口发出，已有 UDP
   会话不迁移。
4. **native IPv6**：在具备非链路本地 IPv6 地址和物理 IPv6 默认路由的网络上，完成真实
   IPv6 DNS、TCP/UDP 转发、CIDR、split-default 与恢复验收。
5. **Phase 9 root 运维验收**：在维护窗口验证 managed stdout/stderr logs、root-only control
   socket 的 peer UID/摘要/Request ID 校验、断线同 ID 重试与缓存结果恢复、可变配置 reload 返回
   worker 最终结果且不替换 worker PID、不可变配置拒绝并保持旧 generation、rollback 使用新 ID 和
   `rollback_of` 并通过 control response 确认，以及 restart 后 PID
   替换、ready、Fake DNS、转发和运行时指标恢复。

这些项目是剩余验收证据，不代表 IPv6、default route、UDP、持久化或权限分离功能尚未实现。
