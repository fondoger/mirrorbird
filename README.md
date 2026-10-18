# MirrorBird

在手机浏览器继续操作电脑上 ChatGPT Desktop / Codex 的同一个会话，双端实时同步。

Windows 设置窗口与托盘使用 [MyGo](https://github.com/egoist/mygo)，远程访问通过 Microsoft 私有 Dev Tunnels。

## 功能

- 浏览、搜索和切换 ChatGPT Desktop 默认目录中的会话，查看历史消息。
- 实时查看消息、代码和文件改动，连续工具操作合并展示，已完成轮次可展开查看过程。
- 按需加载更早历史，按标题、项目路径或来源搜索会话。
- 在 Desktop 已配置的 Project 中新建会话，选择模型、思考强度和已启用的 Skills。
- 服务器端消息排队：网页离线仍可继续执行，支持队首引导、撤回编辑、删除和人工核对后重试。
- 停止任务、处理支持的审批、重命名、压缩上下文、从回复分支到新聊天，以及编辑最新一轮用户消息并重新生成。
- 网页自动连接，支持移动端 PWA、少量离线历史和按会话保存草稿。
- 托盘运行、Windows 登录时启动和自动检查更新。

## 安装与使用

从 [Releases](https://github.com/fondoger/mirrorbird/releases/latest) 下载对应的 Windows ARM64 或 x64 安装器。

准备 Windows 11、ChatGPT Desktop 和 Microsoft Dev Tunnels CLI，使用时保持电脑在线。

1. 打开 MirrorBird，点击“启动 Desktop”，再“刷新连接”。首次接入或更新连接组件时，先完成当前任务并退出已有 Desktop，再从 MirrorBird 启动。
2. 登录 Microsoft Dev Tunnel，点击“开启共享”。
3. 在手机打开访问地址，完成 Microsoft 验证后选择会话。

关闭设置窗口后，MirrorBird 继续在托盘运行。“停止共享”或退出程序会结束远程访问，Desktop 任务独立继续运行。

等待消息保存在运行中的 MirrorBird 内存中，浏览器关闭或断网不会取消队列；停止共享、退出 MirrorBird 或重新连接 Desktop 会取消未执行的队列，不跨程序重启恢复。队首“引导会话”会先停止当前轮，再以该消息开始新轮，而不是向旧轮补充指令。

手机可将 HTTPS 网页添加到主屏幕。离线时只读缓存内容并保存草稿，不会自动重发操作。缓存最多包含 8 个近期会话、每会话最近 8 轮和 40 份草稿，7 天未使用后过期；可在“连接详情”清除。请仅在可信设备上使用。编辑消息只修改会话历史，不会回滚文件改动。

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
