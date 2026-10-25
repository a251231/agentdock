# Windows 升级后隧道恢复修复

## 目标与边界

本机升级从 v1.1.1-fork.2 到 fork.3 后，Core 健康但 Tunnel 监督进程消失，公网返回 530/1033。安装器按旧 Core 路径停止进程时也结束 Tunnel 监督进程，提交后仅处理显式 Cloudflare 参数。已有 Named 生命周期测试手动重启掩盖了缺陷。

以 README.zh-CN.md、docs/runtime-access-and-recovery.md、scripts/install/install.ps1 和原生 Tunnel CLI 为基线。Windows 安装适配器负责恢复被升级打断的运行状态；Tunnel CLI 继续独占组件、配置、凭据和监督生命周期。首次安装与原本停止的隧道不自动启用，显式 Tunnel 参数继续覆盖旧状态；不将公网健康作为 Core 提交条件。

## 实施与验证

1. 去掉 Named 生命周期测试安装后手动重启，运行隔离端到端测试确认失败；增加停止状态保持和恢复失败告警覆盖。
2. 停旧进程前通过原生状态接口捕获运行状态，成功提交后通过原生 start 恢复；Setup 使用既有干净进程上下文启动器。回滚恢复源版本后同样恢复原隧道。
3. 恢复失败写明 warning，不回滚已提交的健康 Core；不重新配置或下载组件。补 Windows PowerShell 5.1 行为回归。
4. 同步文档，运行安装脚本验证、相关 Go 测试和全量检查；验证真实本机隧道和公网健康。

## 退出与风险

删除测试中的手动重启补偿。保留显式 Cloudflare 兼容适配器，原因是历史脚本调用契约；本次不改变其参数。主要风险为 Setup 启动上下文、回滚顺序和 Quick Tunnel 地址变化，由现有原生启动机制负责。改动通过 Git diff 可回退，本机恢复仅调用既有 tunnel start。

## 验证结果

任务已完成。移除测试侧重启补偿后，原实现实际在同版本运行中修复后报 `Installer did not restore the running Named Tunnel supervisor.`，退出码 1；修复后完整 Named 生命周期两轮通过，后一轮包含提交前故障注入，退出码均为 0。

- `powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/test/test-install-windows.ps1`：退出码 0；覆盖状态真假值、Named 退避状态、非法 JSON/读取失败、原生 start 调用、Setup broker 与带空格目录、停止状态保持、显式配置优先、失败告警。
- `go test ./scripts/test ./internal/installer ./internal/desktopruntime -count=1 -timeout=3m`：退出码 0。
- `go test -p 2 ./... -count=1 -timeout=3m`、`go vet ./...`、`git diff --check`：均退出码 0。
- 端到端使用 `scripts/test/test-windows-named-tunnel-lifecycle.ps1`，参数为当前安装/卸载脚本，以及临时目录 `agentdock-upgrade-tunnel-fixtures-20261023` 中的目标 ZIP、SHA256、source.exe、trial.exe、cloudflared.exe。在 Windows PowerShell 5.1 中执行，实际覆盖首次安装不安装组件、停止状态保持、同版本修复、跨版本升级、候选激活后提交前失败回滚、无效凭据的独立失败与恢复、凭据哈希保留和卸载。完整日志 `named-rollback-green.log` 留在该临时目录。
- 本机执行原生 `tunnel start` 后，最终 `service status` 为 healthy，`tunnel status` 为 connected/ready、活动连接 2；本地及公网 `/healthz` 均 HTTP 200，版本为 1.1.1-fork.3。隔离测试进程已退出。

限制与置信度：B。完整 Inno Setup 安装包未重新构建运行；Setup 启动上下文通过真实脚本函数与 broker 合约测试验证。一次早期端到端运行出现旧可执行文件占用导致同版本替换失败，未对这一偶发问题另加修复；之后两轮完整生命周期通过。测试使用模拟 cloudflared，公网恢复另由真实本机隧道验证。源码中的安装器修复需后续打包才进入发布物。

复杂度变化：安装脚本 2035 → 2095 行，新增两个短函数和恢复分支，仍由 Windows 安装适配器负责运行状态恢复；未新增组件或配置所有者。验证脚本 763 → 853 行跨 800 行，但仅新增测试；Named 生命周期 578 → 618 行。删除安装后由测试手动重启的补偿路径。架构对齐结果为 aligned；ADR 回填跳过，因为本次修复恢复既有运行状态约定，配置、公开 CLI 和组件管理边界不变；README 与运行恢复文档已同步。
