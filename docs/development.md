# 开发

## 技术栈

Windows 11、Go、egoist/mygo、TypeScript、Vite、Node.js 24。

运行时依赖 ChatGPT Desktop / Codex 与 Microsoft Dev Tunnels CLI。

## 构建与预览

本地构建入口：`tools/build.mjs`。网页产物写入 `web/static/`，随后内嵌到 Go 程序。

日常本地调试可在仓库根目录运行 `make run`，自动构建网页、主程序、shim 和诊断工具，全部成功后启动 `build\mirrorbird.exe`。

`build/` 包含桌面程序、shim 和诊断工具。使用时将主程序与 shim 放在一起，更新 shim 后重启 Desktop 加载新版本。

本地网页预览入口为 `.claude/launch.json` 中的 `mirrorbird-local-preview`，使用模拟会话数据。

新版 Web 按 `docs/design/web/` 原型实现。Native 保留原有布局，仅补充连接诊断和确认重启入口。先构建内嵌资源，再运行隔离预览：

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

浏览器回归使用 Playwright 和本机 Edge，先检查合成预览标识，再重置**预览数据**。覆盖 320–1280px 布局、历史、队列引导/撤回、编辑、分支、新建、设置、停止及 PWA 离线刷新。`MIRRORBIRD_PREVIEW_URL` 可指定另一预览端口；`MIRRORBIRD_UI_ARTIFACTS` 可指定截图输出目录。预览程序内嵌静态资源，Web 重建后需要重启预览。

新增 RPC 按 `0.155.0-alpha.16.3` 协议核对；不同 Desktop 版本可能不支持 Project、设置、编辑或分支接口，界面会明确提示更新。合成预览和协议测试不能替代真实 Desktop 双端联调，尤其需要确认手机 PWA、Microsoft 登录以及正在运行的 Desktop 会话行为。

Go 的 `-race` 尚不支持 Windows ARM64；并发回归可在受支持的平台另行运行 `go test -race ./internal/session ./internal/server ./tests/preview`。

手机端禁止页面缩放；点按消息打开操作菜单，长按仅用于选择单条消息中的文本，状态说明和按钮不可选中。工具摘要单行省略，展开后可查看完整命令与输出。侧栏不提供归档浏览，协议来源 `vscode` 仍显示为 Codex。切回健康网页只核对状态，保留 SSE 连接；实际断网、SSE 重置或离线恢复才重建连接。

MirrorBird 启动时通过握手检查**运行中的** shim 版本；重建磁盘上的 EXE 不会替换 Desktop 已启动的组件。旧版组件会在 Native 界面提示，并阻止开启共享。未接入时还会定期只读检查当前用户的 Desktop 进程；“重启 Desktop 并接入”必须再次确认，先停止共享，再请求正常退出并使用 shim 启动。保存提示或无法正常退出时不强制终止，改为提示手动退出。重启后会尝试连接，但不会自动恢复共享。

## 应用图标

`web/public/favicon.svg` 是网页、PWA 和 Native 图标的唯一设计源。修改后运行 `node tools/generate-icons.mjs`（使用已有的 Playwright 与本机 Edge），再运行 `npm --prefix web run build` 更新内嵌网页；生成的 PNG、ICO 和 `web/static/` 一并保留在仓库中，普通构建无需浏览器。

窗口、托盘和 MyGo 安装包使用 `internal/desktop/app-icon.png`。本地 `make run` 构建还会将多尺寸 ICO 写入主程序 EXE，在计算构建记录哈希之前完成；不写入会覆盖 MyGo 正式打包资源的 `.syso`。图标文件的内容参与网页离线缓存版本，更新后关闭旧网页再打开以启用新缓存。

## 发布

更新 `mygo.json` 中的版本与 `CHANGELOG.md`，提交后推送对应的 `v版本号` 标签。GitHub Actions 自动检查、构建并发布 Windows ARM64 / x64 安装器和更新包。

发布入口为 `tools/release.mjs`，使用 MyGo v0.5.5。仓库 Secret `MYGO_UPDATER_PRIVATE_KEY` 用于更新签名，公钥保存在 `mygo.json`；本地密钥备份存于 `.local/release-keys/`，请长期保存。

正式安装版默认每日检查更新，也可从设置或托盘手动检查。安装后重启 MirrorBird，按需重新开启共享；Desktop 下次重启时加载新版 shim。

## 排查入口

- `mirrorbird-diagnose`：安装路径、后端与连接状态。
- `build/build-manifest.json`：本地构建来源、工具版本与产物哈希。
- [验证记录](verification.md)：检查结果与后续验证项。
