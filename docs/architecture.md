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

网页自动连接，支持 ChatGPT Desktop 默认目录中的会话搜索和分页，不展示归档、子 agent 或其他 provider 的会话。目录沿用 app-server 默认的交互来源与当前 provider 筛选，只读取元数据；标题、项目路径和来源搜索在分页元数据上匹配，不为搜索恢复会话。选中后仅加载最近的有界历史，通过 SSE 更新消息和任务状态；更早历史单独分页，不能作为实时操作的授权依据。

`session/companion.go` 将 Project、模型、Skills、新建、重命名、设置、分支、编辑和压缩映射到同一 app-server 的真实 RPC。Skills 只接受由服务端列出的已安装条目，不允许浏览器提供任意文件路径；新建沿用 Desktop 权限设置。编辑使用 `thread/revert` 和重新同步后 `turn/start`，不回滚文件；分支通过 `lastTurnId` 指定包含在新会话中的最后一轮。

新消息先进入服务器端 FIFO 队列（每会话最多 32 条，每条正文最多 32 KiB）。等待条目持有投影订阅，网页离线也能在当前轮结束且无待审批请求时继续执行。只有队首可以引导：先 `turn/interrupt`，观察轮次结束，再 `turn/start`。结果不确定的条目停止推进，必须人工核对后重试；停止共享、退出 MirrorBird、Desktop 连接替换时取消队列。

新增接口为 `GET /api/options`、`POST /api/threads`、`GET /api/threads/{id}/history`、`POST /api/threads/{id}/queue` 和 `POST /api/threads/{id}/actions`。原消息、停止和审批接口保持兼容。所有 POST 保留共享生命周期、CSRF、幂等请求标识和结果不确定保护；已有会话操作还绑定投影 generation。上游明确不支持的 RPC 返回 `unsupported`，不会伪造执行成功。

Web Store 分离实时投影、只读历史、每会话草稿与不确定操作保护。离线缓存不包含 CSRF 或审批请求，联网并确认新状态前不能操作。Service worker 只缓存版本化应用壳和静态资源，API、认证与 POST 始终走网络；不缓存第三方登录跳转，也不离线排队重放请求。

## 应用运行

Go + MyGo 提供 Windows 设置窗口与托盘，TypeScript + Vite 网页内嵌在程序中。Microsoft 私有 Dev Tunnel 负责远程转发与账号验证。

MirrorBird 管理远程访问的启停，Desktop 任务独立运行。手机远程使用时，电脑保持在线。
