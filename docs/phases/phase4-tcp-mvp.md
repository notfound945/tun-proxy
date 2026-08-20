# Phase 4–5 TCP MVP 实机验收

本验收会临时创建 utun、添加 `198.18.0.0/15` 路由，并在配置允许时把活动网络服务
的 DNS 指向本机。程序正常退出会按“恢复 DNS → 停止 Fake DNS → 删除路由 → 关闭
utun”的顺序回滚；异常退出后使用状态文件执行 `cleanup`。

本阶段固定使用 `gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c`；上游许可证
为 Apache License 2.0，gVisor 类型只允许出现在 `internal/netstack` 适配层。

## 1. 准备配置和目录

复制示例配置并把 `outbounds` 中的接口改为本机实际存在、已启用且各自可访问其 DNS
上游的接口。当前实机可优先用 `en0` 和 `en7`，不要保留示例中的 `en5`。

```sh
cp configs/example.yaml ./config.yaml
sudo install -d -m 700 /var/run/tun-proxy /var/lib/tun-proxy
./bin/tun-proxy interfaces
```

至少配置两条容易验证的 TCP 规则，例如让 `example.com` 走一个接口，最终默认规则走
另一个接口。Phase 4 只验证单出口时，也可先把所有 direct outbound 指向 `en0`。

## 2. 构建与只读检查

先用普通用户构建，避免让 Go 工具链以 root 运行：

```sh
go build -o ./bin/tun-proxy ./cmd/tun-proxy
sudo ./bin/tun-proxy check -config ./config.yaml
```

`check` 必须在不创建 utun、不添加路由、不修改 DNS 的情况下输出配置摘要。

## 3. 启动与 TCP 验证

```sh
sudo ./bin/tun-proxy run -config ./config.yaml
```

出现 `tun-proxy running` 后，在其他终端执行：

```sh
dig +short example.com A
route -n get 198.18.0.10
curl -v --http1.1 https://example.com/
curl --http1.1 -o /dev/null https://speed.cloudflare.com/__down?bytes=10485760
```

验收点：

- `dig` 返回 `198.18.0.0/15` 中的 Fake IP。
- Fake IP 路由指向本次创建的 utun。
- HTTPS 不需要额外证书，客户端仍按原域名校验证书。
- 10 MiB 下载完成且长度不截断。

## 4. 双网卡、DNS 同路与 fallback

分别抓取两个物理接口；将接口名替换成配置值：

```sh
sudo tcpdump -ni en0 'port 53 or tcp port 443'
sudo tcpdump -ni en7 'port 53 or tcp port 443'
```

访问命中不同规则的域名，确认真实 DNS 与 TCP 连接出现在同一个规则出口。随后断开首选
接口并新建连接，确认只有接口消失、无路由或连接超时时才进入 fallback；既有 TCP 连接
不迁移。`reject` 规则应快速失败，NXDOMAIN 和 connection refused 不应切换出口。

## 5. 正常停止和恢复检查

在运行终端按 `Ctrl-C`，应看到 `tun-proxy stopped cleanly`。然后确认：

```sh
scutil --dns
route -n get 198.18.0.10
pgrep -lf tun-proxy || true
sudo ./bin/tun-proxy status
```

系统 DNS 应恢复到启动前值，Fake IP 专用路由和 utun 应消失，状态文件与锁不应残留。

若进程被强制终止，先确认记录的 PID 已退出，再执行：

```sh
sudo ./bin/tun-proxy cleanup \
  -state /var/run/tun-proxy/state.json \
  -lock /var/run/tun-proxy/tun-proxy.lock
```

## 6. 2026-08-18 实机结果

- Fake DNS：`example.com` 稳定映射为 `198.18.0.37`。
- 路由：Fake IP 流量进入 `utun7`，MTU 为 1400。
- 默认 `en0` 出口：多个 HTTPS 域名均返回 200，证书校验结果为 0。
- `en7` fallback：绑定 `en7` 的探针超时；规则连接等待 10 秒后切换 `en0` 并返回 200。
- 大文件：Cloudflare 10 MiB 下载得到 10,485,760 字节，耗时 1.923 秒，无截断。
- 运行统计：TCP 流 67，DNS 查询 151，TUN RX 12,311 包，TX 18,541 包。
- 退出恢复：DNS 不含本地回环上游，Fake IP 走默认路由，`utun7` 和 53 端口监听消失；
  进程打印 `stopped cleanly`，说明状态文件和锁删除也已成功。

剩余验收：补充明文 HTTP；待 `en7` 恢复公网连通后，完成两个物理出口分别成功以及每个
出口 DNS/TCP 同路的 `tcpdump` 证据。
