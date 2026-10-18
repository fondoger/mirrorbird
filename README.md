# MirrorBird

在手机浏览器继续操作电脑上 ChatGPT Desktop / Codex 的同一个会话，双端实时同步。

Windows 设置窗口与托盘使用 [MyGo](https://github.com/egoist/mygo)，远程访问通过 Microsoft 私有 Dev Tunnels。

## 功能

- 浏览、搜索和切换全部可用会话，查看历史与归档。
- 实时查看消息、代码、工具输出和文件改动。
- 发送消息、补充指令、停止当前任务和处理支持的审批。
- 网页自动连接，新会话自动出现。
- 托盘运行、Windows 登录时启动和自动检查更新。

## 安装与使用

从 [Releases](https://github.com/fondoger/mirrorbird/releases/latest) 下载对应的 Windows ARM64 或 x64 安装器。

准备 Windows 11、ChatGPT Desktop 和 Microsoft Dev Tunnels CLI，使用时保持电脑在线。

1. 打开 MirrorBird，点击“启动 Desktop”，再“刷新连接”。首次接入或更新连接组件时，先完成当前任务并退出已有 Desktop，再从 MirrorBird 启动。
2. 登录 Microsoft Dev Tunnel，点击“开启共享”。
3. 在手机打开访问地址，完成 Microsoft 验证后选择会话。

关闭设置窗口后，MirrorBird 继续在托盘运行。“停止共享”或退出程序会结束远程访问，Desktop 任务独立继续运行。

## 更新

正式版使用 MyGo 自动检查更新，设置窗口与托盘也提供“检查更新”。安装完成后重启 MirrorBird；Desktop 下次重启时加载新版连接组件。

首次使用自动更新需安装 Release 版本。发布流程见[开发说明](docs/development.md)。

## 开发

Go、Node.js 24、TypeScript、Vite。统一构建入口为 `tools/build.mjs`，发布入口为 `tools/release.mjs`。

- [架构](docs/architecture.md)
- [开发说明](docs/development.md)
- [验证记录](docs/verification.md)
- [启动与 Defender 问题](docs/incidents/2026-10-18-launch-and-defender.md)

历史 Defender 检测结论仍待确认。当前验证进展与后续项目见上述记录。
