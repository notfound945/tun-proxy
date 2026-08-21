# Service Operation Lock、Reload Request ID 与结构化错误设计

## 1. 背景与目标

本文描述 tun-proxy 托管服务控制面的三项增强方案：

1. 跨进程 Service Operation Lock；
2. 贯穿 CLI、supervisor 和 worker 的 Reload Request ID；
3. 面向用户和自动化脚本的结构化错误协议。

这些改动不改变 tun-proxy 的 root supervisor、非 root worker、事务化配置更新和系统状态恢复架构，主要解决多个管理进程并发、reload 结果误关联以及错误只能依靠字符串判断的问题。

### 1.1 当前实现

当前 service 生命周期操作包括：

```text
install
start
stop
restart
reload
upgrade
uninstall
```

相关入口位于：

```text
cmd/tun-proxy/service.go
internal/launchservice/manager.go
```

当前存在以下限制。

#### 生命周期操作没有跨进程串行化

不同的 `sudo tun-proxy service ...` 命令运行在不同进程中，当前没有覆盖完整管理事务的 operation lock。

`internal/daemon/lock_darwin.go` 中已有的锁是代理运行实例锁。该锁在代理整个运行期间持续持有，不能直接用于 service 生命周期操作，否则运行中的服务会阻止正常的 reload、stop、upgrade 等管理操作。

#### reload 只在 privsep 内部有 Request ID

`internal/privsep/protocol.go` 的消息已经包含：

```go
type Message struct {
    Version   int             `json:"version"`
    Kind      Kind            `json:"kind"`
    RequestID uint64          `json:"request_id,omitempty"`
    Payload   json.RawMessage `json:"payload"`
}
```

这个 `RequestID` 只负责 supervisor 和 worker 之间的协议帧匹配。

CLI 目前仍通过 `launchctl kill SIGHUP` 发起 reload，并通过 status snapshot 中成功或失败计数器是否增加来判断结果：

```go
after.Reload.Failures > before.Reload.Failures
after.Reload.Successes > before.Reload.Successes
```

并发 reload、迟到的 reload 结果或其他内部 reload 都可能被错误地识别为当前 CLI 请求的结果。

#### CLI 错误没有稳定协议

CLI 顶层当前主要以以下方式处理错误：

```go
fmt.Fprintln(os.Stderr, "error:", err)
os.Exit(1)
```

所有错误基本都使用退出码 1，JSON 只覆盖部分成功输出。自动化调用者只能匹配易变化的错误字符串。

---

## 2. 跨进程 Service Operation Lock

### 2.1 目标

所有会修改托管服务、托管配置或运行状态的命令必须在不同 CLI 进程之间互斥：

```text
service install
service start
service stop
service restart
service reload
service upgrade
service uninstall
managed cleanup
```

只读命令不获取排他锁：

```text
service status
service logs
diagnose
check
```

可以借鉴 clash-verge-rev 中“所有有副作用的 service 操作通过统一 operation lock 串行执行，并对外暴露 operation in flight”的思路，但 tun-proxy 的管理命令来自多个独立进程，不能使用进程内 Mutex，必须使用内核级文件锁。

### 2.2 锁文件路径

在 `launchservice.Layout` 中增加：

```go
type Layout struct {
    // Existing fields...
    OperationLock string
}
```

推荐默认路径：

```text
/var/run/tun-proxy.service-operation.lock
```

不推荐将 operation lock 放在 `/var/run/tun-proxy/` 目录内，原因包括：

- `service install` 开始时该目录可能尚不存在；
- `service uninstall --purge` 会尝试删除该目录；
- uninstall 持锁期间，目录内的锁文件会阻止目录删除；
- 独立锁文件可以跨越 install 和 uninstall 生命周期。

锁文件可以持续保留，不需要每次释放锁时删除。进程退出或文件描述符关闭后，内核会自动释放 `flock`；`/var/run` 也会在系统重启后清理。

### 2.3 Operation Metadata

```go
type OperationKind string

const (
    OperationInstall   OperationKind = "install"
    OperationStart     OperationKind = "start"
    OperationStop      OperationKind = "stop"
    OperationRestart   OperationKind = "restart"
    OperationReload    OperationKind = "reload"
    OperationUpgrade   OperationKind = "upgrade"
    OperationUninstall OperationKind = "uninstall"
    OperationCleanup   OperationKind = "cleanup"
)

type OperationMetadata struct {
    Version              int           `json:"version"`
    ID                   string        `json:"id"`
    Kind                 OperationKind `json:"kind"`
    PID                  int           `json:"pid"`
    StartedAt            time.Time     `json:"started_at"`
    ExpectedConfigDigest string        `json:"expected_config_digest,omitempty"`
}
```

Operation ID 使用 `crypto/rand` 生成 128 bit 随机值，并编码为 32 位小写十六进制字符串。

不应仅使用以下值作为唯一 ID：

- PID；
- 时间戳；
- 配置 digest。

这些值都不能可靠地区分多次独立操作。

### 2.4 API

建议新增：

```text
internal/launchservice/operation_lock_darwin.go
internal/launchservice/operation_lock_test.go
```

API 可以设计为：

```go
type OperationSpec struct {
    Kind                 OperationKind
    ExpectedConfigDigest string
}

type OperationGuard struct {
    Metadata OperationMetadata
    // held file descriptor
}

func (manager *Manager) BeginOperation(
    ctx context.Context,
    spec OperationSpec,
) (*OperationGuard, error)

func (guard *OperationGuard) Close() error
```

推荐再提供自动释放包装：

```go
func (manager *Manager) WithOperation(
    ctx context.Context,
    spec OperationSpec,
    operation func(context.Context, OperationMetadata) error,
) error
```

### 2.5 文件安全和加锁流程

使用以下方式打开锁文件：

```go
unix.Open(
    path,
    unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
    0o600,
)
```

打开后必须校验：

- 路径对应普通文件；
- owner UID 为 0；
- mode 为 `0600`；
- 建议校验 link count 为 1；
- 父目录是预期的 root-owned 系统目录。

使用非阻塞排他锁：

```go
unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
```

成功后：

1. truncate 旧 metadata；
2. 写入本次 operation metadata；
3. `fsync` 文件；
4. 在整个事务期间保持 FD 打开。

锁已被占用时返回稳定错误码：

```text
SERVICE_OPERATION_BUSY
```

错误 details 中尽可能包含当前 holder：

```json
{
  "holder_operation": "upgrade",
  "holder_operation_id": "a7be...",
  "holder_pid": 1234,
  "started_at": "2026-08-21T10:00:00Z"
}
```

metadata 只用于诊断，是否持锁必须以实际 `flock` 结果为准。即使 metadata 缺失或损坏，也必须能够返回 operation busy。

### 2.6 锁的持有范围

Operation lock 必须覆盖完整事务，不能只覆盖单次 `launchctl` 调用。

#### Reload

```text
获取 operation lock
  → 重新检查 installed/running 状态
  → 读取当前托管配置
  → 激活新配置
  → 请求 runtime reload
  → 等待属于本请求的准确结果
  → commit 配置备份
  或 rollback 配置并恢复 runtime
释放 operation lock
```

如果只在 `manager.Reload()` 发送 SIGHUP 或 control request 时短暂加锁，等待确认和 rollback 阶段仍然可能被其他管理命令打断。

#### Restart、Upgrade 和 Uninstall

当前存在嵌套生命周期调用：

```text
Restart   → Stop → Start
Upgrade   → Stop/Start/restore
Uninstall → Stop
```

每一层都重新获取非重入锁会造成自锁。建议把公共入口和已持锁的内部方法分开：

```go
func (manager *Manager) Restart(ctx context.Context) error {
    return manager.WithOperation(ctx, OperationSpec{Kind: OperationRestart}, func(ctx context.Context, metadata OperationMetadata) error {
        if err := manager.stopLocked(ctx); err != nil {
            return err
        }
        return manager.startLocked(ctx)
    })
}
```

内部方法包括：

```text
installLocked
startLocked
stopLocked
restartLocked
upgradeLocked
uninstallLocked
```

### 2.7 Status 输出

扩展 `launchservice.Status`：

```go
type OperationState struct {
    InFlight bool               `json:"in_flight"`
    Holder   *OperationMetadata `json:"holder,omitempty"`
}

type Status struct {
    Installed bool           `json:"installed"`
    Loaded    bool           `json:"loaded"`
    Runtime   RuntimeState   `json:"runtime"`
    Operation OperationState `json:"operation"`
}
```

文本输出示例：

```text
installed=true loaded=true running=true pid=1234 phase=running
operation_in_flight=true operation=upgrade operation_id=a7be... holder_pid=4567
```

status 判断 operation 是否执行中时，应尝试获取非阻塞共享锁或排他锁进行探测，不能只判断锁文件是否存在。

---

## 3. Reload Request ID

### 3.1 ID 分层

保留现有 privsep `Message.RequestID uint64`，同时增加端到端 Reload Request ID。

| ID | 作用域 | 类型 |
|---|---|---|
| IPC Request ID | 单次 supervisor/worker session 内的消息匹配 | `uint64` |
| Reload Request ID | CLI 到最终 reload 结果 | 128-bit hex string |
| Operation ID | 一个完整 service 管理事务 | 128-bit hex string |

IPC Request ID 和 Reload Request ID 不能混为同一个字段。前者是连接内部序号，后者需要在进程重启、日志、status 和 CLI 重试中保持可识别。

### 3.2 推荐使用 Root-only Control Socket

新增：

```text
/var/run/tun-proxy/control.sock
```

扩展 Layout：

```go
type Layout struct {
    // Existing fields...
    ControlSocket string
}
```

扩展 recovery state：

```go
type State struct {
    // Existing fields...
    ControlSocket string `json:"control_socket,omitempty"`
}
```

CLI 应从 state 中读取 control socket 地址，而不是自行猜测路径。

建议新增包：

```text
internal/control/protocol.go
internal/control/server_darwin.go
internal/control/client_darwin.go
internal/control/control_test.go
```

### 3.3 Control Socket 安全要求

- socket owner 为 root；
- mode 为 `0600`；
- 使用 `Lstat` 拒绝 symlink 和非 socket 路径；
- 删除 stale socket 前校验 owner；
- 使用 macOS peer credential 校验连接方 EUID 为 0；
- 每个连接只处理一个请求；
- 严格限制请求体大小，例如 64 KiB；
- JSON decoder 使用 `DisallowUnknownFields()`；
- 设置读取、写入和整个 operation 的 deadline；
- 限制同时连接数；
- 协议必须包含 version。

### 3.4 请求和响应协议

Reload 请求：

```json
{
  "version": 1,
  "kind": "reload",
  "request_id": "c2daf5a67f2f47438637ff65f8a2cb26",
  "operation_id": "1b058f53a3eb4c6aa79c29cffd75187c",
  "expected_config_digest": "sha256:..."
}
```

成功响应：

```json
{
  "version": 1,
  "kind": "reload_result",
  "request_id": "c2daf5a67f2f47438637ff65f8a2cb26",
  "result": "succeeded",
  "config_digest": "sha256:...",
  "started_at": "2026-08-21T10:00:00Z",
  "completed_at": "2026-08-21T10:00:01Z"
}
```

失败响应：

```json
{
  "version": 1,
  "kind": "reload_result",
  "request_id": "c2daf5a67f2f47438637ff65f8a2cb26",
  "result": "failed",
  "error": {
    "code": "RELOAD_REJECTED",
    "operation": "service.reload",
    "message": "reloaded default-route topology requires restart",
    "retryable": false
  }
}
```

推荐 control socket 返回最终结果，而不是只返回 accepted。这样 supervisor 自身的配置读取、digest 比较和 `PreflightReload` 失败可以立即返回，不会因为 worker reload 计数没有变化而一直等到 CLI timeout。

### 3.5 完整调用链

```text
CLI
  1. 获取 service operation lock
  2. 生成 operation_id
  3. 生成 reload request_id
  4. 必要时原子替换托管配置
  5. 连接 control.sock
  6. 发送 request_id + expected_config_digest

Supervisor
  7. 验证协议版本、peer UID 和 request_id
  8. 读取托管配置
  9. 比较 expected_config_digest
 10. 执行 PreflightReload
 11. 把外部 request_id 放入 privsep.Reload
 12. 调用 SupervisorSession.Reload

Worker
 13. 应用配置
 14. monitor 记录 request_id、digest 和结果
 15. ReloadResult 回传同一个外部 request_id

Supervisor
 16. 校验 request_id 和 config digest
 17. 更新 state.ConfigDigest
 18. 返回最终 control response

CLI
 19. 校验 response.request_id
 20. commit 配置更新
 21. 释放 operation lock
```

### 3.6 Privsep 修改

扩展 payload，但保留现有 `Message.RequestID`：

```go
type Reload struct {
    ReloadRequestID string                      `json:"reload_request_id"`
    Config          []byte                      `json:"config"`
    ConfigDigest    string                      `json:"config_digest"`
    InterfaceDNS    map[string][]netip.AddrPort `json:"interface_dns,omitempty"`
}

type ReloadResult struct {
    ReloadRequestID string `json:"reload_request_id"`
    ConfigDigest    string `json:"config_digest,omitempty"`
    ErrorCode       string `json:"error_code,omitempty"`
    Error           string `json:"error,omitempty"`
}
```

`SupervisorSession.Reload()` 需要同时校验：

1. `Message.RequestID` 与当前内部请求一致；
2. `ReloadResult.ReloadRequestID` 与外部请求一致；
3. `ReloadResult.ConfigDigest` 与期望 digest 一致。

由于 payload schema 发生变化，建议将 `privsep.ProtocolVersion` 从 1 升级到 2。

### 3.7 Status Schema v2

当前 `ReloadStats` 只记录计数、最后时间和错误字符串。建议扩展为：

```go
type ReloadStats struct {
    Successes     uint64    `json:"successes"`
    Failures      uint64    `json:"failures"`
    LastRequestID string    `json:"last_request_id,omitempty"`
    LastResult    string    `json:"last_result,omitempty"`
    LastAttempt   time.Time `json:"last_attempt,omitempty"`
    LastCompleted time.Time `json:"last_completed,omitempty"`
    LastSuccess   time.Time `json:"last_success,omitempty"`
    LastErrorCode string    `json:"last_error_code,omitempty"`
    LastError     string    `json:"last_error,omitempty"`
}
```

同时将 `status.Version` 从 1 升级到 2。

旧确认方式：

```go
after.Reload.Successes > before.Reload.Successes
```

应改为：

```go
after.Reload.LastRequestID == requestID
```

正常路径直接使用 control response；status 中的 request ID 用于：

- control response 丢失后的恢复查询；
- `service status -json`；
- diagnose；
- 日志和故障关联。

### 3.8 幂等与断线恢复

control server 建议保留一个有界结果缓存：

```text
最多 64 个结果
或者最多保留 10 分钟
```

缓存以 `request_id` 为 key。

处理规则：

- 相同 request ID、相同 expected digest：
  - 正在执行时返回 `running`；
  - 已完成时返回缓存的最终结果；
- 相同 request ID、不同 expected digest：
  - 返回 `RELOAD_REQUEST_CONFLICT`。

这样 CLI 在连接中断后可以使用相同 request ID 安全重试，而不会重复执行一个无法关联的 reload。

### 3.9 Rollback Request ID

配置应用失败后，rollback 必须生成新的 request ID：

```json
{
  "request_id": "rollback-request-id",
  "rollback_of": "original-request-id",
  "expected_config_digest": "sha256:old"
}
```

不要复用原 request ID，否则无法区分新配置应用结果和旧配置恢复结果。

### 3.10 SIGHUP 和旧协议兼容

推荐默认采用严格策略：

- 新 CLI 发现 state 中没有 `control_socket` 时，返回 `SERVICE_PROTOCOL_TOO_OLD`；
- 提示用户执行 `sudo tun-proxy service upgrade`；
- 默认不自动回退到 SIGHUP 加计数器确认，因为这会重新引入错误关联风险。

如确实需要过渡，可提供显式参数：

```text
service reload --legacy-signal
```

SIGHUP 可以继续作为手工管理入口。supervisor 收到 SIGHUP 后，应自动生成一个 request ID，并进入与 control socket 请求相同的 reload 执行函数和日志链路。

---

## 4. 结构化错误

### 4.1 错误包

建议新增：

```text
internal/apperror/error.go
internal/apperror/codes.go
internal/apperror/envelope.go
internal/apperror/error_test.go
```

基本类型：

```go
type Code string

type Error struct {
    Code      Code
    Operation string
    Message   string
    Retryable bool
    Details   map[string]any
    Cause     error
}

func (err *Error) Error() string
func (err *Error) Unwrap() error
```

Helper：

```go
func New(code Code, operation, message string) *Error

func Wrap(
    code Code,
    operation string,
    message string,
    cause error,
) *Error

func CodeOf(err error) Code
func ExitCodeOf(err error) int
func EnvelopeOf(err error) ErrorEnvelope
```

底层现有代码可以继续使用：

```go
fmt.Errorf("...: %w", err)
errors.Is(err, target)
errors.As(err, &target)
errors.Join(errs...)
```

只在最接近业务语义的边界创建 typed error，不需要一次性改写所有底层错误。

### 4.2 首批错误码

| 错误码 | 场景 | Retryable |
|---|---|---:|
| `USAGE_ERROR` | 参数错误 | false |
| `ROOT_REQUIRED` | 未使用 sudo | false |
| `CONFIG_INVALID` | 配置解析或校验失败 | false |
| `CONFIG_RESTART_REQUIRED` | 修改项不支持热重载 | false |
| `SERVICE_NOT_INSTALLED` | 服务未安装 | false |
| `SERVICE_NOT_RUNNING` | 服务未运行 | true |
| `SERVICE_OPERATION_BUSY` | 另一个生命周期操作持锁 | true |
| `SERVICE_START_TIMEOUT` | 启动确认超时 | true |
| `SERVICE_STOP_TIMEOUT` | 停止确认超时 | true |
| `SERVICE_UNREACHABLE` | control/status socket 不可达 | true |
| `SERVICE_PROTOCOL_TOO_OLD` | CLI 和已安装服务协议不兼容 | false |
| `RELOAD_REJECTED` | runtime 拒绝配置 | false |
| `RELOAD_TIMEOUT` | reload 没有完成 | true |
| `RELOAD_REQUEST_MISMATCH` | response request ID 不匹配 | false |
| `RELOAD_DIGEST_MISMATCH` | 实际 digest 与期望不同 | false |
| `RELOAD_REQUEST_CONFLICT` | 同 ID 对应不同请求 | false |
| `ROLLBACK_INCOMPLETE` | 原操作和 rollback 均失败 | false |
| `UNSAFE_FILE` | owner、mode 或 symlink 不安全 | false |
| `INTERNAL_ERROR` | 未分类内部错误 | false |

### 4.3 JSON Error Envelope

普通错误：

```json
{
  "ok": false,
  "error": {
    "code": "SERVICE_OPERATION_BUSY",
    "operation": "service.reload",
    "message": "another service operation is in progress",
    "retryable": true,
    "details": {
      "holder_operation": "upgrade",
      "holder_operation_id": "a7be...",
      "holder_pid": 1234,
      "started_at": "2026-08-21T10:00:00Z"
    },
    "causes": []
  }
}
```

Rollback 双失败：

```json
{
  "ok": false,
  "error": {
    "code": "ROLLBACK_INCOMPLETE",
    "operation": "service.reload",
    "message": "configuration reload failed and rollback was incomplete",
    "retryable": false,
    "causes": [
      {
        "code": "RELOAD_REJECTED",
        "message": "immutable setting changed"
      },
      {
        "code": "SERVICE_UNREACHABLE",
        "message": "failed to restore rolled-back configuration in runtime"
      }
    ]
  }
}
```

`errors.Join` 应展开为多个 causes，而不是只输出拼接后的字符串。

`details` 必须使用白名单，不允许写入：

- 配置原文；
- 密钥或 token；
- DNS 凭据；
- 未筛选的环境变量；
- 可能包含敏感数据的完整底层对象。

### 4.4 输出模式

建议增加全局参数：

```text
tun-proxy --output=text ...
tun-proxy --output=json ...
```

现有命令级 `-json` 可以保留为兼容别名。

#### Text 模式

```text
error: another service operation is in progress
  operation: upgrade
  operation-id: a7be...
  holder-pid: 1234
```

#### JSON 模式

- stdout 只输出一个完整 JSON document；
- 不在 stderr 同时输出另一段文本错误；
- 失败仍然返回非零退出码；
- `_service-run` 和 `_service-worker` 继续使用现有文本日志格式，不切换为 JSON。

### 4.5 退出码

字符串错误码是精确协议；进程退出码只表示粗粒度分类。

| 退出码 | 分类 |
|---:|---|
| `1` | 未分类内部错误 |
| `2` | usage 或参数错误 |
| `3` | 配置错误 |
| `4` | 权限错误 |
| `5` | 服务状态或前置条件不满足 |
| `6` | unavailable 或 timeout |
| `7` | rollback 不完整 |
| `8` | operation busy 或 request conflict |
| `9` | 协议或版本不兼容 |

示例映射：

```text
SERVICE_OPERATION_BUSY  → 8
ROOT_REQUIRED           → 4
CONFIG_INVALID          → 3
SERVICE_NOT_RUNNING     → 5
RELOAD_TIMEOUT          → 6
ROLLBACK_INCOMPLETE     → 7
SERVICE_PROTOCOL_TOO_OLD → 9
```

### 4.6 顶层错误处理

`cmd/tun-proxy/main.go` 应从：

```go
fmt.Fprintln(os.Stderr, "error:", err)
os.Exit(1)
```

改为统一 renderer：

```go
func main() {
    result, mode, err := execute(os.Args[1:])
    if err != nil {
        renderError(mode, err)
        os.Exit(apperror.ExitCodeOf(err))
    }
    renderResult(mode, result)
}
```

第一阶段不一定需要把所有成功输出立即改为统一 envelope，但必须保证 JSON 模式下失败也是结构化 JSON，且不会出现 stdout 半段 JSON、stderr 再输出文本错误的情况。

---

## 5. 文件改动清单

### 5.1 Operation Lock

```text
internal/launchservice/layout.go
internal/launchservice/operation_lock_darwin.go
internal/launchservice/operation_lock_test.go
internal/launchservice/manager.go
cmd/tun-proxy/service.go
cmd/tun-proxy/main.go
```

### 5.2 Reload Request ID 和 Control Socket

```text
internal/control/protocol.go
internal/control/server_darwin.go
internal/control/client_darwin.go
internal/control/control_test.go

internal/system/state.go
internal/privsep/protocol.go
internal/privsep/supervisor.go
internal/privsep/session.go
internal/app/service_supervisor_darwin.go
internal/app/service_worker_darwin.go
internal/app/monitor.go
internal/status/server.go
cmd/tun-proxy/service.go
```

### 5.3 结构化错误

```text
internal/apperror/error.go
internal/apperror/codes.go
internal/apperror/envelope.go
internal/apperror/error_test.go

cmd/tun-proxy/main.go
cmd/tun-proxy/service.go
internal/launchservice/manager.go
internal/control/*
```

### 5.4 文档

```text
docs/cli-flags.md
docs/build-and-install.md
docs/phases/phase9-cli-operations.md
README.md
```

---

## 6. 推荐实施顺序

### PR 1：结构化错误基础

- 增加 `internal/apperror`；
- 首先覆盖 root required、not installed、not running 和 timeout；
- 增加 JSON error envelope 和退出码映射；
- 暂不改变 service 业务流程。

### PR 2：跨进程 Operation Lock

- 增加独立 operation lock path；
- 所有 service 写操作在命令级持锁；
- 重构 Restart、Upgrade、Uninstall 的嵌套调用；
- 在 service status 中显示 `operation_in_flight`。

### PR 3：Control Socket 和 Reload Request ID

- 新增 root-only control socket；
- 外部 reload request ID 贯穿 supervisor 和 worker；
- privsep protocol 升级到 v2；
- status schema 升级到 v2；
- CLI 改为根据 request ID 和 control response 确认结果。

### PR 4：兼容清理

- 删除默认的 SIGHUP 加计数器确认路径；
- SIGHUP 仅保留为手工兼容入口；
- 增加重复 request ID 幂等缓存；
- 补全 CLI、安装、升级和诊断文档。

---

## 7. 测试矩阵

### 7.1 Operation Lock

- 两个真实子进程竞争，第二个返回 `SERVICE_OPERATION_BUSY`；
- holder 进程退出或崩溃后锁自动释放；
- symlink、目录、非 root owner 和错误 mode 被拒绝；
- metadata 损坏时仍能准确判断锁正在被占用；
- Restart、Upgrade 和 Uninstall 不发生嵌套自锁；
- reload rollback 期间其他写操作仍被阻止；
- uninstall purge 不删除或破坏 operation lock；
- status 能正确显示有锁和无锁状态。

`flock` 测试建议使用 helper subprocess，而不只使用同一测试进程中的两个 goroutine 或文件描述符，以覆盖真实的跨进程语义。

### 7.2 Reload Request ID

- 成功响应的 request ID 和 digest 与请求完全一致；
- supervisor preflight 失败立即响应，不等待 timeout；
- worker reload 失败返回相同 request ID；
- 迟到或错误 request ID 不能确认当前请求；
- 相同 ID 和相同 digest 重试返回缓存结果；
- 相同 ID 和不同 digest 返回 `RELOAD_REQUEST_CONFLICT`；
- apply 失败、rollback 成功；
- apply 失败、rollback 失败并返回 `ROLLBACK_INCOMPLETE`；
- response 丢失后可以使用 request ID 恢复查询；
- 旧服务没有 control socket 时返回 `SERVICE_PROTOCOL_TOO_OLD`；
- 手工 SIGHUP 会生成 request ID 并进入统一 reload 流程。

### 7.3 结构化错误

- `errors.Is`、`errors.As` 和 `Unwrap` 保持有效；
- `errors.Join` 可以展开多个 cause；
- JSON 模式只产生一个合法 JSON document；
- JSON 模式失败不会混入文本错误；
- 每个错误码对应稳定退出码；
- operation ID 和 reload request ID 可以进入 details；
- error details 不包含配置原文或敏感信息；
- text 模式保留可读、可操作的提示信息。

---

## 8. 最终结论

三项改动的推荐依赖关系为：

```text
结构化错误
    ↓
跨进程 service operation lock
    ↓
control socket + reload request ID
```

实现时必须遵守两个核心原则：

1. operation lock 必须覆盖配置替换、runtime 确认、commit 和 rollback 的完整事务，不能只包住一次 SIGHUP 或 control request；
2. 现有 privsep `uint64 RequestID` 应继续负责 supervisor/worker 帧匹配，同时新增独立的外部 Reload Request ID，负责 CLI 到最终结果的端到端关联。
