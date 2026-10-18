# 架构

MirrorBird 让用户离开电脑后，通过手机浏览器继续操作 ChatGPT Desktop / Codex 的同一个会话，双端实时同步。

## 连接方式

```text
ChatGPT Desktop ↔ shim ↔ Codex app-server
                   ↕ named pipe
              MirrorBird
                   ↕ HTTP / SSE
         Microsoft 私有 Dev Tunnel
                   ↕
               手机浏览器
```

Desktop 通过 `CODEX_CLI_PATH` 启动 shim。shim 转发请求和事件，让 Desktop 与 MirrorBird 共用原来的后端、会话和任务。

## 会话与操作

网页自动连接，支持全部可用会话的搜索、分页和归档浏览。选中会话后按需加载历史，通过 SSE 更新消息和任务状态。

用户可以发送消息、补充指令、停止当前任务和处理支持的审批。会话状态由本地快照、实时事件和缓存共同维护。

## 应用运行

Go + MyGo 提供 Windows 设置窗口与托盘，TypeScript + Vite 网页内嵌在程序中。Microsoft 私有 Dev Tunnel 负责远程转发与账号验证。

MirrorBird 管理远程访问的启停，Desktop 任务独立运行。手机远程使用时，电脑保持在线。
