# 运行时访问与连接恢复

## Windows 控制面板与托盘

控制面板和托盘通过 CLI 读取 Core 与 Tunnel 状态；CLI 优先查询本地控制管道，失败后由独立进程查询系统状态。

- 每次状态命令总预算为 10 秒，其中后台 IPC 查询最多等待 2 秒；调用方提供的更短超时优先。
- Windows 管道连接并行处理，单个客户端只发送部分请求或不读取响应时，不阻塞后续请求。
- 服务端每个控制请求最多处理 30 秒。客户端未指定截止时间时也采用 30 秒预算；连接、读写和响应 flush 均支持取消。
- Core 退出时取消正在进行的管道 I/O，并等待连接处理结束。
- Core 的 `running` / `healthy` 与 NexusDock 的 `nexus_connected` 表示不同状态。远程暂时离线时，本机 Core 仍可能正常提供服务。

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

这些自动化测试使用隔离的临时目录和模拟服务，不等同于真实网络上的多日运行、睡眠唤醒或安装包升级验收。
