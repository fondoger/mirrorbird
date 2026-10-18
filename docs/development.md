# 开发

## 技术栈

Windows 11、Go、egoist/mygo、TypeScript、Vite、Node.js 24。

运行时依赖 ChatGPT Desktop / Codex 与 Microsoft Dev Tunnels CLI。

## 构建与预览

本地构建入口：`tools/build.mjs`。网页产物写入 `web/static/`，随后内嵌到 Go 程序。

`build/` 包含桌面程序、shim 和诊断工具。使用时将主程序与 shim 放在一起，更新 shim 后重启 Desktop 加载新版本。

本地网页预览入口为 `.claude/launch.json` 中的 `mirrorbird-local-preview`，使用模拟会话数据。

## 发布

更新 `mygo.json` 中的版本与 `CHANGELOG.md`，提交后推送对应的 `v版本号` 标签。GitHub Actions 自动检查、构建并发布 Windows ARM64 / x64 安装器和更新包。

发布入口为 `tools/release.mjs`，使用 MyGo v0.5.5。仓库 Secret `MYGO_UPDATER_PRIVATE_KEY` 用于更新签名，公钥保存在 `mygo.json`；本地密钥备份存于 `.local/release-keys/`，请长期保存。

正式安装版默认每日检查更新，也可从设置或托盘手动检查。安装后重启 MirrorBird，按需重新开启共享；Desktop 下次重启时加载新版 shim。

## 排查入口

- `mirrorbird-diagnose`：安装路径、后端与连接状态。
- `build/build-manifest.json`：本地构建来源、工具版本与产物哈希。
- [验证记录](verification.md)：检查结果与后续验证项。
