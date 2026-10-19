# 开发

## 技术栈

Windows 11、Go、egoist/mygo、TypeScript、Vite、Node.js 24。

运行时依赖 ChatGPT Desktop / Codex 与 Microsoft Dev Tunnels CLI。

## 构建与预览

本地构建入口：`tools/build.mjs`。网页产物写入 `web/static/`，随后内嵌到 Go 程序。

日常本地调试可在仓库根目录运行 `make run`，自动构建网页、主程序、shim 和诊断工具，全部成功后启动 `build\mirrorbird.exe`。

`build/` 包含桌面程序、shim 和诊断工具。使用时将主程序与 shim 放在一起，更新 shim 后重启 Desktop 加载新版本。

本地网页预览入口为 `.claude/launch.json` 中的 `mirrorbird-local-preview`，使用模拟会话数据。

新版 Web 按 `docs/design/web/` 原型实现，Native 使用 MyGo 按 `docs/design/windows/` 原型实现。先构建内嵌资源，再运行隔离 Web 预览：

```powershell
npm --prefix web run build
go run ./tests/preview -port 43126
```

访问 `http://localhost:43126`。预览包含 Project、模型、Skills、36 轮旧历史、工具分组、审批、服务器队列、编辑和分支等合成场景，不发现 shim、不连接真实 Desktop、不执行模型或工具。

保持预览运行，在另一终端运行浏览器回归：

```powershell
npm --prefix web test
npm --prefix web run test:ui
go test ./internal/session ./internal/server ./internal/attachment ./tests/preview ./web
```

浏览器回归使用 Playwright 和本机 Edge，先检查合成预览标识，再重置**预览数据**。覆盖 320–1280px 布局、历史、队列引导/撤回、编辑、分支、新建、设置、停止及 PWA 离线刷新。增量回归另用超过 1 MB 的合成会话核对实际响应字节数、未变化节点身份、文本选区、展开状态和菜单可用状态。`MIRRORBIRD_PREVIEW_URL` 可指定另一预览端口；`MIRRORBIRD_UI_ARTIFACTS` 可指定截图输出目录。预览程序内嵌静态资源，Web 重建后需要重启预览。

新增 RPC 按 `0.155.0-alpha.16.3` 协议核对；不同 Desktop 版本可能不支持 Project、设置、编辑或分支接口，界面会明确提示更新。合成预览和协议测试不能替代真实 Desktop 双端联调，尤其需要确认手机 PWA、Microsoft 登录以及正在运行的 Desktop 会话行为。

Go 的 `-race` 尚不支持 Windows ARM64；并发回归可在受支持的平台另行运行 `go test -race ./internal/session ./internal/server ./tests/preview`。

手机端禁止页面缩放；点按消息打开操作菜单，长按仅用于选择单条消息中的文本，状态说明和按钮不可选中。工具摘要单行省略，展开后可查看完整命令与输出。侧栏不提供归档浏览，协议来源 `vscode` 仍显示为 Codex。切回健康网页只核对状态，保留 SSE 连接；实际断网、SSE 重置或离线恢复才重建连接。

MirrorBird 启动时通过握手检查**运行中的** shim 版本；重建磁盘上的 EXE 不会替换 Desktop 已启动的组件。旧版组件会在 Native 界面提示，并阻止开启共享。未接入时还会定期只读检查当前用户的 Desktop 进程；“重启 Desktop 并接入”必须再次确认，先停止共享，再请求正常退出并使用 shim 启动。保存提示或无法正常退出时不强制终止，改为提示手动退出。重启后会尝试连接，但不会自动恢复共享。

实时事件中的 RPC 响应只用于推进序号，不作为状态增量。attachment 在 shim 发送端和主程序接收端将其正文替换为空对象 `{}`，保留原始序号作为同步边界；真正的 RPC 返回值、通知和审批请求不变。接收端也处理旧 shim 发来的完整事件，因此此保护不要求重启已经兼容的 Desktop。事件队列仍有 512 条及累计 16 MiB 上限；字节上限与合法 IPC 单帧大小一致，避免旧的 8 MiB 阈值拒绝一条合法通知。

## Web 增量同步

`/api/connection` 通过 `syncVersion: 1` 声明增量能力。SSE 仍只广播小型变更通知，不广播正文；网页合并通知，普通在线读取最多每 250 ms 启动一次，且同一页只有一个正文读取请求在途。旧服务或旧网页继续使用完整快照接口。

`GET /api/threads/{id}/sync?cursor=...` 返回 `mode: delta`、原 `baseCursor`、新 `cursor` 及 `patch`。路径是字段名/数组下标的数组，操作为 `set`、`remove`、字符串后缀 `append` 和带 `index` / `deleteCount` / `items` 的 `splice`。有 ID 的列表按 ID 对齐，窗口头部裁剪与尾部新增不会重发中间未改的轮次。比较的是经过审批白名单过滤的类型化浏览器快照；不重复编码整段正文，不公开原始 IPC 事件或任意审批参数。

每次成功读取只推进本页基准，不保存逐 token 的快照日志。全部基准最多 32 条、累计数据计量 16 MiB，空闲 10 分钟失效；停止共享立即清除。首次打开、重连、显式 `resync=true`、projection 更换、基准过期/被淘汰或超过 2048 个补丁时返回 `mode: snapshot` 和明确的 `resetReason`。单份浏览器状态超过基准预算时仍允许完整读取，但不保留游标。这里的 16 MiB 是 HTTP 增量基准预算，与 IPC 队列上限是两个独立边界。

前端在精确匹配的原始 JSON 基准上事务性应用补丁，复用未变化的规范化轮次/消息，再局部更新 DOM。游标不进入离线缓存；切换连接世代、断线或共享凭据更换后重新建立基准。补丁不合法时仅重读完整快照；版本水位、结构状态新鲜度、人工核对未知操作结果的约束保持不变，任何 POST 都不自动重试。

## Native 设置与共享启动

设置页保留原有图标和系统窗口按钮，提供远程访问、Desktop 连接、Microsoft 登录、开机启动、版本更新与查看日志。正在建立共享时关闭开关会立即取消；已开启共享时需要确认停止。退出 Microsoft 登录和重启 Desktop 也需要确认。验证码来自真实 CLI，完成登录后自动更新，不使用原型中的模拟倒计时或“点击即成功”逻辑。

共享启动先绑定关闭访问的精确本机端口，再由 tunnel Manager 检查一次账号。host 前后的独立明细检查每轮四项并行；仍保留两级 owner-only ACL、精确单端口、原始 Host/Origin、超时配置以及 host 前后的认证读取。host 地址与当前资源的认证读取结果相符后，设置页显示“隧道已建立，正在验证连接”及可复制的待验证地址；**全部检查成功前，打开按钮禁用，HTTP API 不授权访问**。失败、取消或迟到回调不能恢复待验证地址或重新开放共享。

Native 的离线布局与交互回归不启动真实窗口、不操作 Desktop 或 Microsoft 账号：

```powershell
$env:MIRRORBIRD_NATIVE_ARTIFACTS = "$env:TEMP\mirrorbird-native-preview"
go test ./internal/desktop -run 'TestNativeDesignLayouts|TestSettings' -count=1
```

指定目录会得到 MyGo 离线渲染截图，覆盖共享、未连接、验证中和弹窗等状态；系统标题栏按钮由 Windows 绘制，不出现在离线截图中。

## 应用图标

`web/public/favicon.svg` 是网页、PWA 和 Native 图标的唯一设计源。修改后运行 `node tools/generate-icons.mjs`（使用已有的 Playwright 与本机 Edge），再运行 `npm --prefix web run build` 更新内嵌网页；生成的 PNG、ICO 和 `web/static/` 一并保留在仓库中，普通构建无需浏览器。

窗口、托盘和 MyGo 安装包使用 `internal/desktop/app-icon.png`。本地 `make run` 构建还会将多尺寸 ICO 写入主程序 EXE，在计算构建记录哈希之前完成；不写入会覆盖 MyGo 正式打包资源的 `.syso`。图标文件的内容参与网页离线缓存版本，更新后关闭旧网页再打开以启用新缓存。

## 发布

更新 `mygo.json` 中的版本与 `CHANGELOG.md`，提交后推送对应的 `v版本号` 标签。GitHub Actions 自动检查、构建并发布 Windows ARM64 / x64 安装器和更新包。

发布入口为 `tools/release.mjs`，使用 MyGo v0.5.5。仓库 Secret `MYGO_UPDATER_PRIVATE_KEY` 用于更新签名，公钥保存在 `mygo.json`；本地密钥备份存于 `.local/release-keys/`，请长期保存。

正式安装版默认每日检查更新，也可从设置或托盘手动检查。安装后重启 MirrorBird，按需重新开启共享；Desktop 下次重启时加载新版 shim。

## 排查入口

- 设置页底部“查看日志”：打开 `%APPDATA%\MirrorBird\logs` 文件夹，并显示操作结果，不依赖 `.log` 文件的默认程序关联。当前日志为 `mirrorbird.log`；设置 `MIRRORBIRD_HOME` 时改用该目录下的 `logs`。写入失败会在设置页显示提示。
- 日志为逐行 JSON，带时间、阶段、耗时和固定原因分类；单文件上限 1 MiB，保留一个 `mirrorbird.log.1` 备份，正常总量不超过 2 MiB。队列有界且不等待磁盘写入，溢出会记录丢弃数量。
- 排查突然断连时保留两个日志文件，查看故障时段的 `tunnel.host_exit`、`attachment.closed`、`session.connection_released`、`sharing.connection_lost` 与 `sharing.host_failed` 的先后顺序。`expected` 区分主动取消/关闭与意外退出；`watch_release_failed` 等固定原因可识别 IPC 主动断开的路径。日志不能追溯安装此版本前的故障。
- 事件队列溢出时记录 `event_bytes`、`queued_bytes`、`queued_events` 和 `limit_bytes`，用于区分单条事件大小与累计积压，不记录事件正文。`event_queue_byte_limit` 先出现、随后 `tunnel.host_exit` 为 `expected: true` 时，应先检查 IPC 队列，而不是把主动清理误判为 Dev Tunnel 崩溃。
- 日志仅保存在本机，不自动上传；不记录会话正文、RPC 参数、账号名、访问地址、验证码、令牌、CLI 输出或任意错误文本，只保留安全分类和数字错误码。
- `mirrorbird-diagnose`：安装路径、后端与连接状态。
- `build/build-manifest.json`：本地构建来源、工具版本与产物哈希。
- [验证记录](verification.md)：检查结果与后续验证项。
