# Desktop 启动与 Defender 问题

记录日期：2026-10-18。

## Desktop 启动

通过 MirrorBird 启动 Desktop 时，后端报 `Access is denied`，shim 随后以代码 2 退出。

原路径发现逻辑依赖正在运行的 Codex 进程。Desktop 关闭时，程序选中了 WindowsApps 内的后端文件，启动失败。

现已改为根据所选 Desktop 版本，匹配 `%LOCALAPPDATA%\OpenAI\Codex\bin` 中的运行时，并校验配套的四个文件。验证使用的 Desktop 版本为 `26.917.8451.0`。

修复后，真实 Desktop 启动、模型回复和网页双端操作均已验证通过。Windows 拒绝原路径启动的具体原因仍待确认。

## Defender 检测

2026-10-18 09:05 左右，Defender 将 `build/mirrorbird.exe` 检测为 `Trojan:Win32/Bearfoos.B!ml` 并隔离。

原文件的检测结论待确认，隔离前的哈希缺失。用户随后将工作目录加入 Defender 排除项，后续功能验证在这一环境下完成。

当前构建通过 `build/build-manifest.json` 保存来源与产物哈希。需要进一步核实时，可使用 [Microsoft 文件分析入口](https://www.microsoft.com/en-us/wdsi/filesubmission)。
