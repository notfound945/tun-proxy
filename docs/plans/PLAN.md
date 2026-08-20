# tun-proxy 实施计划

## 项目目标

`tun-proxy` 是一个仅面向 macOS、自行编译和使用的纯 Go TUN 代理。程序通过原生 `utun` 接管 Fake IP 网段流量，使用 Fake DNS 保留域名，再根据 YAML 规则为每个新 TCP/UDP 流选择指定的物理网卡出口。

## 文档索引

实施细节按模块拆分如下，开发前应先阅读当前任务对应的模块文档：

0. [当前进度、验收结果与剩余工作](STATUS.md)
1. [范围、架构与技术选型](01-scope-and-architecture.md)
2. [CLI 与 YAML 配置](02-cli-and-configuration.md)
3. [macOS 系统状态、utun 与路由](03-system-utun-and-routing.md)
4. [Fake DNS 与 Fake IP](04-fake-dns-and-fake-ip.md)
5. [gVisor netstack 与会话](05-netstack-and-sessions.md)
6. [规则引擎、多网卡出口与独立 DNS](06-rules-outbounds-and-resolvers.md)
7. [实施阶段、里程碑与时间估算](07-implementation-phases.md)
8. [测试计划与完成标准](08-testing-and-acceptance.md)
9. [安全、可靠性与风险控制](09-security-reliability-and-risks.md)
10. [CLI 运维与诊断](../phases/phase9-cli-operations.md)

## 总体规则

以下规则对所有模块和实施阶段生效：

1. 平台仅支持 macOS，业务代码全部使用 Go，不使用 Xcode、Swift 或 NetworkExtension。
2. 程序以 CLI 形式运行，通过 `sudo` 获取创建 `utun`、修改路由、绑定 53 端口和修改系统 DNS 所需权限。
3. 配置使用 YAML，并启用严格字段校验；配置错误时不得修改任何系统状态。
4. 使用成熟的用户态 TCP/IP 栈，不自行实现 TCP。
5. MVP 只接管 Fake IP 网段，不修改默认路由；直接 IP 流量在 MVP 中不透明接管。
6. 每个新流只匹配一次规则，出口一旦选定便固定到该流结束，既有 TCP 连接不跨网卡迁移。
7. 域名的真实 DNS 查询与其业务连接必须使用同一个出口网卡。
8. 所有系统修改都必须可追踪、可逆；必须提供事务回滚和独立的 `cleanup` 命令。
9. DNS 先成功监听再修改系统 DNS；停止时先恢复系统 DNS，再停止 Fake DNS。
10. `cleanup` 只能恢复状态文件明确记录且仍与预期一致的项目，不得进行宽泛系统清理。
11. 接口名称不得硬编码为 Wi-Fi 或有线类型，由用户通过 `interfaces` 命令确认并写入 YAML。
12. 所有缓存、会话表和地址池必须有容量、超时和可观测指标。
13. 第三方依赖必须检查许可证并固定版本；gVisor 必须通过内部适配层隔离 API 变化。
14. Phase 0 可行性验证未通过前，不进入 TUN、Fake IP 和 netstack 主体开发。

## 跨模块不变量

- 一个活跃 Fake IP 在任何时刻只能反向映射到一个域名。
- 活跃会话引用未归零时不得回收对应 Fake IP。
- 出口 DNS Socket 与业务 Socket 必须应用相同的接口绑定策略。
- 出口 Socket 不得重新进入本程序创建的 `utun`。
- 新配置只影响新流，现有流继续使用创建时的出口决策。
- 任一步启动失败都必须按已完成步骤的反向顺序回滚。
- 程序正常退出或执行 `cleanup` 后，系统 DNS、路由和接口状态必须恢复到启动前状态。

## 文档维护约定

- 主 `PLAN.md` 只维护索引、总体规则和跨模块不变量。
- 模块实现细节只写入对应的 `docs/plans/*.md`，避免在主计划中复制。
- 若设计变更影响多个模块，先更新总体规则，再更新所有受影响模块文档。
- 完成某个 Phase 后，在阶段文档记录结果和偏差；不得仅根据任务完成数量判断里程碑完成。
