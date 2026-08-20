# 实施阶段、里程碑与时间估算

## Phase 0：可行性 Spike

任务：

1. 枚举网络接口、索引、地址和状态。
2. 用 Go 创建测试 TCP/UDP Socket。
3. 分别用 `IP_BOUND_IF` 绑定两块物理网卡。
4. 验证 DNS 查询和 TCP 连接确实从指定接口发出。
5. 验证选定 utun 库能创建、读取和关闭 `utun`。
6. 记录 macOS 版本、CPU 架构和接口行为。

验收：

- `tcpdump -ni <interface>` 能明确看到流量走指定网卡。
- 两块网卡都具备有效路由；不可达时返回可诊断错误。
- utun 能在退出后正常销毁。

## Phase 1：CLI、YAML 和系统事务

任务：

- 初始化 Go 模块和目录结构。
- 实现 `interfaces`、`check`、`run`、`cleanup`、`version`。
- 完成 YAML 严格加载、默认值和完整校验。
- 实现 root 检查、单实例锁、状态日志和事务回滚。
- 实现 DNS 与路由的快照、修改和恢复。

验收：

- 错误 YAML 不修改系统。
- 任一启动步骤失败后系统状态恢复。
- `cleanup` 能恢复模拟的异常退出状态。

## Phase 2：utun 与 Fake IP 路由

任务：

- 创建和配置 utun。
- 安装 Fake IP 网段路由。
- 完成 TUN 读写数据泵。
- 增加包计数、错误日志和缓冲区复用。

验收：

- `route -n get 198.18.0.10` 指向创建的 utun。
- `tcpdump -ni utunX` 能看到发往 Fake IP 的包。
- 停止程序后路由和 utun 消失。

## Phase 3：Fake DNS 与地址池

任务：

- 实现 UDP/TCP DNS 服务。
- 完成 Fake IP 分配、反向查询、TTL 和排除规则。
- 修改并恢复活动网络服务的 DNS。
- 增加地址池并发和耗尽测试。

验收：

- 不同域名获得不同 Fake IP。
- 相同域名在有效期内获得稳定 Fake IP。
- 排除域名不获得 Fake IP。
- DNS TCP 和 UDP 均工作。
- 不发生 DNS 自递归。

## Phase 4：gVisor TCP MVP

任务：

- 将 utun 包注入 gVisor。
- 实现 TCP Forwarder。
- 通过默认出口建立真实连接。
- 实现双向复制、半关闭、超时和取消。

验收：

- HTTP 和 HTTPS 正常访问。
- TLS 证书由客户端按原域名正常校验。
- 大文件传输不截断。
- 客户端或服务器先关闭时不泄漏连接。

## Phase 5：规则和多网卡

任务：

- 编译 YAML 规则。
- 实现域名、后缀、协议、端口和默认匹配。
- 为每个出口创建接口绑定 Dialer。
- 实现 fallback 和 reject。
- 增加每出口独立 Resolver。

验收：

- 不同测试域名的连接出现在不同物理接口。
- DNS 和业务连接走同一接口。
- 拔掉有线网卡后，新连接按 fallback 执行。
- 已建立连接不改变出口。

完成 Phase 0–5 即达到 TCP MVP。

## Phase 6：UDP 与 QUIC

任务：

- 实现 UDP Forwarder 和会话表。
- 实现接口绑定 UDP Socket。
- 增加超时、容量和回收策略。
- 测试普通 UDP、DNS 和 UDP 443。

验收：

- UDP Echo 正常。
- QUIC/HTTP3 能按规则选择出口。
- 会话过期后资源被释放。

## Phase 7：稳定性和可观测性

- Fake IP 映射原子持久化。
- 支持 `SIGHUP` 热更新可变配置。
- 增加状态接口、连接统计和流量统计。
- 处理睡眠、唤醒和网卡状态变化。
- 增加连接数、内存、DNS 和 UDP 会话上限。
- 完成 24 小时稳定性测试。

## Phase 8：后续扩展

- IPv6 和 Fake IPv6 池。
- 全局默认路由及直接 IP 流量。
- 解析后 IP/CIDR 规则。
- 进程级分流。
- launchd 后台服务和最小权限 helper。

## Phase 9：CLI 运维与诊断

- 提供分层 `help`，覆盖顶层、配置和 service 子命令。
- 提供不修改主机的 `config validate` 和离线优先的 `explain`。
- 提供 `config -finder`，在 Finder 中定位当前或显式指定的配置文件。
- 提供只读 `diagnose`，汇总配置、service、runtime、网卡和 hosts 冲突。
- 提供 managed service 的 clean `restart`、确认式原子 `reload` 和安全日志 tail/follow。
- CLI 是唯一日常运维入口，必须能完成配置检查、决策解释、故障诊断和 service 运维。

详细命令契约与验收状态见
[`../phases/phase9-cli-operations.md`](../phases/phase9-cli-operations.md)。Phase 7 严格 24 小时 soak
仍是独立 release gate，不因 Phase 9 开发而视为通过。

## 里程碑与估算

| 里程碑 | 结果 | 估算 |
|---|---|---:|
| M0 | 指定网卡出口与 utun Spike 通过 | 1 天 |
| M1 | CLI、YAML、状态事务和清理 | 1–2 天 |
| M2 | utun 和 Fake IP 路由 | 1 天 |
| M3 | Fake DNS 和地址池 | 1–2 天 |
| M4 | gVisor TCP 单出口可用 | 2–3 天 |
| M5 | 规则、多网卡和每出口 DNS | 1–2 天 |
| M6 | UDP/QUIC | 2–3 天 |
| M7 | 持久化、热更新和稳定性 | 3–5 天 |

- TCP MVP：约 6–10 个工作日。
- 包含 UDP 和每出口 DNS 的自用 Beta：约 10–15 个工作日。
- 经过长期稳定性处理的自用版本：约 3–4 周。

估算假设至少有两块同时可用、各自具备有效路由的物理网卡。Phase 0 若不通过，应先修订出口设计，不得跳过风险继续开发。

## 执行状态

- 2026-08-18：Phase 0 进行中。双活动接口的 `IP_BOUND_IF` TCP 与 UDP/DNS
  探针已通过；utun 生命周期和 `tcpdump` 验收等待 root 实机验证。详细记录见
  [`../phases/phase0-spike-results.md`](../phases/phase0-spike-results.md)。
- 2026-08-18：Phase 0 可行性门禁通过。root 探针成功创建 `utun7`、设置 MTU、
  通过关闭唤醒阻塞读取并确认接口销毁。`tcpdump` 证据移入最终实机验收清单。
- 2026-08-18：Phase 1 进行中。已完成严格 YAML 到强类型配置编译、只读 preflight、
  单实例文件锁、原子恢复状态、逆序事务回滚、DNS 快照/条件恢复、路由条件删除，
  并接通 `check`、`status` 和 `cleanup`。`run` 在 Phase 2 数据面接入前只执行
  preflight，不修改系统。
- 2026-08-18：Phase 2 已完成代码与自动化测试，等待 root 实机验收。已实现 utun
  配置、Fake IP 路由安装后验证、Darwin 包偏移适配、有界缓冲池、IPv4/IPv6 分类、
  收发/丢弃计数，以及“先删路由、后关 utun”的失败回滚。历史验收记录见
  [`../phases/phase2-spike.md`](../phases/phase2-spike.md)。
- 2026-08-18：Phase 2 实机验收通过。`utun7` 路由验证成功，数据泵收到 3 个
  IPv4 UDP 包（87 字节），超时退出后路由、utun、状态文件和锁均完成清理。
- 2026-08-18：Phase 3 本地 DNS 验收通过，等待系统 DNS root 事务验收。Fake IP
  已覆盖稳定分配、双向查询、并发唯一性、TTL 回收、活跃引用保护和耗尽；Fake DNS
  已覆盖 UDP/TCP、A、AAAA NODATA、排除/非地址转发、容量限制，以及绑定同接口的
  上游 UDP 截断后 TCP 回退。历史验收记录见 [`../phases/phase3-spike.md`](../phases/phase3-spike.md)。
- 2026-08-18：Phase 3 系统 DNS root 事务验收通过。切换窗口内处理 45 次查询，
  原 scoped DNS 完整恢复，UDP/TCP 53 端口、状态文件和锁均无残留。
- 2026-08-18：Phase 4 root 实机主链路验收通过。gVisor 已固定为
  `v0.0.0-20250503011706-39ed1f5ac29c` 并隔离在 `internal/netstack`；自动化测试覆盖
  IPv4 包注入/输出、TCP Forwarder、256 KiB 内存 Echo、双向复制、半关闭、取消、
  超时和 goroutine 收敛。实机 HTTPS 证书校验正常，10 MiB 下载完整且耗时 1.923 秒；
  本次共完成 67 条 TCP 流、12,311 个 TUN 入包和 18,541 个 TUN 出包。正常退出后
  DNS 恢复为真实上游，Fake IP 路由回到默认路由，`utun7`、53 端口监听、状态文件和
  锁均无残留。计划单列的明文 HTTP 请求仍待补测。
- 2026-08-18：Phase 5 代码与 fallback 实机测试通过，等待双网卡成功出口抓包。已实现顺序规则、
  domain/domain_suffix/protocol/dst_port/default 匹配、direct/reject、受限 fallback、
  多 A 记录逐个连接，以及按出口隔离且绑定同一接口的 Resolver TTL 缓存。NXDOMAIN、
  无 A 记录和连接拒绝不会触发 fallback；接口消失、无路由和超时才允许 fallback。
  实机中 `example.com` 首选 `en7`，该接口连接超时后约 10 秒正确 fallback 至 `en0`
  并完成 TLS；由于 `en7` 当时无法直连公网，尚未完成两出口分别成功及 DNS/TCP 同接口
  的抓包证据。实机验收步骤见 [`../phases/phase4-tcp-mvp.md`](../phases/phase4-tcp-mvp.md)。
- 2026-08-18：Phase 6 UDP 实机验收通过，QUIC 因缺少 HTTP/3 客户端待补。已实现
  gVisor UDP Forwarder、五元组会话复用、绑定物理接口的 connected UDP socket、
  首包规则选择与 fallback、Fake IP 活跃引用、双向数据报转发、全局空闲回收、总会话
  和单源容量限制。内存 LinkEndpoint UDP Echo、连续数据报、空闲过期、引用释放、容量
  拒绝及 fallback 均通过 race 测试。实机通过 Fake IP 完成 UDP/53 查询、40 路并发
  短会话和 1,139 字节 DNSSEC 响应，退出时累计 46 个 UDP 会话且系统状态完整恢复。
  实机步骤与结果见 [`../phases/phase6-udp.md`](../phases/phase6-udp.md)。
- 2026-08-18：Phase 7 代码与自动化验收完成，等待 root 热更新/重启恢复和 24 小时
  稳定性实测。已实现 Fake IP 同步原子持久化、损坏快照隔离、DNS TTL 重启保护，
  SIGHUP 严格校验与新流原子换代、旧会话自然排空，root-only Unix 状态接口，TCP、
  DNS、UDP、Fake IP、packet queue 和 buffer pool 显式上限，以及睡眠间隔和网卡拓扑
  变化后的绑定 dialer/resolver 自动重建。验收步骤见
  [`../phases/phase7-stability.md`](../phases/phase7-stability.md)。
- 2026-08-18：Phase 7 root 功能门禁通过。SIGHUP 原子换代成功且既有流量未中断，
  reload 统计从 0 增至 1、无失败；Fake IP 文件权限为 `0600 root:wheel`，停止并重启
  后 `example.com` 仍保持 `198.18.0.37`。Darwin 原生 FD 统计修复后实测为 19，状态
  接口同时报告 42 个 goroutine、约 2.9 MiB Go heap 和正常 TCP/DNS/TUN 数据面。
  当前仅剩 24 小时 soak 及手动睡眠/网卡变化门禁。
- 2026-08-18：Phase 7 五分钟 soak 预检通过，15 轮 DNS/HTTPS workload 零失败，
  新增 111 条 TCP 流、38 个 UDP 会话和 201 次 DNS 查询。后台流量峰值达到 236 个
  goroutine、84 个 FD 和 34 个活跃 UDP 会话，随后分别回落到 89、35 和 4；heap
  从约 23.5 MiB 峰值经 GC 回落到 13.7 MiB，未呈单调增长。24 小时 soak 及手动
  睡眠/网卡变化仍待执行。
- 2026-08-18：Phase 7 睡眠/网卡恢复门禁通过。首次测试发现拓扑恢复为原指纹时会
  跳过重建，已由 `574a796` 增加显式 pending 状态和回归测试修复。复测中 en0、en7
  故障均从 `WARN network refresh pending` 转为 `INFO network state refreshed`，状态
  接口记录 `refreshes=2`、最新成功时间且清空 `last_error`；资源回落至 91 个
  goroutine、36 个 FD 和约 7.9 MiB heap。当前仅剩重新开始的 24 小时 soak。
- 2026-08-18：Phase 7 三小时稳定性检查点通过。166 份连续采样覆盖 3 小时 13 分，
  期间新增 3,513 条 TCP 流、1,017 个 UDP 会话、6,610 次 DNS 查询，并经历三次网卡
  恢复。goroutine、FD 和 heap 分别在 107–332、41–117 和约 9.0–39.9 MiB 间波动且
  峰值后均有回落；最终 TCP/UDP 生命周期和 Fake IP 引用账目完全闭合，无容量拒绝或
  Fake IP 耗尽。正常退出后 Fake IP 路由、utun、DNS 监听及系统 DNS 均已恢复。但代理
  运行期间有一次 HTTPS workload 达到 20 秒超时，因此本轮仅通过资源稳定性和网卡恢复
  门禁，端到端 workload 可靠性未通过，需定位后重测；严格的连续 24 小时发布门禁亦待
  执行。详见 [`../phases/phase7-stability.md`](../phases/phase7-stability.md)。
- 2026-08-18：Phase 7 workload 超时复查确认旧监控的 20 秒预算未给 `wired/en7` 的
  10 秒首选尝试及 `wifi/en0` fallback 留出足够的 DNS/TLS 余量。监控预算调整为 45 秒，
  并增加失败时间戳及手动中断汇总；五分钟复测的 15 轮 DNS/HTTPS workload 全部通过，
  DNS 失败、容量拒绝和 Fake IP 耗尽均为零。短回归已关闭，严格的连续 24 小时门禁仍待
  执行。
- 2026-08-18：按决定暂缓 Phase 7 连续 24 小时发布门禁并进入 Phase 8；该门禁保持待办，
  不视为已通过。Phase 8 已拆为双栈出口基础、Fake IPv6、IPv6 utun 数据面、默认路由与
  直接 IP、解析后 CIDR 规则、进程归属 Spike 和最小权限 launchd/helper 七个独立
  切片，先实施不改变现有 IPv4 行为的双栈出口基础。详细范围与门禁见
  [`../phases/phase8-roadmap.md`](../phases/phase8-roadmap.md)。
- 2026-08-18：Phase 8.1 双栈出口基础完成。配置和 Resolver 支持显式 IPv4/IPv6 DNS
  上游并按地址族选择 UDP/TCP socket，A/AAAA 使用隔离的有界 TTL 缓存；direct TCP/UDP
  支持 IPv6 目标并通过 `IPV6_BOUND_IF` 绑定物理接口。macOS `lo0` 实际双栈 socket、
  IPv6 DNS 截断回退、错误分类及缓存隔离测试通过，全量 race、vet、build 通过。Fake
  DNS 仍对 AAAA 返回 NODATA，现有 IPv4 TUN 行为未改变；下一切片为 Fake IPv6 身份与
  持久化。
- 2026-08-18：Phase 8.2 Fake IPv6 身份与持久化完成。通用地址池以 `netip.Addr` 安全
  支持 IPv4/IPv6，大前缀分配保持有界；可选 `fake_ipv6` 仅接受 ULA 前缀，使用与 IPv4
  分离的原子 `0600` 快照，并复用损坏隔离、重启保护、引用和容量统计。旧版 IPv4
  `version: 1` 快照格式保持兼容，跨地址族内容会被拒绝并隔离。状态接口已增加独立容量
  和使用量；全量 race、vet、build 通过。为避免半实现，AAAA 仍返回 NODATA，待 Phase
  8.3 的 IPv6 utun 路由和 netstack 数据面就绪后启用。
- 2026-08-18：Phase 8.3 数据面基础完成第一步。TUN pump 与 gVisor 适配层按包版本
  同时处理 IPv4/IPv6，TCP/UDP Flow Metadata 可保留 16 字节地址；内存链路上的 IPv6
  TCP、UDP Echo 及 TUN 双栈收发/短包拒绝测试通过。此时尚未配置主机 utun IPv6 地址、
  安装 IPv6 Fake 路由或启用 Fake AAAA，故仍不进入 root 验收。
- 2026-08-18：Phase 8.3 双栈主机路径代码完成。utun IPv6 点对点地址、显式 `-inet6`
  Fake 路由、兼容旧状态文件的双路由事务、按地址族选择的地址池/A/AAAA 解析及
  `tcp4`/`tcp6` 出口、可安全启停的 Fake AAAA 均已接通。自动化回滚与全量质量门禁通过；
  root 路由/DNS smoke 尚待验收。当前开发机物理接口只有 link-local IPv6 且无公网 IPv6
  默认路由，因此不能把公网 `curl -6` 成功作为该网络环境下的验收条件。
- 2026-08-18：Phase 8.3 root 预检发现 macOS IPv6 `route get` 必须显式使用 `-inet6`，
  且空 IPv6 路由表可能成功退出并输出 `not in table`；预检、验证和幂等清理均已覆盖。
  同时增加启动能力门：物理出口没有非 link-local IPv6 地址或 IPv6 默认路由时自动保持
  IPv4-only、AAAA NODATA，并在日志/status 暴露降级原因；切换到 IPv6 网络后重启启用。
- 2026-08-18：Phase 8.3 无原生 IPv6 降级门禁通过。双栈配置保留时，当前 en0/en7
  仅有 link-local IPv6，运行状态正确报告 configured=true、enabled=false 和降级原因；
  A/IPv4 HTTPS 正常、AAAA NODATA、Fake IPv6 零分配零引用，各项 DNS/TUN 错误计数为零。
  Ctrl-C 后 utun、53 监听、state、socket、lock 均清理，两份映射文件保持 0600
  root:wheel。原生 IPv6 转发实机门禁仍等待具备 IPv6 默认路由的网络环境。
- 2026-08-18：Phase 8.4 默认路由与直接 IP 捕获代码及自动化门禁完成，配置保持显式
  opt-in 且默认关闭。启用时先通过 scoped route lookup 证明每个出口 DNS 的物理网关，
  持久化网关/DNS 主机旁路，再安装 IPv4 及能力门允许时的 IPv6 `/1` 分裂默认路由；
  既有精确主机路由、跨接口地址歧义或拓扑变化均拒绝接管并安全回滚。直接 IP TCP/UDP
  跳过 DNS，只匹配协议、端口和默认规则。root 抓包、默认路由保持、异常退出 cleanup 与
  原生 IPv6 实机验收仍待执行，步骤见 [`../phases/phase8-default-route.md`](../phases/phase8-default-route.md)。
- 2026-08-18：Phase 8.4 root 复验确认 macOS 上全局 utun `/1` 会压过物理接口的
  scoped default，安全门按设计拒绝启动并完整回滚。现为每个 direct 出口先安装等长的
  物理 scoped `/1`，持久化 scope 与 gateway，再安装全局 utun `/1`；安装后重验及逆序
  cleanup 的自动化覆盖已补齐，等待再次 root 转发验收。
- 2026-08-18：Phase 8.4 双栈转发与正常退出 root 验收通过。首次 `kill -9` 验收发现
  utun 随进程关闭后，全局 `/1` 已由内核删除，而查询会落到等长的物理 scoped `/1`；
  cleanup 为避免误删按设计保留状态并停止。现增加记录接口不存在即视为路由已由内核
  清除的安全分支，且保留其他接口检查失败时的拒绝策略，等待恢复态续跑验收。
- 2026-08-18：使用修复后的二进制从首次 `kill -9` 保留的原状态成功续跑 cleanup；
  IPv4/IPv6 均恢复 en7 普通 default，DNS diff 为空，utun、旁路、state、socket、lock、
  53 listener 均无残留。Phase 8.4 自动化、双栈分流、literal TCP、正常与崩溃清理通过；
  严格 root 清单仅余 literal UDP、双接口抓包证据和人为冲突路由注入。
- 2026-08-18：Phase 8.4 最后三项严格 root 验收通过。未列入 bypass 的 `8.8.4.4:53`
  literal UDP 成功返回，utun4 与 en0 同时抓到请求/响应且内核零丢包，UDP 无失败、拒绝或
  fallback。临时注入的 `203.0.113.53` 精确 gateway host route 同时被 `check` 与 `run`
  在任何状态变更前拒绝；删除后恢复 en7 default，运行目录为空。Phase 8.4 完整验收结束。
- 2026-08-18：Phase 8.5 解析后 IP/CIDR 规则实现完成。配置新增 canonical IPv4/IPv6
  `ip_cidr`，规则引擎与 TCP/UDP 会话完成候选出口解析、任一地址 CIDR 后匹配、出口变化
  重解析和不可变最终决策；resolver/dial fallback 不改写策略，literal IP 跳过 DNS，reload
  generation 保持新旧 flow 隔离。自动化门禁完成后进入 root 双接口路由 smoke。
- 2026-08-18：Phase 8.5 自动化门禁已通过，root 验收尚未执行，状态保持 pending。为在
  另一台 Mac 继续，已补充跨机器交接清单：重新识别双出口接口/网关/DNS、动态获取测试
  域名的 A/AAAA（不复用旧 `/32`/`/128`）、抓取候选出口 DNS 与 CIDR 选中出口的重解析和
  TCP、验证 literal IP 不解析、SIGHUP 后旧 flow 保持旧 generation 且新 flow 使用新规则，
  最后核对路由、DNS、utun、state/socket/lock 和 53 监听完整恢复。详细命令见
  [`../phases/phase8-cidr-rules.md`](../phases/phase8-cidr-rules.md)。
- 2026-08-19：Phase 8.5 IPv4 严格 root 验收通过。双接口抓包确认 `en0` 候选 DNS 解析后，
  CIDR 命中触发 `en7` 独立重解析并从 `en7` 建立最终 TCP；literal IP 直接在 `en7` 转发且
  测试窗口内没有目标域名 DNS。A→B reload 时，旧连接 `50684` 在 `en7` 保持至正常 FIN，
  新连接 `50686` 仅在 `en0` 建立并完成，证明 generation 隔离。重复执行产生的第二次 B→B
  reload 也成功，但未作为隔离证据。Ctrl-C 后 DNS 无 diff，路由仅有邻居缓存变化，utun、
  state/socket/lock、53 监听和进程均无残留，持久化映射保持 0600 root:wheel。当前机器两条
  物理出口均无非 link-local IPv6，因此原生 IPv6 root 转发仍按环境条件跳过。
- 2026-08-19：Phase 8.6 进程归属 Spike 完成，结论为当前非 NetworkExtension 架构不支持
  确定性进程分流。Darwin `libproc` 探针可按 TUN 原始四元组唯一识别 6 条并发 TCP 的各自
  PID，但进程退出后原 tuple 已无 owner；两个不同进程通过 `SO_REUSEPORT` 共享未连接 UDP
  源端点时，同一 UDP flow 同时匹配两个合法 PID。root/helper 只能扩大扫描权限，不能补回
  历史归属或消除多 owner，因此不增加任何进程规则配置字段。探针、验收命令和重新评估条件
  见 [`../phases/phase8-process-attribution.md`](../phases/phase8-process-attribution.md)。下一切片为 Phase 8.7
  launchd 服务与最小权限 helper；它不得把同样的 socket 表轮询包装成“进程分流支持”。
- 2026-08-19：Phase 8.6 严格 root 验收通过。6 条并发 TCP 均唯一命中各自子进程 PID，完整
  进程表扫描耗时观测为 2–3 ms；首个进程退出后原 flow 返回 `none`，两个进程共享 UDP
  `SO_REUSEPORT` 源端点时返回两个不同 owner，最终稳定标记为
  `RESULT process_attribution=unsupported`。Phase 8.6 至此完整关闭。
- 2026-08-19：Phase 8.7 拆分为 8.7a LaunchDaemon 生命周期与 8.7b 最小权限进程分离，
  避免把 root 后台运行误报为 least privilege。8.7a 已完成固定系统路径、root 权限和文件
  类型校验、install/start/stop/status/upgrade/uninstall、失败回滚、崩溃后 launchd 重启、
  启动前 stale cleanup、默认保留配置/映射/日志及精确 purge。升级采用干净停止、原子替换、
  按原运行状态重启；新版本启动失败会恢复旧文件和旧服务。自动化 transaction、rollback、
  mode/owner、CLI、race、vet、build 门禁完成后进入严格 root launchd 验收；当前状态为
  **8.7a automated complete / install、forwarding、clean stop/start、upgrade acceptance
  passed / crash restart、preservation-safe uninstall accepted / 8.7a complete，8.7b
  pending**。验收步骤见
  [`../phases/phase8-launchd-service.md`](../phases/phase8-launchd-service.md)。
- 2026-08-19：Phase 8.7a 首段严格 root 验收通过。LaunchDaemon 以固定 label、binary、
  config 参数和日志路径在 system domain 运行，状态为 installed/loaded/running，实测
  PID 18820；binary/config/plist 均为 root owner，权限依次为 0755/0600/0644。Fake DNS
  返回 198.18.0.37，HTTPS 转发成功。
- 2026-08-19：Phase 8.7a clean stop/start 严格 root 验收通过。停止后服务保持
  installed/loaded，runtime 为 running=false、PID 0，state/socket/lock 全部删除；launchd
  job 保持注册并处于 not running，last exit code 为 0。重新启动后 PID 更新为 22407，Fake
  DNS 仍返回 198.18.0.37，HTTPS 转发成功。
- 2026-08-19：Phase 8.7a transactional upgrade 严格 root 验收通过。升级将已安装 binary
  替换为本地构建产物且 SHA-256 完全一致，PID 从 22407 更新为 27631；system LaunchDaemon
  的固定 binary/config/log 路径保持不变，Fake DNS 返回 198.18.0.37，HTTPS 转发成功。
- 2026-08-19：Phase 8.7a crash restart 严格 root 验收通过。对 PID 27631 发送 SIGKILL 后，
  第一次状态采样观测为未运行，第二次采样已由 launchd 拉起 PID 30144；job runs 增至 2，
  last terminating signal 明确记录 Killed: 9。恢复后 Fake DNS 返回 198.18.0.37，HTTPS
  转发成功。
- 2026-08-19：Phase 8.7a preservation-safe uninstall 严格 root 验收通过。默认卸载后状态为
  installed=false、loaded=false、running=false，plist/binary、launchd job、state/socket/lock、
  utun7 和 DNS listener 均已移除；配置、Fake IPv4/IPv6 快照及 stdout/stderr 日志保留。
  两个快照在干净停止时刷新 `saved_at`，因此 digest 变化属于预期写回而非丢失。Phase 8.7a
  至此完整关闭；Phase 8.7b 最小权限进程分离仍 pending。
- 2026-08-19：Phase 8.7b privilege-boundary audit 与 capability spike 已实现。root supervisor
  创建并配置临时 utun、绑定 Fake DNS UDP/TCP listener，通过私有 socketpair 和继承 FD 将
  资源交给从首条业务指令起即为非 root 且清空附加组的 worker；worker 复用生产 Fake DNS、
  inherited-utun 与 `IP_BOUND_IF` resolver 路径。自动化测试覆盖 listener/FD 所有权、控制通道
  deadline、Darwin `nobody` 的 `-2`/`4294967294` 等价表示和 marker packet 编解码。
- 2026-08-19：Phase 8.7b capability spike 严格 root 验收通过。非 root worker 以唯一附加组
  `4294967294` 成功打开继承的 utun7、在继承的 UDP/TCP 127.0.0.1:53 listener 上返回
  198.18.87.10、完成双向 utun marker，并通过 en7 的 `IP_BOUND_IF` 向 8.8.4.4:53 查询；同步
  抓包观测到请求与含两个 A 记录的响应。退出后 utun7、DNS listener 和 staging 目录均不存在，
  稳定结论为 `RESULT least_privilege_capability=supported`。该 spike 不等于生产 least-privilege
  拆分，专用 `_tun-proxy` 身份、数据 ownership 事务、双进程 launchd 生命周期和 crash/upgrade
  验收仍 pending。详见 [`../phases/phase8-least-privilege.md`](../phases/phase8-least-privilege.md)。
- 2026-08-19：Phase 8.7b 生产最小权限拓扑实现完成。system LaunchDaemon 保留 root
  supervisor 负责 utun、路由、系统 DNS、恢复日志与逆序清理；专用 `_tun-proxy` worker
  通过私有版本化协议接收描述符，并负责 Fake DNS/持久化、规则、gVisor、resolver、TCP/UDP
  relay、reload、status 与 metrics。账号、worker 存储 ownership、install/upgrade rollback、
  preservation-safe uninstall 和 purge 已纳入同一事务。新增 `check -service` 按 root/worker
  边界校验固定路径，并修复 launchctl 请求超时但运行态已 ready 时的误报。
- 2026-08-19：Phase 8.7b 严格生产验收通过。transactional upgrade、clean stop/start、
  managed preflight、PID replacement 和 split-privilege 父子关系全部通过；实测 supervisor
  UID 0，直属 worker UID/GID 499。新增 en7 intranet 出口和 `code.266.com`、`*.oa.com`、
  `*.mtt.xyz`、`*.ifere.com` 规则后，DNS 与 TCP/TLS 抓包均命中 en7 且无匹配 en0 泄漏。
  清理旧 `/etc/hosts` 覆盖后，20 次受控 DNS 查询全部获得 Fake IPv4，`Queries +20`、
  `FakeAnswers +20`、`Failures +0`、无容量拒绝。Phase 8.7b 至此完整关闭。
- 2026-08-19：Phase 9 CLI 运维切片实现和自动化验收完成。新增分层 `help`、离线
  `config validate`、规则/出口 `explain`、只读 `diagnose`，以及 managed service 的
  `restart`、带 runtime 成败确认的 `reload` 和安全日志 tail/follow。CLI 是项目唯一
  规划的运维入口；24 小时 soak 仍 pending。root 环境下的 restart/reload/logs 最终
  实机验收将在不干扰
  当前运行服务的维护窗口执行。
