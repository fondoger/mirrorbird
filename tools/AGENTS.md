# 开发脚本

## 构建

| 文件 | 用途 |
| --- | --- |
| `build.mjs` | 统一构建网页、桌面程序、shim 和诊断工具，生成构建记录。 |
| `build-evidence.mjs` | 收集源码、工具链与产物哈希，供构建脚本使用。 |
| `build-evidence.test.mjs` | 验证构建记录的生成逻辑。 |
| `release.mjs` | 构建双架构安装包，验签后发布；支持 `--platform`、`--verify-only`、`--publish`。 |
| `release.test.mjs` | 验证发布门禁、签名、包内容与草稿续传。 |

## Desktop 检查与联调

| 文件 | 用途 |
| --- | --- |
| `inspect-desktop.mjs` | 读取 Desktop 的 ASAR 包，检查版本、启动方式与事件实现。 |
| `start-desktop.mjs` | 通过 shim 启动 Desktop 联调实例，启用本机调试端口。 |
| `cdp.mjs` | 通过 CDP 查询控件、执行页面脚本、输入、截图和关闭 Desktop。 |
| `desktop-status.ps1` | 读取 Desktop 窗口与按钮状态，也可请求关闭窗口。 |
| `native-dialog.ps1` | 通过 UI Automation 检查原生对话框、填写文件夹并确认。 |
| `select-folder.ps1` | 通过键盘模拟，在已打开的文件夹对话框中选择目录。 |

联调脚本按需运行，操作实际的 Desktop 窗口和会话。

## 维护

新增脚本、改变用途或删除文件时，同步更新本文件及相关引用。临时脚本用完后及时清理，保留仍有复用价值的工具。
