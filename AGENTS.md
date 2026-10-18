# AGENTS.md

## 产品

MirrorBird 让用户通过手机浏览器继续操作电脑上 ChatGPT Desktop / Codex 的同一个会话，双端实时同步。网页自动连接，提供全部可用会话供选择。

## 技术

- Go、egoist/mygo：Windows 设置窗口与托盘。
- TypeScript、Vite：手机优先的网页，构建产物内嵌 Go 程序。
- Microsoft 私有 Dev Tunnels：远程访问与账号验证。

## 架构

Desktop 通过 `CODEX_CLI_PATH` 启动 shim，复用原 Codex app-server。MirrorBird 经 named pipe 连接 shim，通过 HTTP / SSE 向网页提供会话与实时事件。

目录说明：[内部模块](internal/AGENTS.md)、[开发脚本](tools/AGENTS.md)。

## 设计

参考 iOS、macOS 内置软件：极简、克制、精致、内容优先、原生质感。
