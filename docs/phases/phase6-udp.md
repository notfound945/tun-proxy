# Phase 6 UDP/QUIC 实机验收

Phase 6 在 Phase 4–5 的事务生命周期上增加 UDP 数据面。启动和退出仍会临时修改并完整
恢复 utun、Fake IP 路由及系统 DNS。

## 自动化门禁

```sh
go test -race ./...
go vet ./...
go build ./...
```

自动化测试覆盖：

- 内存 LinkEndpoint UDP Echo 和数据报边界。
- 同一五元组的连续数据报复用会话。
- 首包只匹配一次规则，并保持选定出口。
- 可恢复解析错误触发 fallback。
- 空闲过期后关闭两端并释放 Fake IP 引用。
- 总会话和单源会话容量限制。
- context 取消后两个复制 goroutine 均退出。

## 配置

```yaml
sessions:
  udp_idle_timeout: 2m
  max_udp_sessions: 4096
  max_udp_sessions_per_source: 256
```

验收空闲回收时可暂时把 `udp_idle_timeout` 改为 `10s`，完成后恢复为 `2m`。

## UDP DNS 穿透测试

先以普通用户构建，再启动正式数据面：

```sh
go build -o ./bin/tun-proxy ./cmd/tun-proxy
sudo ./bin/tun-proxy check -config ./configs/config.yaml
sudo ./bin/tun-proxy run -config ./configs/config.yaml
```

另开终端执行：

```sh
dig +short one.one.one.one A
dig @one.one.one.one example.com A +notcp +time=5 +tries=1
```

第一个命令应返回 `198.18.0.0/15` 内的 Fake IP。第二个命令把该 Fake IP 当作 DNS
服务器发送真实 UDP/53 查询：请求必须进入 utun，由规则选定出口解析
`one.one.one.one` 的真实地址，然后从相同物理接口发出并返回 DNS 答案。

可同时抓包：

```sh
sudo tcpdump -ni utun7 'udp port 53'
sudo tcpdump -ni en0 'udp port 53'
```

## QUIC/HTTP3

当前系统 `/usr/bin/curl` 不含 HTTP3 feature，不能用于本项门禁。使用支持 HTTP/3 的
Chrome、Chromium 或独立 HTTP/3 客户端访问已支持 QUIC 的站点，并在开发者工具中确认
协议为 `h3`。同时抓取规则出口：

```sh
sudo tcpdump -ni en0 'udp port 443'
sudo tcpdump -ni en7 'udp port 443'
```

验收要求：域名仍解析为 Fake IP，真实 UDP/443 只出现在规则选择的物理接口；已有 UDP
会话不跨接口迁移。

## 停止与恢复

在运行终端按 `Ctrl-C`，输出应包含 UDP 会话数：

```text
tun-proxy stopped cleanly tcp_flows=... udp_sessions=... dns_queries=...
```

随后按 Phase 4–5 的步骤确认 DNS、路由、utun、53 端口、状态文件和锁均无残留。

## 2026-08-18 实机结果

- `one.one.one.one` 由 Fake DNS 映射为 `198.18.0.10`。
- `dig @one.one.one.one example.com A +notcp` 经 Fake IP UDP 数据面在 45 ms 内返回答案。
- 40 个并发 UDP/53 短会话全部成功，无超时或空响应。
- 根区 DNSKEY 查询收到 1,139 字节 UDP 响应，未截断且用时 45 ms。
- 停止统计：TCP 流 26、UDP 会话 46、DNS 查询 120、TUN RX 6,456 包、TX 5,899 包。
- 正常停止后 Fake IP 恢复默认路由，`utun7`、53 端口监听和进程消失，系统 DNS 不含
  `127.0.0.1`；`stopped cleanly` 确认状态文件与锁也已删除。

本机 `/usr/bin/curl` 不含 HTTP3 feature，且未安装其他 QUIC 客户端，因此 UDP/443
协议级验收待工具可用后补充。UDP 转发层不解析应用协议，QUIC 与已通过的 UDP/53 共用
同一会话、规则、出口绑定和回包路径。
