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
- 停止过程中持续重新检查这些路径；强制清理后等待路径稳定为空，再返回成功。不会仅凭进程名终止其他安装目录的程序。
- 保留已验证的 Tunnel supervisor 和更新调用链上的祖先进程；supervisor 可以属于同一安装内的旧 generation，PID 指向其他目录时仍拒绝。
- 终止请求遇到进程同时自行退出时，以已打开句柄确认的退出状态为准；仍存活的权限错误与退出超时继续返回失败。

## NexusDock 连接

- WebSocket 升级成功后，等待 `node.ready` 的预算为 15 秒；超时进入现有回退与重试流程。
- 连接建立后每 30 秒发送标准 WebSocket Ping；收到 Pong 或业务消息会刷新 90 秒读取期限。对端失去响应后关闭旧连接并自动重连。
- 业务心跳继续遵循服务端提供的间隔；自托管服务需要正常处理标准 WebSocket Ping/Pong。
- 重试沿用指数退避与随机抖动；连接稳定运行 30 秒后，下次重试从 1 秒退避开始。
- 工具调用在接收循环中先注册取消句柄，再交给执行协程，确保紧随调用到达的取消消息生效。

## HTTP Runtime API

未启用认证时，所有 `/internal/runtime/` 管理接口仅允许本机直连：对端和 Host 必须是回环地址，不允许代理转发特征或跨站请求；浏览器携带 Origin 时必须与请求同源。

远程或反向代理访问需要启用并通过 Bearer/OAuth 认证。原生本机客户端可以不发送 Origin。

## 回归验证

```sh
go test ./...
go test -race ./internal/nexusbridge ./internal/desktopcontrol ./internal/desktopruntime ./internal/httpx ./internal/publicartifacts ./internal/tool/contract
```

在 Windows 原生环境执行 `go test ./internal/desktopcontrol ./internal/desktopruntime ./internal/nexusbridge`，覆盖半条请求、未读取响应、等待响应期间取消、状态查询回退、握手无响应、失效连接自动重连，以及正常空闲连接保持在线。

`go test ./internal/desktopruntime -run TestCoreRecovery` 使用真实 Windows 测试进程验证：旧 generation 占用端口、清理后新 generation 接管端口、启动前清理不健康旧 Core、保留健康 Core、旧版 supervisor 识别、进程退出竞态，以及其他安装与源版本 updater 不被误杀。

这些自动化测试使用隔离的临时目录和模拟服务，不等同于真实网络上的多日运行、睡眠唤醒或安装包升级验收。
