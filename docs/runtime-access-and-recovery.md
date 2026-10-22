# 运行时访问与连接恢复

## Windows 控制面板与托盘

控制面板和托盘通过 CLI 读取 Core 与 Tunnel 状态；CLI 优先查询本地控制管道，失败后由独立进程查询系统状态。

- 每次状态命令总预算为 10 秒，其中后台 IPC 查询最多等待 2 秒；调用方提供的更短超时优先。
- Windows 管道连接并行处理，单个客户端只发送部分请求或不读取响应时，不阻塞后续请求。
- 服务端每个控制请求最多处理 30 秒。客户端未指定截止时间时也采用 30 秒预算；连接、读写和响应 flush 均支持取消。
- Core 退出时取消正在进行的管道 I/O，并等待连接处理结束。
- Core 的 `running` / `healthy` 与 NexusDock 的 `nexus_connected` 表示不同状态。远程暂时离线时，本机 Core 仍可能正常提供服务。

### 旧进程退出与新进程启动

- Core 健康时，重复启动保持幂等；不健康时，先停止残留 Core，再启动当前版本并等待健康检查。
- 旧布局沿用清单指定的 Core 入口；generation 布局的状态查询、停止和重启检查同一安装根内当前及旧版 `versions/<版本>/agentdock-core.exe` 的完整路径，避免只看 active pointer 而漏掉旧版本。稳定 shim 不计为 Core，也不作为清理目标。
- 停止过程中持续重新检查这些路径；强制清理后等待路径稳定为空，再返回成功。稳定窗口以新一轮扫描开始时间判断，避免慢扫描的旧空快照漏掉刚重启的进程。不会仅凭进程名终止其他安装目录的程序。
- 保留已验证的 Tunnel supervisor 和更新调用链上的祖先进程；supervisor 可以属于同一安装内的旧 generation，PID 指向其他目录时仍拒绝。
- 终止请求遇到进程同时自行退出时，以已打开句柄确认的退出状态为准；仍存活的权限错误与退出超时继续返回失败。

## Windows 固定域名 Cloudflare Tunnel

固定域名（Named Tunnel）的连接恢复由已有 Tunnel supervisor 统一管理。控制面板、托盘和 CLI 读取同一份本机健康快照，状态查询不会启动、停止或重新配置隧道。

- 每 5 秒检查活动网卡、IP、DNS、接口度量、网关及 IPv4 默认/分流默认路由；网络变化只触发检查，健康连接继续运行。持续变化期间至少等待 3 秒稳定。
- supervisor 为 cloudflared 显式分配回环指标端口，并核对监听 PID。以 `/ready` 的活动连接作为连接证据；历史注册日志和单纯进程存在不代表当前就绪。快照超过 20 秒、所属 supervisor 已退出或子进程路径不匹配时，状态不可用，不报 ready。
- Core `/healthz` 独立判断本地服务健康；连接建立后及每 30 秒检查固定域名 `/healthz`。公网检查失败单独显示，不触发隧道进程重建。检查保持 TLS 验证、拒绝重定向，不发送认证凭据。
- 新进程有 45 秒启动宽限；连续 30 秒无可用连接且网络已稳定后，先结束并等待当前 cloudflared 退出，再启动替代进程。固定域名、Token、配对信息及 Core 均沿用现有配置。
- 重建时重新读取 component、Token 和当前 DNS。从当前活动接口中按 IPv4 接口度量选择优先接口的解析器，通过 cloudflared `--dns-resolver-addrs` 重新查询，避开上一套 VPN 遗留的 Windows 解析缓存。显式 `TUNNEL_DNS_RESOLVER_ADDRS` 配置优先；没有可用解析器时采用系统默认解析。需要企业 NRPT/加密 DNS 等特殊解析策略时，应显式配置适用解析器并验证。
- 协议默认采用 cloudflared `auto`，显式 `TUNNEL_TRANSPORT_PROTOCOL` 配置优先。恢复退避从 5 秒增长至 1 分钟并加入抖动；滚动 5 分钟内最多启动 3 个子进程，超额等待窗口释放。活动连接连续健康 2 分钟后重置预算。网络恢复时如果处于冷却窗口，仍遵守该窗口。
- 主动停止会取消健康请求、子进程和重试等待；同一安装目录继续由原有 mutex 保证单个 supervisor。退出时清除健康快照，后台不会在停止后自行恢复。

状态包括 `connecting`、`connected`、`reconnecting`、`waiting_network`、`origin_unhealthy`、`public_unreachable`、`stopped` 和 `unknown`。CLI 保留 `running`、`ready`、`public_url`，增量返回 `connection_state`、`ready_connections`、`origin_healthy`、`public_checked`、`public_reachable`、`recovery_count`、`recovery_reason`。`ready` 要求当前 cloudflared 有连接且本地 Core 健康；公网验证结果独立返回。

该恢复机制适用于 Windows 固定域名模式。Quick Tunnel 和 Unix 平台继续采用各自现有生命周期。网络中断时正在执行的请求可能失败，恢复机制不会重放有副作用的请求。

## NexusDock 连接

- WebSocket 升级成功后，等待 `node.ready` 的预算为 15 秒；超时进入现有回退与重试流程。
- 连接建立后每 30 秒发送标准 WebSocket Ping；收到 Pong 或业务消息会刷新 90 秒读取期限。对端失去响应后关闭旧连接并自动重连。
- 业务心跳继续遵循服务端提供的间隔；自托管服务需要正常处理标准 WebSocket Ping/Pong。
- 重试沿用指数退避与随机抖动；连接稳定运行 30 秒后，下次重试从 1 秒退避开始。
- 工具调用在接收循环中先注册取消句柄，再交给执行协程，确保紧随调用到达的取消消息生效。

## HTTP Runtime API

未启用认证时，所有 `/internal/runtime/` 管理接口仅允许本机直连：对端和 Host 必须是回环地址，不允许代理转发特征或跨站请求；浏览器携带 Origin 时必须与请求同源。

远程或反向代理访问需要启用并通过 Bearer/OAuth 认证。原生本机客户端可以不发送 Origin。

## 动态 MCP 连接与授权

- MCP Streamable HTTP 仅允许同一协议、主机和端口的重定向，最多 10 次；跨源地址或 HTTPS 降级返回连接错误，避免配置的静态头、环境变量头和 OAuth 凭据随重定向泄漏。需要迁移服务地址时直接更新服务器配置。
- 工具列表包含 `null` 等导致 SDK 解码异常的定义时，返回 `MCP_INVALID_RESPONSE`；授权完成后的后台发现同样受此边界保护。
- 工具调用返回 `MCP_CONNECTION_FAILED` 时清理客户端和工具缓存，并记录连接错误；下一次独立调用重新初始化。当前调用不会自动重放，调用方应先确认有副作用的操作是否已执行。
- 工具业务错误、请求超时以及保留逻辑连接的 `MCP_TRANSPORT_REJECTED` 不清理会话。
- 清除 OAuth 授权会撤销同一存储键对应的流程和 TokenSource。清除前已开始的准备、换码或刷新不能重新写入或返回旧授权；新授权提交后，旧 TokenSource 同样失效。网络请求在管理锁外执行，保存与清除在锁内串行完成。

## ACP 管道取消

- 发送报文和等待写入队列均遵循请求截止时间，单次写入预算最多 30 秒；请求已取消且尚未开始写入时保留连接。
- 请求等待响应期间取消后立即返回，后台尽力发送 `$/cancel_request`，通知预算为 1 秒。
- 写入中途失败或取消时关闭连接，避免复用只有部分 JSON-RPC 报文的流；关闭连接也会取消正在进行的写入。Windows ACP 与桌面控制管道共用同步 I/O 取消实现。

## 文件补丁回滚

恢复原文件备份时使用原子的“不覆盖已有目标”操作。恢复窗口内其他进程重建目标文件时，返回冲突并保留该文件与原始备份；错误信息包含备份路径，供核对后人工恢复。

## 回归验证

```sh
go test ./...
go test -race ./...
```

在 Windows 原生环境执行 `go test ./internal/desktopcontrol ./internal/desktopruntime ./internal/nexusbridge`，覆盖半条请求、未读取响应、等待响应期间取消、状态查询回退、握手无响应、失效连接自动重连，以及正常空闲连接保持在线。

`go test ./internal/desktopruntime -run TestCoreRecovery` 使用真实 Windows 测试进程验证：旧 generation 占用端口、清理后新 generation 接管端口、启动前清理不健康旧 Core、保留健康 Core、旧版 supervisor 识别、进程退出竞态，以及其他安装与源版本 updater 不被误杀。

`go test ./internal/desktopruntime -run 'TestNamedTunnel|TestTunnelReady|TestTunnelHealth|TestTunnelRecovery|TestTunnelNetwork|TestTunnelMetrics|TestTunnelStatus'` 覆盖固定域名隧道的持续断线恢复、替代进程启动前回收旧进程、DNS 刷新与显式覆盖、健康连接保留、退避限流、主动停止取消、指标端口归属，以及过期或不同 supervisor 的快照拒绝。

`go test ./internal/mcp/client ./internal/mcp/oauthclient ./internal/acp ./internal/tool/file` 覆盖跨源凭据保护、后台发现畸形工具、失效会话恢复、授权清除与准备/换码/刷新竞态、真实匿名管道阻塞与排队取消，以及回滚恢复窗口的并发文件冲突。

这些自动化测试使用隔离的临时目录和模拟服务，不等同于真实网络上的多日运行、睡眠唤醒或安装包升级验收。
