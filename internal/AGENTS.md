# 内部模块

| 包 | 职责 |
| --- | --- |
| `app` | 应用装配，协调连接、登录、远程访问与设置。 |
| `desktop` | 原生设置窗口、托盘与更新入口，Desktop 安装定位与启动。 |
| `shim` | Desktop 调用的代理入口，启动原 Codex 后端并接入桥接。 |
| `bridge` | app-server 协议路由、事件分发、审批响应与订阅协调。 |
| `attachment` | 发现 shim，通过 named pipe 传输调用与事件。 |
| `session` | 会话目录、分页历史与实时投影，服务器消息队列、Project/模型/Skills、新建/编辑/分支，以及停止任务与审批。 |
| `server` | 网页资源、HTTP API 与 SSE。 |
| `tunnel` | Microsoft 登录、隧道创建与复用、转发启停。 |
| `settings` | 应用设置的读取与保存。 |
| `autostart` | Windows 登录时自动启动。 |
| `platform` | 命名管道、进程、用户与权限等 Windows 系统适配。 |
| `testutil` | 测试夹具辅助，统一临时目录的所有者。 |

核心链路：网页 ↔ `server` ↔ `session` ↔ `attachment` ↔ `bridge` ↔ Codex。Desktop 通过 shim 接入同一后端，`app` 协调应用运行。

## 维护

新增、调整或删除模块时，同步更新本文件。失去用途的代码与临时文件及时清理。
